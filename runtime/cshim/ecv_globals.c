// Wasm globals shared between ecvisor and the lifted code.
//
// WHY THIS FILE EXISTS. The lifted module calls into ecvisor three times per
// guest BL -- `_ecv_save_call_history`, `_ecv_func_epilogue`, and
// `_ecv_suspended` after every call and every syscall. Counted on the linked
// bash fixture (7,567 lifted functions) that is 32,972 / 32,115 / 33,540 call
// sites, about 13 per lifted function, and none of it is guest work: it is the
// cooperative scheduler's own bookkeeping, which exists to serve replay-based
// resume. See .agents/docs/MULTIMODULE.md 3c.
//
// `_ecv_suspended`'s entire body is one flag read. A wasm GLOBAL expresses that
// in one instruction with no call at all, so the lifter emits `global.get`
// against the symbol below instead (elfconv patch 0059) and the 33,540 calls
// become 33,540 inline reads.
//
// Rust cannot express a wasm global, exactly as it cannot express the inline asm
// in ecv_sp.c beside this file -- so the definition lives here and ecvisor
// reaches it through the two accessors. `address_space(1)` is clang's spelling
// for a wasm global; it needs no inline asm, and it stays inside Wasm 2.0
// (`mutable-globals`, which the emitted module already required for
// `__stack_pointer`).
//
// Verified before this file was written, in the builder image on BOTH LLVM
// lines (16 via the wasi-sdk clang, 22 via `clang-22 --target=wasm32-wasi`,
// which is what internal/builder.compileIR builds): the definition below is a
// real `(global (mut i32))`, an `external addrspace(1)` load written in LLVM IR
// and compiled through `clang -x ir -c` resolves against it, `wasm-opt -g -O0`
// preserves it, and a write through the accessor is observed by the IR-side
// read. Neutralized by pointing the accessor at a decoy global, which makes the
// read stop observing the write.

// Non-zero once a syscall has suspended the current process (a blocking
// syscall, exit, or execve), until the scheduler picks the leg back up.
//
// One flag for the whole runtime, not one per process: it lives on EcvContext,
// of which there is exactly one, and entry.rs reads and clears it once per
// scheduler leg. A wasm global is therefore the same object it always was.
__attribute__((address_space(1))) int __ecv_unwinding;

int ecv_get_unwinding(void) {
  return __ecv_unwinding;
}

void ecv_set_unwinding(int v) {
  __ecv_unwinding = v;
}

// --- Call history (P0.4 PROTOTYPE, elfconv patch 0060) --------------------
//
// The other two per-BL crossings. `_ecv_save_call_history` pushes a
// {func_vma, return_pc} frame and writes x30; `_ecv_func_epilogue` pops. The
// lifter can do both inline if it knows where the stack lives, which is what
// these three globals say. Measured headroom for removing both hooks entirely:
// -31% on a call-heavy guest, -4.7% on a realistic one. Trimming the Rust
// bodies alone reached only -2.3% of that, so the rest is the CALLS.
//
// `__ecv_ch_base` is a byte offset into linear memory, republished by the
// runtime whenever the buffer grows. `__ecv_ch_len` is the authoritative depth
// -- Rust reads and writes it through the accessors, the lifted code bumps it
// directly, and there is exactly one of each because the scheduler is
// cooperative and only switches at a block, a yield or an exit.
__attribute__((address_space(1))) int __ecv_ch_base;
__attribute__((address_space(1))) int __ecv_ch_len;
__attribute__((address_space(1))) int __ecv_ch_cap;

// Non-zero when ANY per-call diagnostic is armed. The lifted fast path tests
// this first and calls the original hooks when set, so every diagnostic keeps
// working unchanged and the old path stays reachable for differential testing.
__attribute__((address_space(1))) int __ecv_slow;

// Whether THIS MODULE was built with `--inline-call-history`.
//
// WEAK and zero here; `link-all --inline-call-history` links a strong `= 1`
// beside it, and a strong definition wins. Absent, the weak zero stands -- so a
// module that never opted in cannot be talked into the fast path, and there is
// no undefined symbol to become an `env` import that fails at instantiation.
//
// This exists because the runtime gate ALONE was not safe. Setting
// `RAPTORMARK_ECV_INLINE_CH=1` against a default-built module made ecvisor adopt
// a length that the lifted code never maintains, truncating the call history at
// every syscall; a forking guest then produced no output at all. That is exactly
// the combination someone tries while debugging, and the variable is documented
// as a kill switch, so it had to become unreachable rather than merely
// discouraged.
__attribute__((weak)) int __ecv_ch_built = 0;

int ecv_ch_built(void) {
  return __ecv_ch_built;
}

int ecv_get_ch_len(void) {
  return __ecv_ch_len;
}

void ecv_set_ch_len(int v) {
  __ecv_ch_len = v;
}

void ecv_set_ch_buf(int base, int cap) {
  __ecv_ch_base = base;
  __ecv_ch_cap = cap;
}

void ecv_set_slow(int v) {
  __ecv_slow = v;
}
