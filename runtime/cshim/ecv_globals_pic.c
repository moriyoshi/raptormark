// ecv_globals.c, without the wasm globals, for the `wasix` profile.
//
// # Why a second shim rather than an #ifdef
//
// `ecv_globals.c` exists to define WASM GLOBALS -- `address_space(1)` -- because
// lifted code reads them inline: `__ecv_unwinding` for the suspend check (elfconv
// patch 0059) and `__ecv_ch_*` for the inline call history (patch 0060). Rust
// cannot express a wasm global, which is the whole reason that file is C.
//
// ❗ A WASM GLOBAL CANNOT BE COMPILED -fPIC. clang tries to relocate it through
// `__memory_base` and the backend cannot select the node:
//
//     fatal error: error in backend: Cannot select:
//     WebAssemblyISD::GLOBAL_GET<(load from @__ecv_unwinding, addrspace 1)>
//       ... TargetExternalSymbol:i32'__memory_base'
//
// The `wasix` supervisor MUST be PIC -- it has to be a dynamic library for
// WASIX's linker to treat the instance as dynamically linked at all -- so that
// file cannot be part of it. Keeping the two apart is clearer than one file
// under an `#ifdef` where half the declarations are illegal.
//
// # Why plain statics are not a weakening
//
// The same reasoning `context.rs` already applies to the HOST build, which has
// no shim and uses a plain static: ecvisor's scheduler is cooperative and only
// switches at a block, a yield or an exit, so there is no concurrent writer.
//
// The globals were never about atomicity. They were about being readable by
// LIFTED code without a call -- and under `wasix` the lifted code does not read
// them: patch 0067 puts the suspend check back to a `_ecv_suspended` CALL,
// which is the only reason a WASIX-loadable side module is possible.
//
// ⚠️ THIS FILE IS THEREFORE ONLY CORRECT ALONGSIDE PATCH 0067. If a `wasix`
// build ever lifted with the global form, the lifted code would read a wasm
// global nothing here defines, `--allow-undefined` would resolve it to zero, and
// the guest would never suspend -- a hang, not a link error.
// `internal/builder.LinkAll` forces `--suspend-via-call` for this profile, and
// `pipeline.Build` says so when it does.
//
// ⚠️ THE INLINE CALL HISTORY IS NOT AVAILABLE HERE for the same reason: lifted
// code reads `__ecv_ch_*` directly, and these are ordinary memory. `ecv_ch_built`
// returns 0, which is exactly how ecvisor already refuses the fast path when a
// module was not lifted for it (`context.rs` checks it before enabling).

static int ecv_unwinding_flag = 0;

int ecv_get_unwinding(void) {
  return ecv_unwinding_flag;
}

void ecv_set_unwinding(int v) {
  ecv_unwinding_flag = v;
}

// ⚠️ ZERO, AND DELIBERATELY NOT WEAK. In `ecv_globals.c` this is a weak zero a
// module can override with a strong `__ecv_ch_built = 1` to opt into the inline
// call history. Here there is nothing to opt into -- the history globals are not
// wasm globals, so lifted code cannot bump them -- and a build that linked the
// marker object anyway would turn the fast path on over ordinary memory the
// lifted code never touches. Returning a constant makes that unrepresentable.
int ecv_ch_built(void) {
  return 0;
}

// The remaining accessors ecvisor links against. They must exist -- `libecvisor.a`
// references them unconditionally -- and they must be harmless, because with
// `ecv_ch_built()` returning 0 the runtime never enables the path that would use
// them.
static int ecv_ch_len_v = 0;
static int ecv_ch_base_v = 0;
static int ecv_ch_cap_v = 0;
static int ecv_slow_v = 0;

int ecv_get_ch_len(void) {
  return ecv_ch_len_v;
}

void ecv_set_ch_len(int v) {
  ecv_ch_len_v = v;
}

void ecv_set_ch_buf(int base, int cap) {
  ecv_ch_base_v = base;
  ecv_ch_cap_v = cap;
}

void ecv_set_slow(int v) {
  ecv_slow_v = v;
}
