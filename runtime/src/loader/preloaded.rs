//! The backend that ships: every unit's code is already in the module, and only
//! the MERGE was deferred.
//!
//! # Why this is the default, and why it is not a consolation prize
//!
//! The flat artifact physically cannot gain code after link time. Measured over
//! the real modules in this tree, every one declares
//! `__indirect_function_table` with `min == max` -- bash 6,694, nginx 21,902,
//! postgres 155,275 -- it is not exported, and nothing calls `table.grow`;
//! `--growable-table` appears only in `supervisorLinkArgs`. So on the stock
//! runwasi path "dynamic" can only ever mean deferring the merge of code that
//! was already linked in.
//!
//! That still delivers the thing this work is for. postgres:17's 79 extensions
//! each define `Pg_magic_func`, `_PG_init` and `pg_finfo_*`; before the split
//! they collapsed into one flat `.ecv.dlsyms` and first-definition-wins bound
//! the wrong one silently. With each fused as its own unit and `dlsym` scoped to
//! the handle, they no longer collide -- on the artifact that ships, with no
//! embedder and no new import, and testable in the existing E2E suite.
//!
//! What it does NOT deliver is paying only for the plugins you load: every
//! unit's code is in the module whether or not the guest ever dlopens it. That
//! is the `hosted` / `emscripten` / `wasix` backends' job.

use super::{LoadOutcome, LoaderBackend};

/// Loads nothing, because there is nothing to load.
#[derive(Default)]
pub struct Preloaded;

impl LoaderBackend for Preloaded {
    /// Always [`LoadOutcome::Ready`].
    ///
    /// ⚠️ It does NOT validate `idx`. Whether a unit exists is a question about
    /// the REGISTRY, and the caller has already answered it -- `dlopen` resolved
    /// the path through the dlopen map and `execve` through the exec map, both
    /// of which yield an index or nothing. Re-checking here would put the same
    /// rule in two places, and the copy that is wrong is the one nobody
    /// exercises.
    fn request(&mut self, _idx: usize, _name: &[u8]) -> LoadOutcome {
        LoadOutcome::Ready
    }

    fn backend_name() -> &'static str {
        "preloaded"
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The property the flat artifact depends on: no request can fail, because
    /// the code is already there. A `Failed` here would surface as a `dlopen`
    /// that returns NULL for a plugin the module demonstrably contains.
    #[test]
    fn every_request_is_ready() {
        let mut p = Preloaded;
        assert_eq!(p.request(0, b"a"), LoadOutcome::Ready);
        assert_eq!(p.request(usize::MAX, b""), LoadOutcome::Ready);
    }
}
