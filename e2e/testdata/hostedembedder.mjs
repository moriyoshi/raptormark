// A host that serves a side-module load MID-RUN, in response to the guest's
// own dlopen.
//
// This is what `e2e/testdata/embedder.mjs` is not. That harness places every
// side module BEFORE `_start`, which is the deferred-instantiation shape: the
// set is fixed at link time and the guest never influences it. Here the guest
// runs first, calls `dlopen`, and the module it needs is compiled and
// instantiated in response -- which is the only shape that can work in a
// browser, where a 36 MB unit cannot be compiled synchronously on the main
// thread (Chromium's ceiling is 8 MB).
//
// # The loop, and why it cannot be `_start`
//
// `_start` returns only when the guest is finished, so a host driving it has no
// window in which to instantiate anything. The re-entrant surface exists for
// exactly this:
//
//   ecv_boot()                     once
//   ecv_run_slice(n)   -> 1        the guest made progress; go again
//                      -> 0        nothing runnable; a parked dlopen looks
//                                  EXACTLY like this, so serve the queue
//                      -> 2        exited; read ecv_exit_code
//
// ⚠️ ECV_IDLE IS 0 AND ECV_PREEMPTED IS 1, which reads backwards if you expect
// zero to mean success. A loop written as `if (r !== 0) break` stops on the
// first slice that made progress and runs the guest to completion only when it
// exits by another route. That bug was live in
// `.agents-workspace/drivers/hostedrun/hostedrun.mjs` and passed, because the
// guest there exited through `proc_exit` before the loop mattered.
//
// # ⚠️ Why loads are QUEUED rather than served inline
//
// `ecv_host_load_side` is called from inside `ecv_run_slice`, so the slice is on
// the wasm stack. `ecv_side_loaded` mutates the process table, which the running
// slice holds `&mut`. Calling it from inside the import would alias that borrow.
// So the import records the request and returns 0 (PENDING), and the queue is
// flushed only after the slice returns -- the same rule `ecv_net_ready` and
// `ecv_signal` document.
//
//   node hostedembedder.mjs --supervisor S.wasm --program-size N --dir D \
//        --main <path> --unit <hash>:<index>:<path> [--unit ...]
import { readFileSync } from 'node:fs';
import { WASI } from 'node:wasi';

function fail(msg) {
  console.log(`HOSTEDEMBEDDER-FAILED: ${msg}`);
  process.exit(1);
}

// --- arguments ------------------------------------------------------------
const argv = process.argv.slice(2);
const opts = { supervisor: null, programSize: null, dir: null, main: null, units: [], env: {} };
for (let i = 0; i < argv.length; i += 2) {
  const k = argv[i], v = argv[i + 1];
  if (k === '--supervisor') opts.supervisor = v;
  else if (k === '--program-size') opts.programSize = Number(v);
  else if (k === '--dir') opts.dir = v;
  else if (k === '--main') opts.main = v;
  else if (k === '--env') {
    // ❗ REQUIRED to reach the rfs sidecar. The dlopen map lives in it, and
    // ecvisor finds it through RAPTORMARK_ROOTFS. Without this the runtime
    // reports the rootfs "set but unreadable" (or never looks), runs with NO
    // dlopen map, and every dlopen fails with "cannot open shared object file"
    // -- which looks exactly like the loader being broken.
    const eq = v.indexOf('=');
    if (eq < 0) fail(`--env wants K=V, got ${v}`);
    opts.env[v.slice(0, eq)] = v.slice(eq + 1);
  }
  else if (k === '--unit') {
    // hash:index:path -- the hash is what the runtime passes to
    // ecv_host_load_side (a unit's content name), the index is which
    // `ecv_program_<i>` global the side module exports.
    const m = /^([^:]+):(\d+):(.+)$/.exec(v);
    if (!m) fail(`--unit wants <hash>:<index>:<path>, got ${v}`);
    opts.units.push({ hash: m[1], index: Number(m[2]), path: m[3] });
  } else fail(`unknown flag ${k}`);
}
if (!opts.supervisor || !opts.main || !opts.programSize) {
  fail('usage: --supervisor S --program-size N --main M [--unit h:i:p ...]');
}

function dylinkMemInfo(bytes) {
  let p = 8; // magic + version
  const leb = () => {
    let result = 0, shift = 0, b;
    do { b = bytes[p++]; result |= (b & 0x7f) << shift; shift += 7; } while (b & 0x80);
    return result >>> 0;
  };
  while (p < bytes.length) {
    const id = bytes[p++];
    const size = leb();
    const end = p + size;
    if (id === 0) {
      const nameLen = leb();
      const name = Buffer.from(bytes.subarray(p, p + nameLen)).toString();
      p += nameLen;
      if (name === 'dylink.0') {
        while (p < end) {
          const sub = bytes[p++];
          const subSize = leb();
          const subEnd = p + subSize;
          if (sub === 1) {
            return { memSize: leb(), memAlignLog2: leb(), tableSize: leb(), tableAlignLog2: leb() };
          }
          p = subEnd;
        }
        return null;
      }
    }
    p = end;
  }
  return null;
}

// --- the host surface -----------------------------------------------------
const wasi = new WASI({
  version: 'preview1',
  args: ['guest'],
  env: opts.env,
  preopens: opts.dir ? { '/': opts.dir } : {},
  returnOnExit: true,
});

let S = null;                 // the supervisor's exports
const exit = { called: false, code: 0 };
const queue = [];             // loads asked for during the current slice
const served = [];            // {hash, token, ok} after the sequence ran
const placedUnits = new Map(); // hash -> token, to refuse a DOUBLE placement

const env = {
  // THE LOADER IMPORT. Returns 0 = PENDING: this host will finish the load
  // between slices and call `ecv_side_loaded(token)`.
  //
  // `token` is opaque here and must be echoed back exactly. The runtime picks
  // it: for a unit that has no registry index yet -- which is every unit on its
  // first dlopen, because the descriptor lives in the side module -- it is a
  // synthetic value above any real index.
  ecv_host_load_side: (token, namePtr, nameLen) => {
    const name = new TextDecoder().decode(new Uint8Array(S.memory.buffer, namePtr, nameLen))
      .replace(/\0+$/, '');
    // ❗ REFUSE A SECOND PLACEMENT OF THE SAME UNIT, loudly.
    //
    // A unit is asked for by two different names: a synthetic TOKEN on the first
    // dlopen (no registry index exists yet) and its real registry INDEX on the
    // retry after the wake. If the runtime does not reconcile them, the backend
    // has no record under the second name and asks again -- and this host would
    // cheerfully reserve a second arena region and a second block of table slots
    // for a module already live. That succeeds, reports nothing, and the guest
    // traps later somewhere unrelated. Measured exactly once, on 2026-08-24.
    if (placedUnits.has(name)) {
      fail(`asked to place ${name} a SECOND time (first as token ` +
           `${placedUnits.get(name)}, now as ${token}). The unit is already ` +
           'registered and live; placing it again would duplicate its arena ' +
           'region and table slots. The runtime is not reconciling the pending ' +
           'token with the registry index it got after registration.');
    }
    // The neutralization hook. See hostedload_test.go's `mounts` block: with
    // this set the host refuses every load, which must make the guest's dlopen
    // return NULL with a real dlerror rather than quietly succeeding.
    if (process.env.NEUTRALIZE_REFUSE_ALL) {
      console.log(`load: token=${token} REFUSED (neutralization)`);
      return -1;
    }
    const unit = opts.units.find((u) => u.hash === name);
    if (!unit) {
      console.log(`load: token=${token} name=${name} -- NOT OFFERED to this host`);
      return -1; // a refusal, which the guest sees through dlerror
    }
    console.log(`load: token=${token} name=${name} -> queued ${unit.path}`);
    placedUnits.set(name, token);
    queue.push({ token, unit });
    return 0; // PENDING
  },
};

const imports = wasi.getImportObject();
const p1 = imports.wasi_snapshot_preview1;
const kSetMemory = Object.getOwnPropertySymbols(wasi).find((s) => String(s) === 'Symbol(kSetMemory)');
if (!kSetMemory) fail('node:wasi no longer exposes kSetMemory');
let lastBuffer = null;
for (const name of Object.keys(p1)) {
  const orig = p1[name];
  p1[name] = (...args) => {
    if (S && S.memory.buffer !== lastBuffer) { lastBuffer = S.memory.buffer; wasi[kSetMemory](S.memory); }
    return orig(...args);
  };
}
// WasmEdge's socket names, stubbed: the loopback backend under `hosted` does not
// import them, but a guest that probes gets an errno rather than a trap.
const ENOTSUP = 58;
for (const n of ['sock_bind', 'sock_connect', 'sock_getlocaladdr', 'sock_getpeeraddr',
                 'sock_getsockopt', 'sock_listen', 'sock_open', 'sock_recv_from',
                 'sock_send_to', 'sock_setsockopt']) {
  if (p1[n] === undefined) p1[n] = () => ENOTSUP;
}
const origExit = p1.proc_exit;
p1.proc_exit = (code) => { exit.code = code; exit.called = true; return origExit(code); };

// --- instantiate the supervisor -------------------------------------------
const supModule = new WebAssembly.Module(readFileSync(opts.supervisor));
const supplied = { env, wasi_snapshot_preview1: p1 };
const missingSup = WebAssembly.Module.imports(supModule)
  .filter((im) => !supplied[im.module] || supplied[im.module][im.name] === undefined)
  .map((im) => `${im.module}.${im.name}`);
if (missingSup.length) fail(`the supervisor imports what this host does not supply: ${missingSup.join(', ')}`);

let supInstance = new WebAssembly.Instance(supModule, supplied);
S = supInstance.exports;
wasi.initialize({ exports: { memory: S.memory } });

// ❗ THE SUPERVISOR MUST HAVE BEEN LINKED FOR THIS, and the failure is otherwise
// a hang rather than a message: without `ecv_side_loaded` nothing can wake a
// parked dlopen, and without the loader archive nothing would have asked.
for (const need of ['ecv_boot', 'ecv_run_slice', 'ecv_side_loaded', 'ecv_exit_code',
                    'ecv_reserve_side', 'ecv_register_program', '__indirect_function_table']) {
  if (S[need] === undefined) {
    fail(`the supervisor does not export ${need}. Was it linked with ` +
         '`--side-out --profile hosted`? The supervisor link ignored --profile ' +
         'entirely before 2026-08-24, which produced exactly this.');
  }
}
if (!WebAssembly.Module.imports(supModule).some((im) => im.module === 'env' && im.name === 'ecv_host_load_side')) {
  fail('the supervisor does not import env.ecv_host_load_side, so it carries no ' +
       'hosted loader backend and will never ask this host for anything. It was ' +
       'linked against the wrong ecvisor archive.');
}
console.log(`supervisor: ${Object.keys(S).length} exports, loader import present`);

// --- the MULTIMODULE.md section-8 sequence, for one side module -----------
function place(path, index, label) {
  const bytes = readFileSync(path);
  const need = dylinkMemInfo(bytes);
  if (!need) fail(`${path} has no dylink.0 MEM_INFO; it was linked flat, not as a side module`);

  const align = 1 << need.memAlignLog2;   // dylink.0 stores the LOG2
  const memBase = S.ecv_reserve_side(BigInt(need.memSize), BigInt(align));
  if (memBase === 0) fail(`ecv_reserve_side(${need.memSize}, ${align}) returned 0`);

  const tableBase = S.__indirect_function_table.length;
  try {
    S.__indirect_function_table.grow(need.tableSize);
  } catch (err) {
    fail(`growing the table by ${need.tableSize} failed (${err}); the supervisor ` +
         'was probably linked without -Wl,--growable-table');
  }

  const senv = {
    memory: S.memory,
    __indirect_function_table: S.__indirect_function_table,
    __stack_pointer: S.__stack_pointer,
    __memory_base: new WebAssembly.Global({ value: 'i32', mutable: false }, memBase),
    __table_base: new WebAssembly.Global({ value: 'i32', mutable: false }, tableBase),
  };
  for (const name of Object.keys(S)) if (senv[name] === undefined) senv[name] = S[name];

  const sideModule = new WebAssembly.Module(bytes);

  // ❗ GOT.mem, WHICH ONLY A LIBRARY UNIT HAS.
  //
  // Phase 0b measured "no GOT.mem / GOT.func" and that was true -- of a MAIN
  // program's side module. A library unit is different: it has no entry
  // function, so elflift emits no `_ecv_entry_func`, and the descriptor
  // fragment's `.entry_func = &_ecv_entry_func` leaves an UNDEFINED data
  // symbol. Measured with llvm-nm on the objects:
  //
  //   unit_a: U _ecv_entry_func           (undefined)
  //   main:   D _ecvmain_..._entry_func   (defined, namespaced)
  //
  // The flat link resolves that undefined symbol to 0 through
  // `--allow-undefined`; a PIC side link turns it into a `GOT.mem` import
  // instead. Supplying 0 here is the SAME resolution, not a workaround -- and
  // 0 is the correct value: a library has no entry and nothing ever calls it.
  //
  // ⚠️ NARROW ON PURPOSE. Supplying every GOT.mem import as 0 would silence a
  // genuinely missing symbol, which is how a module gets a null pointer at a
  // random later moment instead of a link error here.
  const NULLABLE_GOT_MEM = new Set(['_ecv_entry_func']);
  const got = { 'GOT.mem': {}, 'GOT.func': {} };
  const unexpectedGot = [];
  for (const im of WebAssembly.Module.imports(sideModule)) {
    if (im.module !== 'GOT.mem' && im.module !== 'GOT.func') continue;
    if (im.module === 'GOT.mem' && NULLABLE_GOT_MEM.has(im.name)) {
      got['GOT.mem'][im.name] = new WebAssembly.Global({ value: 'i32', mutable: true }, 0);
    } else {
      unexpectedGot.push(`${im.module}.${im.name}`);
    }
  }
  if (unexpectedGot.length) {
    fail(`${path} imports GOT entries this host has no value for: ${unexpectedGot.join(', ')}. ` +
         'Only symbols known to be legitimately NULL in a library unit are supplied ' +
         'as 0; anything else needs a real address, and inventing one would produce ' +
         'a null dereference far from here.');
  }

  const full = { env: senv, ...got };
  const missing = WebAssembly.Module.imports(sideModule)
    .filter((im) => !full[im.module] || full[im.module][im.name] === undefined)
    .map((im) => `${im.module}.${im.name}`);
  if (missing.length) fail(`${path} imports what nothing supplies: ${missing.join(', ')}`);

  const side = new WebAssembly.Instance(sideModule, full);
  side.exports.__wasm_apply_data_relocs();
  side.exports.__wasm_call_ctors();

  // The descriptor global holds an OFFSET from __memory_base, not an address.
  const descName = `ecv_program_${index}`;
  const desc = side.exports[descName];
  if (desc === undefined) fail(`${path} does not export ${descName}`);
  const off = desc.value;
  if (off < 0 || off >= need.memSize) {
    fail(`${descName} = ${off} is not an offset into the module's own ${need.memSize} bytes`);
  }

  const codes = { '-1': 'NULL descriptor', '-2': 'ABI mismatch', '-3': 'frozen registry (no late hook)',
                  '-4': 'duplicate', '-5': 'a static registry is present' };
  const rc = S.ecv_register_program(memBase + off, BigInt(opts.programSize));
  if (rc !== 0) fail(`ecv_register_program(${label}) -> ${rc} (${codes[String(rc)] ?? 'unknown'})`);
  console.log(`placed ${label}: mem=${memBase} table=${tableBase} ${descName}@${off} registered`);
}

// The MAIN program is placed before boot, exactly as the pre-_start embedder
// does. Only the dlopen-able units are deferred -- that is the distinction
// under test, and placing everything early would erase it.
place(opts.main, 0, 'main');

// --- run ------------------------------------------------------------------
if (S.ecv_boot() !== 0) fail('ecv_boot failed');
console.log('boot: ok');

const ECV_IDLE = 0, ECV_PREEMPTED = 1, ECV_EXITED = 2;
let slices = 0, code = null;
const MAX_SLICES = 200000;
try {
  for (; slices < MAX_SLICES; slices++) {
    const r = S.ecv_run_slice(2000);

    // FLUSH BETWEEN SLICES, never inside one. See the header.
    while (queue.length) {
      const { token, unit } = queue.shift();
      let ok = true;
      try {
        place(unit.path, unit.index, unit.hash);
      } catch (err) {
        ok = false;
        console.log(`serving ${unit.hash} failed: ${err && err.message ? err.message : err}`);
      }
      const woke = S.ecv_side_loaded(token, ok ? 1 : 0);
      served.push({ hash: unit.hash, token, ok, woke });
      console.log(`ecv_side_loaded(${token}, ${ok ? 1 : 0}) -> woke=${woke}`);
      if (!woke) {
        fail(`nothing was waiting for token ${token}. The guest parked on a ` +
             'different token than the one this host was handed, so the load was ' +
             'delivered to nobody and the guest will hang.');
      }
    }

    if (r === ECV_EXITED) { code = S.ecv_exit_code(); break; }
    if (r === ECV_IDLE && queue.length === 0 && served.length && S.ecv_next_deadline_in_ms() < 0) {
      // Idle with nothing pending and no timer: either finished by another
      // route or genuinely stuck. One more slice decides it.
      const again = S.ecv_run_slice(2000);
      if (again === ECV_EXITED) { code = S.ecv_exit_code(); break; }
      if (again === ECV_IDLE && queue.length === 0) {
        fail('the guest is idle with no pending load and no deadline: it is stuck. ' +
             'A dlopen parked on a token nobody delivered looks exactly like this.');
      }
    }
  }
} catch (err) {
  if (!exit.called) fail(`trapped: ${err && err.stack ? err.stack : String(err)}`);
  code = exit.code;
}
if (slices >= MAX_SLICES) fail(`the guest never finished in ${MAX_SLICES} slices`);

console.log(`slices: ${slices + 1}`);
console.log(`loads served: ${served.length} (${served.map((s) => s.hash).join(', ')})`);
console.log(`HOSTEDEMBEDDER-COMPLETE exit=${code ?? (exit.called ? exit.code : 'unknown')}`);
