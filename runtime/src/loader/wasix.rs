//! The backend that asks WASMER's WASIX runtime to load a side module.
//!
//! Unlike [`super::hosted`], nothing outside the module participates: WASIX's
//! `dlopen` IS the loader, and ecvisor drives it directly. There is no host to
//! call back, so a load either completes inside `request` or fails there --
//! this backend never returns [`LoadOutcome::Pending`].
//!
//! # The ABI was MEASURED, not read off a document
//!
//! wasmer tells you a signature if you ask it wrong. A `.wat` importing
//! `wasix_32v1.dlopen` with a deliberately wrong type produces:
//!
//! ```text
//! incompatible import type.
//! Expected  Function(FunctionType { params: [I32],     results: [I32] })   <- the MODULE's declaration
//! but received Function(FunctionType { params: [I32 x8], results: [I32] }) <- what WASMER PROVIDES
//! ```
//!
//! ⚠️ The wording is inverted from the intuitive reading. "Expected" is what the
//! module declared; "received" is the host's. Read it the other way and this
//! file would be written against a guess.
//!
//! Measured against wasmer 7.3.0:
//!
//! | import | signature |
//! |---|---|
//! | `wasix_32v1.dlopen` | `(i32 x8) -> i32` |
//! | `wasix_32v1.dlsym`  | `(i32 x6) -> i32` |
//! | `dlclose` | **not provided** -- not a WASIX import |
//! | `dlerror` | **not provided** -- errors come back in `err_buf` |
//!
//! There being no `dlerror` import is not a gap: ecvisor keeps its own per-task
//! `dlerror` for the guest, and WASIX's errors arrive through the out-buffer.
//!
//! # ✅ What actually blocks us, measured end to end
//!
//! WASIX **loads our real side module**: it parses the `dylink.0`, accepts the
//! memory and table, and resolves imports. It refuses at exactly one:
//!
//! ```text
//! failed to load module: Expected import to be a function: 'env'.__ecv_unwinding
//! ```
//!
//! Its linker requires every `env` import to be a FUNCTION. Our side modules
//! import four globals; WASIX supplies the three standard PIC ones
//! (`__stack_pointer`, `__memory_base`, `__table_base`) itself, and rejects
//! ours. `__ecv_unwinding` comes from `runtime/cshim/ecv_globals.c`, where
//! elfconv patch 0059 introduced it to replace an `_ecv_suspended()` CALL with a
//! global read -- an optimization, because the suspend check runs on every leg.
//!
//! So the blocker is a performance choice, revertible per profile. It is **not**
//! exception handling, **not** shared memory, and **not** `__tls_base`; all
//! three were proposed during this investigation and all three were wrong.
//!
//! # ⚠️ The main module must still be a dynamic library
//!
//! Adding `dylink.0` is what switches wasmer onto the dynamic-linking path; it
//! then requires `env.memory` (shared, 129..65536),
//! `env.__indirect_function_table`, `env.__stack_pointer`, `env.__memory_base`,
//! `env.__table_base` and a `__tls_base` export. That is why `--profile wasix`
//! has to be a link path and not merely an archive.

use super::{LoadOutcome, LoaderBackend};
use std::collections::HashMap;

/// Where the side modules live in the WASIX guest's filesystem.
///
/// wasmer maps host directories into the guest, so this is a path in ITS
/// namespace -- not in ecvisor's virtual rfs, which WASIX knows nothing about.
/// Overridable because only the embedder knows where it mounted them.
/// Read once at startup by `sys::init_diag_flags` and exposed via
/// `diag::side_dir`; see the note in `side_path`.
const _SIDE_DIR_ENV_DOC: &str = "RAPTORMARK_SIDE_DIR";

/// Scratch for WASIX's error text. Read back only for diagnostics; the guest's
/// own `dlerror` is ecvisor's, not this.
const ERR_BUF: usize = 256;

#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
enum UnitState {
    #[default]
    Cold,
    Ready,
    Failed,
}

/// Drives WASIX `dlopen` directly.
#[derive(Default)]
pub struct Wasix {
    /// Keyed by the opaque `idx` the seam passes, which is SPARSE -- see the
    /// note on [`LoaderBackend::request`]. A `Vec` indexed by it would ask for
    /// a billion elements the first time a pending token arrives.
    state: HashMap<usize, UnitState>,
}

impl Wasix {
    fn side_path(name: &[u8]) -> Vec<u8> {
        // ❗ NOT `std::env::var` HERE. This runs inside a `dlopen` syscall,
        // which is a post-fork path, and `std::env::var` there can hit
        // `lazy_lock::panic_poisoned` -- whose panic handler re-reads the
        // environment and loops forever. Read once in `sys::init_diag_flags`.
        // `diag::tests::env_is_read_only_from_startup_paths` caught this in the
        // first version of this file.
        let mut p = crate::diag::side_dir().as_bytes().to_vec();
        p.push(b'/');
        p.extend_from_slice(name);
        p.extend_from_slice(b".side.wasm");
        p
    }
}

impl LoaderBackend for Wasix {
    fn request(&mut self, idx: usize, name: &[u8]) -> LoadOutcome {
        match self.state.get(&idx).copied().unwrap_or_default() {
            UnitState::Ready => return LoadOutcome::Ready,
            // Sticky, as in `hosted`: WASIX will not succeed on the next attempt
            // either, and the guest re-enters this syscall on every wake.
            UnitState::Failed => return LoadOutcome::Failed("WASIX could not load this unit"),
            UnitState::Cold => {}
        }

        let path = Self::side_path(name);
        let mut err = [0u8; ERR_BUF];
        let mut handle: u32 = 0;
        let rc = unsafe {
            dlopen(
                path.as_ptr() as u32,
                path.len() as u32,
                RTLD_NOW,
                // ⚠️ err_buf BEFORE ld_library_path. Verified against
                // wasmer v7.3.0 `lib/wasix/src/syscalls/wasix/dlopen.rs`; the
                // first version of this file had them swapped, which sends
                // every diagnostic to address 0 and leaves the caller reading
                // an empty buffer. The arity is eight i32s either way, so
                // nothing about the type catches it.
                err.as_mut_ptr() as u32,
                ERR_BUF as u32,
                0, // ld_library_path: the path above is absolute
                0,
                &mut handle as *mut u32 as u32,
            )
        };
        if rc != 0 {
            self.state.insert(idx, UnitState::Failed);
            // ⚠️ 79 is `unknown`, a CATCH-ALL -- it does not identify the
            // cause, and `err_buf` comes back empty with it. The message says
            // the most likely reason without asserting it, because the most
            // likely reason is also the only one a caller can act on: the
            // supervisor is not linked as a dynamic library, so WASIX never
            // reaches any dl work at all.
            return LoadOutcome::Failed(if rc == ERRNO_UNKNOWN {
                "WASIX dlopen failed with errno 79 (unknown), which is what it \
                 returns before doing any work. The usual cause is that the \
                 supervisor is not linked as a dynamic library"
            } else {
                "WASIX dlopen failed"
            });
        }

        self.state.insert(idx, UnitState::Ready);
        LoadOutcome::Ready
    }

    fn note_loaded(&mut self, idx: usize, ok: bool) {
        // Nothing outside the module can report a load here -- `request` either
        // completed or failed. Recorded anyway so a host that calls
        // `ecv_side_loaded` out of turn cannot leave the state disagreeing with
        // what the guest was told.
        self.state.insert(
            idx,
            if ok {
                UnitState::Ready
            } else {
                UnitState::Failed
            },
        );
    }

    fn backend_name() -> &'static str {
        "wasix"
    }
}

/// `RTLD_NOW`. WASIX's flag values follow POSIX; lazy binding would defer
/// exactly the resolution this backend exists to force.
const RTLD_NOW: u32 = 2;

/// WASIX errno 79 = `unknown`, the LAST variant of its errno enum and a
/// catch-all.
///
/// ⚠️ **CORRECTION.** This was named `ERRNO_NOT_DYNAMIC` and documented as "the
/// errno WASIX returns when the calling instance is not a dynamic library",
/// inferred from the engine's error strings. Wrong. Counting the errno name
/// table out of the wasmer binary -- anchored on `notcapable` = 76, which
/// matches WASI preview1 -- gives `... xdev(75) notcapable(76) shutdown(77)
/// memviolation(78) unknown(79)`, and the binary also carries
/// `variant index 0 <= i < 80`.
///
/// So 79 says only "this failed". Measured: `dlopen` from an ordinary instance
/// returns it, `dlsym` with a bogus handle returns it, and `err_buf` comes back
/// EMPTY in both cases. It cannot be used to distinguish WHY.
const ERRNO_UNKNOWN: i32 = 79;

/// The two imports this backend adds, with the measured signatures.
///
/// ⚠️ Every parameter is `u32` rather than a pointer type on purpose. The
/// measured type is `[I32 x8] -> I32`; declaring a `*const u8` would still lower
/// to i32 on wasm32, but it hides that the contract is positional and makes a
/// future 64-bit port (`wasix_64v1`) look like a no-op when it is not.
#[link(wasm_import_module = "wasix_32v1")]
extern "C" {
    /// `(path, path_len, flags, err_buf, err_buf_len, ld_library_path,
    /// ld_library_path_len, out_handle) -> errno`
    ///
    /// ⚠️ ORDER TAKEN FROM THE SOURCE, not from the arity. wasmer's import type
    /// is eight i32s; which parameter is which appears nowhere in it. Verified
    /// against v7.3.0 `lib/wasix/src/syscalls/wasix/dlopen.rs`.
    fn dlopen(
        path: u32,
        path_len: u32,
        flags: u32,
        err_buf: u32,
        err_buf_len: u32,
        ld_library_path: u32,
        ld_library_path_len: u32,
        out_handle: u32,
    ) -> i32;

    /// `(handle, name, name_len, err_buf, err_buf_len, out_ptr) -> errno`
    ///
    /// Currently unused: resolving the unit's descriptor by name needs the
    /// symbol `ecv_program_<index>`, and a lazily-loaded unit has only a hash.
    /// See the `⚠️ UNRESOLVED` note in `runtime/src/loader/mod.rs`.
    #[allow(dead_code)]
    fn dlsym(
        handle: u32,
        name: u32,
        name_len: u32,
        err_buf: u32,
        err_buf_len: u32,
        out_ptr: u32,
    ) -> i32;
}
