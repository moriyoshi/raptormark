/**
 * The Node reference host for raptormark modules.
 *
 * Runs a linked artifact against a hand-rolled WASI preview1 shim plus the
 * WasmEdge socket extension. It exists for three reasons, in order:
 *
 *   1. It is the EXECUTABLE SPECIFICATION of the 28 imports a host must supply.
 *      `runtime/src/sys.rs:520` and `:568` both point at
 *      `third_party/wazero/.../sock_wasmedge.go` as the reference host; that
 *      path does not exist in this tree, so until now the ABI was specified
 *      only by the Rust `extern` blocks that consume it.
 *   2. It runs the real module with neither Docker nor a browser, which makes a
 *      runtime change verifiable in seconds instead of a 20-minute E2E suite.
 *   3. It is the shared core the browser embedder is built on.
 *
 * ⚠️ NOT a deployment target. `node:wasi` is deliberately unused -- it aborts
 * the process on the first call after `memory.grow`, which every raptormark
 * guest triggers (see web/src/wasi/mem.ts).
 *
 * Usage:
 *   node bin/run.ts --module out.wasm [--rootfs rootfs.img] [--env K=V]... [-- args]
 *
 * `--clock-step-ms N` jumps the wall clock by N ms once the guest first goes
 * idle, which is how the clock-step regression test perturbs a running guest.
 *
 * `--stamp PREFIX` reports the host time at which the guest printed each
 * `PREFIX <NAME>` line, which is how a benchmark gets timed by a clock it is
 * not measuring.
 */

import { readFile } from 'node:fs/promises';
import { basename } from 'node:path';
import process from 'node:process';
import { SOCKET_HANDLE_BASE, run } from '../src/host.ts';
import { NodeSockets } from '../src/node/sockets.ts';
import { outputStamper } from '../src/stamp.ts';

type Args = {
  module?: string;
  rootfs?: string;
  env: Record<string, string>;
  guestArgs: string[];
  quiet: boolean;
  noNet: boolean;
  reentrant: boolean;
  legs: number;
  netV1: boolean;
  clockStepMs: number;
  stamp?: string;
};

function parseArgs(argv: string[]): Args {
  const out: Args = {
    env: {},
    guestArgs: [],
    quiet: false,
    noNet: false,
    reentrant: false,
    legs: 0,
    netV1: false,
    clockStepMs: 0,
  };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i]!;
    if (a === '--') {
      out.guestArgs = argv.slice(i + 1);
      break;
    } else if (a === '--module') out.module = argv[++i];
    else if (a === '--rootfs') out.rootfs = argv[++i];
    else if (a === '--quiet') out.quiet = true;
    // Real sockets are the default. `--no-net` swaps in the stub backend, which
    // reports ENOTSUP and NAMES every call a guest reached -- useful for finding
    // out what a guest actually wants from the network.
    else if (a === '--no-net') out.noNet = true;
    // Drive `ecv_boot`/`ecv_run_slice` instead of `_start`: the shape a browser
    // needs, where the guest hands control back rather than blocking the host.
    else if (a === '--reentrant') out.reentrant = true;
    else if (a === '--legs') out.legs = Number(argv[++i]) || 0;
    // Offer `raptormark_net_v1`, which the browser profile imports instead of
    // WasmEdge's socket extension. Harmless for a module that does not.
    else if (a === '--net-v1') out.netV1 = true;
    // Step the wall clock by N ms the first time the guest goes IDLE, i.e. at
    // the one moment it has an armed deadline and nothing else to do. See
    // `clockStepMs` below for why that instant and not a timer.
    else if (a === '--clock-step-ms') out.clockStepMs = Number(argv[++i]) || 0;
    // Timestamp, on the HOST clock, every `PREFIX <NAME>` line the guest
    // prints. See `src/stamp.ts`: a guest cannot time itself with the clock
    // under test.
    // ⚠️ A MISSING PREFIX MUST NOT BE SILENT. `--stamp` with nothing after it
    // would leave the flag unset, the run would look normal, and the stamps
    // would simply be absent -- read at the far end as "the guest never printed
    // the markers".
    else if (a === '--stamp') out.stamp = argv[++i] || die('--stamp needs a prefix');
    else if (a === '--env') {
      const [k, ...v] = (argv[++i] ?? '').split('=');
      if (k) out.env[k] = v.join('=');
    } else die(`unexpected argument: ${a}`);
  }
  if (!out.module) die('--module is required');
  return out;
}

function die(msg: string): never {
  console.error(`HOST-FAILED: ${msg}`);
  process.exit(2);
}

const args = parseArgs(process.argv.slice(2));

const moduleBytes = await readFile(args.module!);
const rootfs = args.rootfs ? await readFile(args.rootfs) : undefined;

// `RAPTORMARK_ROOTFS` is read by `load_sidecar` (`runtime/src/entry.rs:322`)
// before anything else. Setting it explicitly means the guest never has to fall
// through the `out.rootfs.img` / `rootfs.img` / `/out.rootfs.img` candidates,
// so the sidecar is found under whatever name it actually has on disk.
const env = { ...args.env };
if (args.rootfs && !env['RAPTORMARK_ROOTFS']) {
  env['RAPTORMARK_ROOTFS'] = '/' + basename(args.rootfs);
}

// `new Uint8Array(buf)` rather than the Buffer directly: node's Buffer is
// backed by a pooled ArrayBufferLike that may be a SharedArrayBuffer, which is
// not a BufferSource. It happens to work at run time and fails to typecheck,
// which is the right way round to find out.
const module = await WebAssembly.compile(new Uint8Array(moduleBytes));

const sockets = args.noNet ? undefined : new NodeSockets(SOCKET_HANDLE_BASE);

// `--clock-step-ms`: jump the wall clock once, WHILE the guest is parked.
//
// ⚠️ THE TRIGGER IS THE FIRST IDLE, NOT A TIMER, and that is what makes the
// test decisive rather than flaky. The deadline has to be ARMED before the
// clock moves: step it earlier and a wall-clock deadline is simply computed
// from the stepped clock and the sleep behaves correctly for the wrong reason.
// A timer cannot hit that window, because how long boot takes depends on the
// module -- instantiating a 40 MB code section is not a fixed cost.
//
// A step of one hour is not a plausible NTP correction; it is chosen to be far
// outside every tolerance in the test, so a failure reports a number nobody has
// to interpret.
let steppedAt = 0;
const realtimeOffsetMs = args.clockStepMs ? () => (steppedAt ? args.clockStepMs : 0) : undefined;
const onIdle = args.clockStepMs
  ? () => {
      if (steppedAt) return;
      steppedAt = performance.now();
      console.error(`HOST-CLOCK-STEP: ${args.clockStepMs} ms`);
    }
  : undefined;

// `--stamp`: the host reads its own clock when a marker line arrives.
//
// ⚠️ It has to run BEFORE the bytes are forwarded to a pipe, and it has to be
// installed even under `--quiet`: the stamp is a measurement of when the guest
// got there, and writing to stdout first would fold the cost of that write into
// the interval being measured.
const stamper = args.stamp
  ? outputStamper({
      prefix: args.stamp,
      // `performance.now()` is monotonic from process start and is not the
      // clock `--clock-step-ms` moves, which is the whole point: it is the one
      // reading in the run that neither the guest nor a test instrument can
      // touch. Microseconds, because the consumer divides by an iteration count.
      nowUs: () => performance.now() * 1000,
      emit: (line) => console.error(line),
    })
  : undefined;
const echo = args.quiet
  ? undefined
  : (fd: number, text: string) => (fd === 2 ? process.stderr : process.stdout).write(text);
const onOutput =
  stamper && echo
    ? (fd: number, text: string) => {
        stamper(fd, text);
        echo(fd, text);
      }
    : (stamper ?? echo);

const result = await run({
  module,
  rootfs: rootfs ? new Uint8Array(rootfs) : undefined,
  rootfsName: args.rootfs ? basename(args.rootfs) : undefined,
  args: args.guestArgs.length > 0 ? args.guestArgs : undefined,
  env,
  socketBackend: sockets,
  reentrant: args.reentrant,
  legsPerSlice: args.legs,
  netV1: args.netV1,
  onIdle,
  realtimeOffsetMs,
  // Stream through as it arrives rather than only at the end: a guest that
  // hangs has usually already said why, and buffering hides exactly that.
  onOutput,
});

await sockets?.stop();

if (result.unimplemented.length > 0) {
  console.error(
    `HOST-NOTE: guest reached ${result.unimplemented.length} unimplemented socket call(s): ` +
      result.unimplemented.join(' '),
  );
}
if (args.reentrant) {
  console.error(`HOST-SLICES: ${result.slices} idle=${result.idleWaits}`);
}
if (steppedAt) {
  // Real elapsed host time between moving the clock and the guest finishing.
  // ⚠️ This is the only measure in the run that the guest cannot influence and
  // the step cannot move, which is what makes it able to say whether a parked
  // guest STAYED parked. Every clock the guest reads is one of the two under
  // test.
  console.error(`HOST-AFTER-STEP-MS: ${Math.round(performance.now() - steppedAt)}`);
}
console.error(`HOST-EXIT: ${result.exitCode}`);
process.exit(result.exitCode);
