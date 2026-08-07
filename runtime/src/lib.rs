//! ecvisor — raptormark's supervisor runtime for elfconv-lifted programs,
//! built as a wasm32-wasip1 staticlib and linked with the clang-compiled
//! lifted objects by wasi-sdk lld (see builder/translate-one.sh).
//!
//! It owns the entire runtime surface the lifted code needs — entry/arena
//! setup, the remill intrinsics, dispatch tables, the pluggable VFS, and the
//! Linux syscall layer — replacing upstream elfconv's C++ runtime
//! (`Entry.cpp`, `Memory.cpp`, `Runtime.cpp`, `VmIntrinsics.cpp`,
//! `SyscallWasi.cpp`) entirely at link time. The lifted code treats the former
//! `RuntimeManager*` argument as an opaque pointer, which ecvisor points at
//! its own [`context::EcvContext`]; no C++ object ever exists in the module.
//!
//! ABI ground truth is the pinned elfconv submodule: the `_ecv_*` tables in
//! `runtime/Memory.h`, the `State` layout in
//! `backend/remill/include/remill/Arch/AArch64/Runtime/State.h` (mirrored and
//! layout-asserted in [`abi`]), and the undefined-symbol list of a lifted
//! object.
//!
//! The pure modules ([`vfs`], [`boot`]) carry `#[cfg(test)]` unit tests that
//! run on the host via `cargo test`; the wasm-only glue (`entry`, `intrinsics`,
//! `sys`) references the lifted-code externs and wasi-libc, so it compiles only
//! for `wasm32`.

pub mod abi;
pub mod arena;
pub mod boot;
pub mod context;
pub mod diag;
pub mod dlmap;
pub mod execmap;
pub mod loader;
// Deliberately NOT gated to wasm32: the point of the backend seam is that the
// socket logic and the address codec become reachable from `cargo test`. Only
// `net::wasmedge`, which holds the `extern` blocks, is gated.
pub mod net;
pub mod shebang;
pub mod trace;
pub mod vfs;

#[cfg(target_arch = "wasm32")]
mod entry;
#[cfg(target_arch = "wasm32")]
mod intrinsics;
#[cfg(target_arch = "wasm32")]
mod sys;

/// Aborts guest execution with a diagnostic, mirroring upstream's
/// `elfconv_runtime_error`.
pub(crate) fn runtime_error(msg: core::fmt::Arguments<'_>) -> ! {
    eprintln!("[ecvisor] fatal: {msg}");
    std::process::exit(1);
}

macro_rules! fatal {
    ($($arg:tt)*) => { crate::runtime_error(format_args!($($arg)*)) };
}
pub(crate) use fatal;
