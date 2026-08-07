//! The loader backend seam: how a UNIT's wasm code becomes reachable.
//!
//! `dlopen` and `execve` both need the same thing -- make unit `k` runnable --
//! and differ only in what they do afterwards. Everything about that operation
//! except *how the code arrives* is backend-independent and lives in
//! [`crate::context::EcvContext::ensure_unit_loaded`]: merging the dispatch
//! tables, loading the data sections, running the ifuncs and the constructors.
//! This module is the remaining sliver.
//!
//! # Why a cfg-selected type alias, and not `dyn`
//!
//! The same reason [`crate::net`] gives, and it is not a style preference.
//! `wasm-ld` emits an import for every undefined function symbol reachable from
//! live code. A trait object's vtable takes the address of every method of every
//! impl, so every backend stays live, so EVERY backend's imports end up in the
//! module -- and a stock-shim artifact would import WASIX's `dlopen`, which no
//! runwasi shim supplies, and fail to instantiate. `--gc-sections` cannot help:
//! it cannot prove a vtable slot is never called. An `enum` with a `match` has
//! the same problem for the same reason.
//!
//! So [`LoaderBackend`] is a CONFORMANCE CONTRACT -- one set of signatures that
//! every implementation is checked against, and the vehicle for
//! backend-generic tests -- rather than a polymorphism mechanism.
//!
//! # Why the contract is async-capable when the only backend is synchronous
//!
//! Decided 2026-08-23 before the signature was written, because it cannot be
//! retrofitted without touching every caller. Chromium will not synchronously
//! compile a wasm module over **8 MB** on the main thread, and this tree's
//! CPython side module is **36.4 MB**. Since `execve` is also a trigger, a
//! browser host CANNOT be served by a synchronous seam. [`Preloaded`] never
//! returns [`LoadOutcome::Pending`], but the shape is there for the backend that
//! must -- and the park/wake path behind it is implemented and tested rather
//! than stubbed, so the first backend to return `Pending` does not also have to
//! build the scheduler side.
//!
//! [`Preloaded`]: preloaded::Preloaded

// Exactly one backend is COMPILED, not merely selected. `hosted` calls a wasm
// import, so it exists only on wasm32 -- on the host build there is nothing for
// it to call and `cargo test` would not link.
#[cfg(all(target_arch = "wasm32", feature = "load-hosted"))]
pub mod hosted;
pub mod preloaded;
#[cfg(all(target_arch = "wasm32", feature = "load-wasix"))]
pub mod wasix;

/// ⚠️ MUTUAL EXCLUSION IS AN ERROR HERE, not a precedence chain.
///
/// `net::Net` resolves an over-specified feature set by cfg ordering, so
/// enabling two network backends silently picks one. That is survivable there
/// because both are answering the same calls. It is NOT survivable here: the
/// backends differ in whether they add a host IMPORT, so a silent pick decides
/// whether the artifact can be instantiated by a stock shim at all -- and the
/// failure would appear at deploy time, as a module that will not load, with
/// nothing pointing back at a feature flag.
#[cfg(any(
    all(feature = "load-hosted", feature = "load-preloaded"),
    all(feature = "load-hosted", feature = "load-wasix"),
    all(feature = "load-preloaded", feature = "load-wasix"),
))]
compile_error!(
    "exactly one loader backend may be enabled: two or more of `load-hosted`, \
     `load-preloaded` and `load-wasix` are on. They differ in WHICH host import \
     the module carries -- none, `env.ecv_host_load_side`, or WASIX's \
     `wasix_32v1.dlopen` -- so picking one silently would decide at deploy time \
     whether a given engine can instantiate the artifact at all."
);

/// The backend this build talks to. Exactly one, chosen at compile time.
#[cfg(all(target_arch = "wasm32", feature = "load-hosted"))]
pub type Loader = hosted::Hosted;
/// WASIX drives its own loader: `dlopen` IS the host, so nothing calls back and
/// this backend never returns `Pending`.
#[cfg(all(target_arch = "wasm32", feature = "load-wasix"))]
pub type Loader = wasix::Wasix;
/// The default, and what a host build always gets: `hosted` and `wasix` call
/// wasm imports that do not exist off-target, so `cargo test` could not link
/// either.
#[cfg(not(all(
    target_arch = "wasm32",
    any(feature = "load-hosted", feature = "load-wasix")
)))]
pub type Loader = preloaded::Preloaded;

/// What a load attempt yielded.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum LoadOutcome {
    /// The unit's code is reachable now; the caller may proceed.
    Ready,
    /// The host has been asked and has not finished. The caller must park the
    /// guest the way a blocking syscall parks it and retry when resumed.
    ///
    /// ⚠️ Never returned by [`preloaded::Preloaded`], so on the shipping
    /// artifact this arm is unreachable. It IS wired:
    /// `EcvContext::apply_load_outcome` parks the caller on
    /// `BlockedOn::SideLoad`, and `ecv_side_loaded` wakes it. The scheduler
    /// knows about that state too -- without `has_side_load_waiters` a parked
    /// process has no fd and no deadline and reads exactly like a deadlock.
    Pending,
    /// It cannot be loaded. The string reaches the guest through `dlerror`, so
    /// it is written for whoever reads that, not for a log.
    Failed(&'static str),
}

/// One set of signatures, checked against every implementation.
pub trait LoaderBackend: Default {
    /// Ask for unit `idx`'s code to be made reachable.
    ///
    /// `name` is the unit's content hash, which is how an embedder identifies
    /// the side module to instantiate. It is passed even though `Preloaded`
    /// ignores it, because a backend that must name the artifact has no other
    /// way to learn it -- and adding a parameter later means touching the
    /// callers this seam exists to insulate.
    ///
    /// ❗ `idx` IS OPAQUE AND SPARSE. It is usually a registry index, but for a
    /// unit that has no index yet -- every unit on its first `dlopen` under a
    /// host-driven loader, because the descriptor lives in the side module --
    /// it is a synthetic token at or above
    /// `crate::context::PENDING_UNIT_BASE` (2^30). A backend MUST NOT use it to
    /// index a dense array. `Hosted` did, via `Vec::resize(idx + 1)`, and the
    /// first such call quietly allocated ~1 GiB while the SECOND dlopen died of
    /// `capacity overflow` -- one successful load away from the cause.
    fn request(&mut self, idx: usize, name: &[u8]) -> LoadOutcome;

    /// The host reports that unit `idx` finished loading (or failed).
    ///
    /// Defaulted to a no-op because a synchronous backend has nothing to record
    /// -- `Preloaded` never returns `Pending`, so nothing can be outstanding.
    /// It is on the TRAIT rather than on the one implementation that needs it so
    /// the export calling it does not have to know which backend is compiled in.
    fn note_loaded(&mut self, _idx: usize, _ok: bool) {}

    /// A short identifier, for diagnostics and for the exclusion test.
    fn backend_name() -> &'static str;
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The default build must be `preloaded`, which is the one that ships: it
    /// adds no import, so `e2e/imports_test.go`'s pinned 28 stay 28.
    ///
    /// This runs on the HOST, where `hosted` cannot be selected at all, so it
    /// pins the default rather than the whole selection matrix -- the matrix is
    /// what `//runtime:loader_exclusion_test` is for.
    #[test]
    fn the_default_backend_is_preloaded() {
        assert_eq!(Loader::backend_name(), "preloaded");
    }

    /// ❗ THE DEFAULT BACKEND MUST DECLARE NO WASM IMPORT.
    ///
    /// This is what `e2e/imports_test.go`'s pinned 28 imports rest on, and it is
    /// otherwise unguarded on this side: a backend is selected by `cfg`, so
    /// adding an `extern` to the wrong module would not fail any build -- it
    /// would add an import to the artifact a stock runwasi shim loads, and
    /// nothing there supplies it, so the module would fail to INSTANTIATE
    /// before running an instruction.
    ///
    /// A source-level check because the real one cannot run here: proving it on
    /// the ARTIFACT needs `//runtime:loader_exclusion_test` reading a linked
    /// archive, and that needs a `load-hosted` archive target that does not
    /// exist yet. This is the half that can be checked without Bazel, and it
    /// checks the half that actually goes wrong.
    ///
    /// Precedent: `TestBrkStartMatchesTheRuntime` reads Rust source from Go for
    /// the same reason -- the constant has no other single home.
    #[test]
    fn the_default_backend_declares_no_import() {
        let src = include_str!("preloaded.rs");
        for needle in ["extern \"C\"", "wasm_import_module", "#[link"] {
            assert!(
                !src.contains(needle),
                "preloaded.rs contains {needle:?}: the default profile would gain a \
                 host import, and a stock runwasi shim supplies none"
            );
        }

        // The CONTROL. Without it this passes if `preloaded.rs` is empty, if the
        // path is wrong, or if the needles stop matching how an import is
        // spelled -- and it is the hosted backend that tells us they still do.
        let hosted = include_str!("hosted.rs");
        assert!(
            hosted.contains("wasm_import_module") && hosted.contains("extern \"C\""),
            "hosted.rs no longer declares an import the way this test looks for one, \
             so the assertions above prove nothing"
        );
    }

    /// A backend must answer for a unit it was asked about. Trivial for
    /// `Preloaded`, and the point is that it is stated ONCE against the trait so
    /// every future backend inherits the expectation.
    #[test]
    fn a_backend_answers_every_request() {
        let mut l = Loader::default();
        for idx in [0usize, 1, 7, 4096] {
            match l.request(idx, b"unit_hash") {
                LoadOutcome::Ready | LoadOutcome::Failed(_) => {}
                LoadOutcome::Pending => {
                    panic!("preloaded returned Pending, which nothing is wired to handle")
                }
            }
        }
    }
}
