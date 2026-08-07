// Shadow-stack-pointer probe.
//
// All that survives of the old yield-unwind shim (ecv_sjlj.c). That shim existed
// because a blocking syscall had to get from deep inside lifted code back to the
// scheduler, and asyncify could not do it under elflift's --fork_emulation
// codegen; it used setjmp/longjmp lowered to wasm EH, which then had to be run
// through `wasm-opt --translate-to-exnref`.
//
// Suspension is a plain return now (elfconv patch 0026 tests `_ecv_suspended`
// after every syscall and every lifted call), so there is no unwind, no EH, and
// no exception-handling proposal in the emitted module -- which is what lets a
// stock runwasi shim run it. The shadow-stack leak the old shim had to clamp at
// leg boundaries is gone too: a real `ret` runs the real epilogue, so each frame
// pops its own `__stack_pointer`.
//
// What remains is the diagnostic below, kept because Rust cannot express the
// wasm inline asm it needs.

// Read the wasm shadow-stack pointer (the `__stack_pointer` mutable global that
// lifted C prologues/epilogues push and pop). wasi-sdk's clang does not implement
// the `__builtin_stack_save`/`__builtin_stack_restore` GCC builtins (it handles
// VLA scoping internally), so we reach the global directly via wasm inline asm.
// The `.globaltype` declares the linker-provided global. Reading it does not
// itself touch the shadow stack (no address-taken locals), so this is safe to
// call at any depth.
__asm__(".globaltype __stack_pointer, i32\n");

// Exported for the ecvisor low-water probe (RAPTORMARK_ECV_LEGSP, and the
// _ecv_save_call_history hook in runtime/src/intrinsics.rs).
unsigned long ecv_cur_sp(void) {
  void *sp;
  __asm__ volatile("global.get __stack_pointer\n\tlocal.set %0" : "=r"(sp)::"memory");
  return (unsigned long)(__UINTPTR_TYPE__) sp;
}
