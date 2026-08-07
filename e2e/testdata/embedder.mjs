// A DEVELOPMENT embedder: it runs the multi-module artifacts that `link-all
// --side-out` produces, by driving the sequence in .agents/docs/MULTIMODULE.md
// §8.
//
// ⚠️ This is NOT a deployment target and must not become one. It exists to
// answer the one question §5 could not: whether the protocol -- reserve, place,
// relocate, register, start -- actually works on real artifacts, or only on
// paper. Everything it does that a shipping embedder would also have to do is
// marked STEP n, matching §8.
//
// # What it cannot do, stated up front
//
// A raptormark module imports 28 WASI functions, 11 of which are WasmEdge's
// non-standard socket extension. `node:wasi` implements preview1 and none of
// those 11, so they are stubbed to ENOTSUP here. A guest that opens a socket
// therefore fails in this harness and works under wasmedge -- which is the
// opposite of the usual direction, and worth knowing before reading a failure
// as a defect in the multi-module path.
//
// `sock_accept` and `sock_shutdown` are the exception: ecvisor uses the
// standardized preview1 forms of both (runtime/src/sys.rs), so node's own
// implementations are ABI-compatible and are left in place.
//
// # Usage
//
//   node embedder.mjs --supervisor sup.wasm --program-size 72 \
//        [--dir /w] [--env K=V]... side1.wasm [side2.wasm ...] [-- guest args]
//
// Prints a line per step; the last line is EMBEDDER-SEQUENCE-COMPLETE followed
// by the guest's own output, or a diagnosis of which step failed.

import { readFileSync } from 'node:fs';
import { WASI } from 'node:wasi';

// ---------------------------------------------------------------------------
// Arguments
// ---------------------------------------------------------------------------

function parseArgs(argv) {
  const out = { supervisor: null, sides: [], programSize: null, dir: null, env: {}, guestArgs: [] };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === '--') { out.guestArgs = argv.slice(i + 1); break; }
    else if (a === '--supervisor') out.supervisor = argv[++i];
    else if (a === '--program-size') out.programSize = Number(argv[++i]);
    else if (a === '--dir') out.dir = argv[++i];
    else if (a === '--env') { const [k, ...v] = argv[++i].split('='); out.env[k] = v.join('='); }
    else out.sides.push(a);
  }
  if (!out.supervisor) fail('--supervisor is required');
  if (out.sides.length === 0) fail('at least one side module is required');
  // ⚠️ Deliberately NOT defaulted and deliberately NOT asked of the supervisor.
  // This value plays the part `registry.c` plays on the flat path: it is the C
  // side's opinion of sizeof(EcvProgram), and `ecv_register_program` compares it
  // with the Rust side's. A harness that asked the supervisor for the number and
  // handed it straight back would turn that ABI check into a tautology.
  if (!out.programSize) fail('--program-size is required (sizeof(EcvProgram))');
  return out;
}

function fail(msg) {
  console.log(`EMBEDDER-FAILED: ${msg}`);
  process.exit(1);
}

// ---------------------------------------------------------------------------
// dylink.0
// ---------------------------------------------------------------------------

// Read independently of internal/oci's Go parser, on purpose: the harness has to
// stand alone to be usable by hand, and `TestEmbedderRunsTheSideModule` compares
// what it prints here against what the Go parser found. Two parsers that agree
// are evidence; one parser is an assumption.
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
    if (id === 0) { // custom section
      const nameLen = leb();
      const name = Buffer.from(bytes.subarray(p, p + nameLen)).toString();
      p += nameLen;
      if (name === 'dylink.0') {
        while (p < end) {
          const sub = bytes[p++];
          const subSize = leb();
          const subEnd = p + subSize;
          if (sub === 1) { // WASM_DYLINK_MEM_INFO
            return {
              memSize: leb(), memAlignLog2: leb(),
              tableSize: leb(), tableAlignLog2: leb(),
            };
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

// ---------------------------------------------------------------------------
// The host surface
// ---------------------------------------------------------------------------

// WasmEdge's socket extension: the 11 non-standard names, minus the two whose
// preview1 forms ecvisor actually uses. ENOTSUP rather than a trap, so a guest
// that probes for sockets gets an errno it can handle instead of dying inside
// the harness -- and so the failure names itself if one is reached.
const ENOTSUP = 58;
const wasmEdgeSocketStubs = [
  'sock_bind', 'sock_connect', 'sock_getlocaladdr', 'sock_getpeeraddr',
  'sock_getsockopt', 'sock_listen', 'sock_open', 'sock_recv_from',
  'sock_send_to', 'sock_setsockopt',
];

// ⚠️ node:wasi ABORTS THE PROCESS -- SIGABRT, no message, no stack -- on the
// first WASI call after the guest grows its linear memory. The binding caches
// the backing store when the memory is bound, `memory.grow` detaches that
// ArrayBuffer, and nothing re-reads it.
//
// It is not this harness's fault and not the multi-module path's: reduced to 15
// lines (`fprintf`, `malloc(384 MiB)`, `fprintf`), one ordinary module, the
// documented `wasi.start()` call, and it still aborts. Every raptormark guest
// hits it, because ecvisor allocates a 384 MiB arena during startup.
//
// The fix is to re-bind on growth. `kSetMemory` is private, so it is fished out
// of the instance's own symbols -- which is exactly the kind of thing that makes
// this a DEVELOPMENT harness. Cost is one identity comparison per WASI call, and
// it fires once per growth (measured: 1 rebind for a guest that allocates the
// arena and prints).
function hostImports(wasi, reached, memoryOf, exit) {
  const imports = wasi.getImportObject();
  const p1 = imports.wasi_snapshot_preview1;

  const kSetMemory = Object.getOwnPropertySymbols(wasi).find((s) => String(s) === 'Symbol(kSetMemory)');
  if (!kSetMemory) {
    fail('node:wasi no longer exposes kSetMemory; without it the first WASI call ' +
         'after the guest grows memory aborts the process with no diagnostic');
  }
  let lastBuffer = null;
  const rebind = () => {
    const mem = memoryOf();
    if (mem && mem.buffer !== lastBuffer) { lastBuffer = mem.buffer; wasi[kSetMemory](mem); }
  };

  for (const name of Object.keys(p1)) {
    const orig = p1[name];
    p1[name] = (...args) => { rebind(); return orig(...args); };
  }
  for (const name of wasmEdgeSocketStubs) {
    p1[name] = (...args) => { reached.add(name); return ENOTSUP; };
  }
  // The guest's exit status, recorded on the way past. node keeps it on a
  // private symbol that only `wasi.start()` reads, and `start()` is the one
  // method this harness cannot use -- so it is taken from the call instead of
  // invented afterwards. Reporting a made-up code would make every clean exit
  // and every failure look alike.
  const origExit = p1.proc_exit;
  p1.proc_exit = (code) => { exit.code = code; exit.called = true; return origExit(code); };
  return imports;
}

// ---------------------------------------------------------------------------
// The sequence
// ---------------------------------------------------------------------------

const opts = parseArgs(process.argv.slice(2));
const reachedSocket = new Set();

const wasi = new WASI({
  version: 'preview1',
  args: ['guest', ...opts.guestArgs],
  env: opts.env,
  preopens: opts.dir ? { '/': opts.dir } : {},
  returnOnExit: true,
});

// STEP 1: instantiate the supervisor with the WASI imports.
//
// The memory is reached through a thunk because the import object has to exist
// before the instance that exports the memory does.
let supInstance = null;
const exit = { called: false, code: 0 };
const supModule = new WebAssembly.Module(readFileSync(opts.supervisor));
supInstance = new WebAssembly.Instance(
  supModule, hostImports(wasi, reachedSocket, () => supInstance && supInstance.exports.memory, exit));
const S = supInstance.exports;
console.log(`step1: supervisor instantiated, ${Object.keys(S).length} exports`);

// node's WASI binds the memory it will read guest pointers through. `start()`
// would do this AND call `_start` immediately, and every step below has to
// happen before `_start` -- so the memory is bound through a stand-in instance
// and `_start` is called by hand at step 9.
wasi.initialize({ exports: { memory: S.memory } });

for (const [name, need] of [['memory', S.memory], ['__indirect_function_table', S.__indirect_function_table],
                            ['__stack_pointer', S.__stack_pointer], ['ecv_reserve_side', S.ecv_reserve_side],
                            ['ecv_register_program', S.ecv_register_program], ['_start', S._start]]) {
  if (need === undefined) fail(`the supervisor does not export ${name}; it was not linked for an embedder`);
}

const registered = [];
for (const [i, path] of opts.sides.entries()) {
  const bytes = readFileSync(path);

  // STEP 2: read what the side module needs.
  const need = dylinkMemInfo(bytes);
  if (!need) fail(`${path} has no dylink.0 MEM_INFO; it was linked flat, not as a side module`);
  console.log(`step2[${i}]: needs mem=${need.memSize} align=2^${need.memAlignLog2} table=${need.tableSize}`);

  // STEP 3: the SUPERVISOR reserves the memory. Not memory.grow and not
  // __heap_base -- both let the supervisor's own dlmalloc hand the same region
  // out again later, and the corruption is silent and arbitrarily delayed.
  //
  // ⚠️ dylink.0 stores the LOG2 of the alignment. Passing the field through
  // unshifted would ask for align=4 where 16 was meant: a power of two, so every
  // check accepts it, and the module lands under-aligned.
  const align = 1 << need.memAlignLog2;
  const memBase = S.ecv_reserve_side(BigInt(need.memSize), BigInt(align));
  if (memBase === 0) fail(`ecv_reserve_side(${need.memSize}, ${align}) returned 0`);
  if (memBase % align !== 0) fail(`ecv_reserve_side returned ${memBase}, which is not ${align}-aligned`);
  console.log(`step3[${i}]: reserved at ${memBase} (0x${memBase.toString(16)})`);

  // STEP 4: table slots.
  //
  // Verified by removal: a supervisor linked without `-Wl,--growable-table`
  // reaches exactly here and no further. wasm-ld caps the exported table at its
  // initial length, and V8 reports only "failed to grow table by 900" -- which
  // names neither the flag nor the module that lacks it.
  const tableBase = S.__indirect_function_table.length;
  try {
    S.__indirect_function_table.grow(need.tableSize);
  } catch (err) {
    fail(`growing the supervisor's table by ${need.tableSize} failed (${err}). ` +
         'The usual cause is a supervisor linked without -Wl,--growable-table, which ' +
         'caps the table at its initial length and leaves room for zero side modules.');
  }
  console.log(`step4[${i}]: table base ${tableBase}, grown to ${S.__indirect_function_table.length}`);

  // STEP 5: instantiate against the supervisor's memory, table and shadow stack.
  const env = {
    memory: S.memory,
    __indirect_function_table: S.__indirect_function_table,
    __stack_pointer: S.__stack_pointer,
    __memory_base: new WebAssembly.Global({ value: 'i32', mutable: false }, memBase),
    __table_base: new WebAssembly.Global({ value: 'i32', mutable: false }, tableBase),
  };
  for (const name of Object.keys(S)) {
    if (env[name] === undefined) env[name] = S[name];
  }
  const sideModule = new WebAssembly.Module(bytes);
  const missing = WebAssembly.Module.imports(sideModule)
    .filter((im) => im.module !== 'env' || env[im.name] === undefined)
    .map((im) => `${im.module}.${im.name}`);
  if (missing.length) fail(`${path} imports what nothing supplies: ${missing.join(', ')}`);
  const side = new WebAssembly.Instance(sideModule, { env });
  console.log(`step5[${i}]: instantiated`);

  // STEP 6: rebase the module's data pointers, then run its constructors.
  side.exports.__wasm_apply_data_relocs();
  side.exports.__wasm_call_ctors();
  console.log(`step6[${i}]: relocs applied, ctors run`);

  // STEP 7: where the descriptor ended up.
  //
  // ⚠️ CORRECTION, found by running this. MULTIMODULE.md §8 said the exported
  // global "holds the ADDRESS of the descriptor". It does not: it holds the
  // OFFSET from `__memory_base`, and the embedder adds the base itself. The
  // global section is 8 bytes -- one `i32.const 393476` -- and neither
  // `__wasm_apply_data_relocs` nor `__wasm_call_ctors` touches it, because a
  // wasm global's initialiser is a const expression and `__memory_base + off`
  // is not one without the extended-const proposal. The relocs fix up the
  // module's DATA; nothing was ever going to fix up this global.
  //
  // Reading it as an address is the dangerous mistake rather than the loud one.
  // 393476 is a perfectly valid address inside the supervisor's own heap, so
  // `ecv_register_program` would accept it and the supervisor would read a
  // descriptor out of somebody else's allocation.
  const descName = `ecv_program_${i}`;
  const desc = side.exports[descName];
  if (desc === undefined) fail(`${path} does not export ${descName}`);
  const off = desc.value;
  if (off < 0 || off >= need.memSize) {
    fail(`${descName} = ${off} is not an offset into the module's own ${need.memSize} bytes. ` +
         'If a newer wasm-ld began emitting an absolute address here (extended-const), ' +
         'this is where that shows, and the addition below has to go.');
  }
  const addr = memBase + off;
  console.log(`step7[${i}]: ${descName} at offset ${off} -> ${addr}`);

  // STEP 8: tell the supervisor. A non-zero code names the rule that was broken.
  const codes = {
    '-1': 'NULL descriptor', '-2': `ABI mismatch: the harness said sizeof(EcvProgram)=${opts.programSize} and the runtime disagrees`,
    '-3': 'the registry is frozen: _start already read it', '-4': 'duplicate registration',
    '-5': 'a static registry is present: this supervisor was linked with a registry.c',
  };
  const rc = S.ecv_register_program(addr, BigInt(opts.programSize));
  if (rc !== 0) fail(`ecv_register_program -> ${rc} (${codes[String(rc)] ?? 'unknown code'})`);
  console.log(`step8[${i}]: registered`);
  registered.push(descName);
}

// STEP 9: run.
console.log(`step9: starting with ${registered.length} program(s) registered`);
try {
  S._start();
} catch (err) {
  // node's WASI throws a private sentinel out of proc_exit when returnOnExit is
  // set, and `wasi.start()` is what normally swallows it. Calling `_start` by
  // hand means catching it here; a throw that did NOT come through proc_exit is
  // a real trap and must not be reported as a clean exit.
  if (!exit.called) {
    console.log(`EMBEDDER-FAILED: the guest trapped: ${err && err.stack ? err.stack : String(err)}`);
    process.exit(1);
  }
}
if (reachedSocket.size) {
  console.log(`note: the guest reached ${[...reachedSocket].join(', ')}, which this harness stubs to ENOTSUP`);
}
console.log(`EMBEDDER-SEQUENCE-COMPLETE exit=${exit.code}`);
