import { Files } from './wasi/files.ts';
import { ProcExit, preview1 } from './wasi/preview1.ts';
import { NullSockets, socketAwareRw, sockets } from './wasi/sockets.ts';
import type { SocketBackend } from './wasi/sockets.ts';
import { AddressPool, CAP, READY, netV1 } from './wasi/netv1.ts';

/**
 * Assembles the 28 imports a raptormark module needs and runs it.
 *
 * Platform-agnostic on purpose: everything here works unchanged in Node and in
 * a browser. The pieces that differ -- where the bytes come from, where stdout
 * goes, and which socket transport is in play -- arrive as arguments.
 */

/**
 * Socket handles start here so they never collide with a file descriptor.
 *
 * They share one integer space because the WasmEdge ABI assumes it: ecvisor
 * closes a socket with libc `close(2)` (`sys.rs:2186`), which is only coherent
 * if a socket handle IS a WASI fd. So the split has to be by range, and a
 * backend must allocate from here up.
 */
export const SOCKET_HANDLE_BASE = 64;

export interface RunOptions {
  /** The linked module. */
  module: WebAssembly.Module;
  /** The RAPTORFS sidecar. Optional: ecvisor boots without one, falling back to
   *  host argv, but no real guest will find its files. */
  rootfs?: Uint8Array;
  /** Name the sidecar is published under. Must line up with what ecvisor looks
   *  for -- `$RAPTORMARK_ROOTFS`, else `out.rootfs.img` / `rootfs.img`. */
  rootfsName?: string;
  args?: string[];
  env?: Record<string, string>;
  socketBackend?: SocketBackend;
  /** Receives decoded stdout/stderr text as it arrives. */
  onOutput?: (fd: number, text: string) => void;
  /**
   * Drive the module through `ecv_boot`/`ecv_run_slice` instead of `_start`.
   *
   * This is the shape a browser needs: the guest never blocks the host, it
   * returns and says when to come back. Requires a module linked with the
   * re-entrant surface AND a backend that declines to wait -- otherwise the
   * scheduler simply blocks inside a slice and IDLE never occurs.
   */
  reentrant?: boolean;
  /** Legs per slice. 0 means unbounded. */
  legsPerSlice?: number;
  /**
   * A readiness queue shared with the transport.
   *
   * A transport with real event handlers (a WebSocket) pushes into this the
   * moment something happens, and the driver flushes it between slices -- which
   * is both lower latency and less work than polling. A transport that can only
   * be probed leaves it empty and the driver polls instead.
   *
   * ⚠️ It must NOT be drained by the transport itself. Delivering readiness
   * means calling `ecv_net_ready`, and `ecv_run_slice` is on the stack during
   * every import and every event callback that a slice provoked.
   */
  readyQueue?: Array<[number, number]>;
  /** Signals to post to guest processes, drained between slices. */
  signalQueue?: Array<[number, number]>;
  /**
   * Supply `raptormark_net_v1` as well as the WASI socket names.
   *
   * Harmless when the module does not import it -- an unused entry in the
   * import object is ignored -- so this is not a mode the caller has to get
   * right, just a capability to offer.
   */
  netV1?: boolean;
  /**
   * Called each time the driver finds the guest IDLE, before it decides how
   * long to wait.
   *
   * Safe to call anything from here: `ecv_run_slice` has already returned, so
   * this is not inside a host import and the reentrancy rule does not bite.
   *
   * It exists because "the guest is now parked" is not otherwise observable
   * from outside the driver, and a host wants it -- to park a worker, to update
   * a UI, or (as the clock-step test does) to perturb the world at the one
   * moment a guest has an armed deadline and nothing else to do.
   */
  onIdle?: () => void;
  /**
   * Milliseconds to add to the REALTIME clock, sampled on every read.
   *
   * ⚠️ A TEST INSTRUMENT, not a configuration knob. Guest deadlines are
   * monotonic and must be unmoved by a wall-clock step; nothing in the tree can
   * step a real host clock, so the shim offers a fake one. Leave it unset and
   * the clock is the host's, unmodified.
   */
  realtimeOffsetMs?: () => number;
}

/** Status codes returned by `ecv_run_slice`. Mirrors `runtime/src/entry.rs`. */
export const ECV = { IDLE: 0, PREEMPTED: 1, EXITED: 2 } as const;

/**
 * The re-entrant surface a module exports when linked with a non-default
 * profile. Absent from the shipping build, where nothing drives it.
 */
export interface Reentrant {
  ecv_boot(): number;
  /** Posts a signal to a guest process. Browser profile only. */
  ecv_signal?(pid: number, sig: number): number;
  ecv_run_slice(maxLegs: number): number;
  /**
   * Milliseconds until the soonest guest deadline, or -1 for none.
   *
   * ⚠️ A DURATION. Its predecessor `ecv_next_deadline_ms` returned an absolute
   * unix instant, because guest deadlines were wall-clock; they are monotonic
   * now and there is no meaningful absolute form to report. The rename is what
   * turns a host that still subtracts `Date.now()` into an instantiation
   * failure instead of a spin.
   */
  ecv_next_deadline_in_ms(): number;
  ecv_exit_code(): number;
  /** Browser profile only: push readiness into the runtime's cache. */
  ecv_net_ready?(handle: number, events: number): void;
}

export function reentrantSurface(instance: WebAssembly.Instance): Reentrant | undefined {
  const e = instance.exports as Record<string, unknown>;
  const names = ['ecv_boot', 'ecv_run_slice', 'ecv_next_deadline_in_ms', 'ecv_exit_code'];
  if (!names.every((n) => typeof e[n] === 'function')) return undefined;
  return e as unknown as Reentrant;
}

export interface RunResult {
  exitCode: number;
  stdout: string;
  stderr: string;
  /** WasmEdge socket names the guest reached but the backend did not implement. */
  unimplemented: string[];
  /** How many slices the re-entrant driver ran, or 0 when `_start` was used. */
  slices?: number;
  /** How many times the guest went idle with a future deadline. */
  idleWaits?: number;
}

export async function run(opts: RunOptions): Promise<RunResult> {
  const files = new Files('/');
  if (opts.rootfs) files.add(opts.rootfsName ?? 'rootfs.img', opts.rootfs);

  // The instance does not exist yet, but the import closures need its memory.
  // A late-bound getter is the standard knot-tying here -- and it is also the
  // ONLY correct shape, because `memory.buffer` must be re-read per call
  // anyway (see wasi/mem.ts). Capturing the memory object is fine; capturing
  // its buffer is the bug.
  let instance: WebAssembly.Instance | undefined;
  const memory = (): WebAssembly.Memory => {
    const m = instance?.exports['memory'];
    if (!(m instanceof WebAssembly.Memory)) {
      throw new Error('module does not export `memory`; not a raptormark artifact');
    }
    return m;
  };

  // stdout and stderr are decoded with STREAMING decoders. ecvisor writes
  // whenever it has bytes, not on character boundaries, so a fresh decoder per
  // call mangles any multi-byte sequence that lands across a write.
  const decoders = new Map<number, TextDecoder>();
  const captured = new Map<number, string>();
  files.onWrite = (fd, bytes) => {
    let d = decoders.get(fd);
    if (!d) {
      d = new TextDecoder('utf-8', { fatal: false });
      decoders.set(fd, d);
    }
    const text = d.decode(bytes, { stream: true });
    if (!text) return;
    captured.set(fd, (captured.get(fd) ?? '') + text);
    opts.onOutput?.(fd, text);
  };

  const backend = opts.socketBackend ?? new NullSockets();
  const sock = sockets(memory, backend);
  const p1 = preview1({
    memory,
    files,
    args: opts.args,
    env: opts.env,
    fdPoll: sock.fdPoll,
    realtimeOffsetMs: opts.realtimeOffsetMs,
  });

  const isSocket = (fd: number) => fd >= SOCKET_HANDLE_BASE;
  const rw = socketAwareRw(
    memory,
    backend,
    isSocket,
    p1['fd_read'] as (a: number, b: number, c: number, d: number) => number,
    p1['fd_write'] as (a: number, b: number, c: number, d: number) => number,
  );

  // Readiness the host has observed but not yet delivered. It is QUEUED rather
  // than pushed immediately because `ecv_net_ready` must not be called from
  // inside an import: `ecv_run_slice` is on the stack there, and re-entering is
  // reentrancy into a `&mut`-borrowed context. The driver flushes between slices.
  const pendingReady: Array<[number, number]> = opts.readyQueue ?? [];
  const pool = new AddressPool();
  const netImports = opts.netV1
    ? netV1({
        memory,
        backend,
        capabilities: CAP.RELAY | CAP.DATAGRAM,
        notify: (h, ev) => pendingReady.push([h, ev]),
        pool,
      })
    : {};

  const wasi = { ...p1, ...sock.imports, ...rw };
  // `fd_close` has to route by handle too, for the same reason.
  const fileClose = wasi['fd_close'] as (fd: number) => number;
  wasi['fd_close'] = (fd: number) => (isSocket(fd) ? backend.close(fd) : fileClose(fd));

  instance = await WebAssembly.instantiate(opts.module, {
    wasi_snapshot_preview1: wasi,
    raptormark_net_v1: netImports,
  });

  let exitCode = 0;
  let slices = 0;
  let idleWaits = 0;
  try {
    if (opts.reentrant) {
      const r = reentrantSurface(instance);
      if (!r) {
        throw new Error(
          'module does not export the re-entrant surface; link it with ' +
            '`link-all --profile loopback` (the shipping profile omits it)',
        );
      }
      const d = await drive(
        r,
        backend,
        pendingReady,
        opts.legsPerSlice ?? 0,
        opts.signalQueue,
        opts.onIdle,
      );
      exitCode = d.exitCode;
      slices = d.slices;
      idleWaits = d.idleWaits;
    } else {
      const start = instance.exports['_start'];
      if (typeof start !== 'function') {
        throw new Error('module does not export `_start`; not a raptormark command module');
      }
      (start as () => void)();
      // Falling out of `_start` without a proc_exit is not something wasi-libc
      // does -- it calls exit() after main returns -- so treat it as a clean 0
      // rather than inventing a failure.
    }
  } catch (err) {
    if (!(err instanceof ProcExit)) throw err;
    exitCode = err.code;
  }

  // Flush the streaming decoders: a trailing partial sequence is only emitted
  // once the stream is declared finished.
  for (const [fd, d] of decoders) {
    const tail = d.decode();
    if (tail) captured.set(fd, (captured.get(fd) ?? '') + tail);
  }

  return {
    exitCode,
    stdout: captured.get(1) ?? '',
    stderr: captured.get(2) ?? '',
    unimplemented: backend instanceof NullSockets ? [...backend.reached].sort() : [],
    slices,
    idleWaits,
  };
}

/**
 * The re-entrant driver loop.
 *
 * Extracted from `run` so it can be tested against a fake surface: the
 * invariant that matters most here -- that the guest is never re-entered
 * without the event loop turning -- cannot be observed from outside, and is
 * silent when broken until a transport needs a callback that never arrives.
 */
export async function drive(
  r: Reentrant,
  backend: SocketBackend,
  pendingReady: Array<[number, number]>,
  legs: number,
  pendingSignals: Array<[number, number]> = [],
  onIdle?: () => void,
): Promise<{ exitCode: number; slices: number; idleWaits: number }> {
  let exitCode = 0;
  let slices = 0;
  let idleWaits = 0;
  try {
    const rc = r.ecv_boot();
    if (rc !== 0) throw new Error(`ecv_boot failed: ${rc}`);
    // Readiness reported at the previous idle, to tell a changed condition
    // from the same one re-reported. Cleared whenever the guest runs.
    let lastReady = '';
    const flush = () => {
      if (!r.ecv_net_ready) {
        pendingReady.length = 0;
        return;
      }
      for (const [h, ev] of pendingReady) r.ecv_net_ready(h, ev);
      pendingReady.length = 0;
    };
    // ⚠️ QUEUED, NOT CALLED DIRECTLY. `ecv_signal` mutates the process table,
    // which a slice holds `&mut` -- so a page calling it mid-slice would be
    // re-entering a borrowed context. Draining here, between slices, is the same
    // discipline the readiness queue follows and for the same reason.
    const flushSignals = () => {
      if (!r.ecv_signal) {
        pendingSignals.length = 0;
        return;
      }
      for (const [pid, sig] of pendingSignals) r.ecv_signal(pid, sig);
      pendingSignals.length = 0;
    };
    for (;;) {
      flush();
      flushSignals();
      slices++;
      const status = r.ecv_run_slice(legs);
      if (status !== ECV.IDLE) lastReady = '';
      if (status === ECV.EXITED) {
        exitCode = r.ecv_exit_code();
        break;
      }
      if (status === ECV.IDLE) {
        idleWaits++;
        onIdle?.();
        // ⚠️ A DELAY in milliseconds, not an instant, and NOT on `Date.now()`'s
        // clock: guest deadlines are monotonic, counted from ecvisor's boot.
        // The runtime measures it when asked, so it already accounts for
        // anything this loop did between the slice returning and here.
        const delayMs = r.ecv_next_deadline_in_ms();
        // Idle with no deadline means I/O is the ONLY wake source. The host
        // does not know which handle the guest is parked on -- that lives in
        // the runtime's process table -- so it polls everything the transport
        // owns and notifies liberally.
        //
        // ⚠️ Over-notifying is safe and under-notifying hangs: a woken process
        // re-checks its condition and re-parks, costing one scheduler pass,
        // whereas a missed notification costs the run.
        const polled = pollTransport(backend, pendingReady);
        // ⚠️ NEVER RE-ENTER THE GUEST WITHOUT TURNING THE EVENT LOOP.
        //
        // An open stream is PERMANENTLY writable, so `pollTransport` reports a
        // hit on every pass for as long as one socket is connected. A bare
        // `continue` here therefore span synchronously and forever -- and the
        // event loop is exactly what delivers the WebSocket messages the guest
        // is idle waiting for, so the wake-up could never arrive. The tab
        // froze hard, and only once the relay CONNECTED, which made it look
        // like a relay bug rather than a driver bug.
        //
        // Readiness that has not changed since the last idle is not progress:
        // the guest saw it and parked anyway. Re-notifying it is churn, so
        // that case falls through to the timed wait below instead.
        const sig = pendingReady.map(([h, ev]) => `${h}:${ev}`).join(',');
        if (polled && sig !== lastReady) {
          lastReady = sig;
          await macrotask();
          continue;
        }
        if (delayMs < 0) {
          if (!(backend as SocketBackend).liveHandles) {
            // No deadline, no transport that can report readiness: nothing
            // will ever wake this guest, and waiting would hang with no
            // diagnosis.
            throw new Error('guest is idle with no deadline and no transport to wake it');
          }
          // A transport exists but nothing is ready yet. Yield briefly rather
          // than spin; a browser would park on a real callback here.
          await sleep(IDLE_POLL_MS);
          continue;
        }
        const delay = Math.max(0, delayMs);
        // ⚠️ Cap the sleep ONLY when there is a transport to poll. A deadline
        // with no I/O has exactly one wake source -- the clock -- so waiting
        // the whole delay is both correct and one timer instead of many.
        // Capping unconditionally turned a single 400 ms guest sleep into 76
        // wakeups, which is not a correctness bug but is pure churn, and in a
        // browser it is 76 timer callbacks a profile will notice.
        const canPoll = (backend as SocketBackend).liveHandles !== undefined;
        await sleep(canPoll ? Math.min(delay, IDLE_POLL_MS) : delay);
      }
      if (status === ECV.PREEMPTED) {
        // ⚠️ YIELD, do not loop straight back in. The budget ran out
        // mid-flight and the guest is fine, but a tight loop here never
        // returns to the event loop -- which is precisely what the host needs
        // in order to deliver the socket messages and fetch resolutions the
        // guest is waiting for. Looping is how a re-entrant driver reproduces
        // the blocking behaviour it exists to avoid.
        await macrotask();
      }
    }
  } catch (err) {
    // ⚠️ CATCH IT HERE, not in the caller. A guest usually leaves through
    // `proc_exit`, which throws -- and an exception unwinding past this
    // function would carry the slice and idle counts away with it, leaving the
    // caller to report a run that "never went idle". The counts are the only
    // evidence that the driver yielded at all, so losing them turns a working
    // run into a failing assertion with no visible cause.
    if (!(err instanceof ProcExit)) throw err;
    exitCode = err.code;
  }
  return { exitCode, slices, idleWaits };
}

/**
 * Yields to the event loop for `ms`.
 *
 * ⚠️ A REAL await, not a spin. The whole point of the re-entrant driver is that
 * the host stays responsive while the guest is idle -- a busy-wait here would
 * burn a core and, in a browser, block the very event loop that delivers the
 * readiness the guest is waiting for.
 */
function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * Yields to the event loop once.
 *
 * ⚠️ `MessageChannel`, NOT `queueMicrotask`. A microtask does not yield to the
 * event loop at all: it runs before the loop gets control back, so a driver that
 * "yielded" with one would still starve every WebSocket `message` and every
 * `fetch` resolution behind it -- the guest would wait forever for readiness the
 * host had already received and could not deliver.
 *
 * `setTimeout(0)` also yields, but is clamped (>= 4 ms once nested, >= 1 s in a
 * background tab), so it is the fallback rather than the default.
 */
function macrotask(): Promise<void> {
  if (typeof MessageChannel !== 'undefined') {
    return new Promise((resolve) => {
      const c = new MessageChannel();
      c.port1.onmessage = () => {
        c.port1.close();
        resolve();
      };
      c.port2.postMessage(0);
    });
  }
  return new Promise((resolve) => setTimeout(resolve, 0));
}

/**
 * How long the driver yields when the guest is idle on I/O.
 *
 * A POLL, not a callback, because `SocketBackend` is a synchronous probe API --
 * which is what the WasmEdge ABI needs. A browser transport pushes readiness
 * from its own event handlers instead and never reaches this path; the constant
 * bounds the latency for hosts that cannot.
 */
const IDLE_POLL_MS = 5;

/**
 * Asks the transport which of its handles are ready and queues notifications.
 * Returns true if anything was queued.
 */
function pollTransport(b: SocketBackend, out: Array<[number, number]>): boolean {
  const live = b.liveHandles?.();
  if (!live || live.length === 0) return false;
  const subs = live.flatMap((fd) => [
    { fd, write: false },
    { fd, write: true },
  ]);
  const hits = b.wait(subs, 0);
  if (hits.length === 0) return false;
  for (const i of hits) {
    const s = subs[i];
    if (!s) continue;
    out.push([s.fd, s.write ? READY.WRITE : READY.READ]);
  }
  return true;
}
