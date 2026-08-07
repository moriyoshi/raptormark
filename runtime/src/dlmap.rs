//! The DLOPEN map: guest `.so` path -> the index of the UNIT that serves it.
//!
//! It is to `dlopen` what [`crate::execmap`] is to `execve`. The sidecar carries
//! both, at [`EXEC_PATH`](crate::execmap::EXEC_PATH) and [`DL_PATH`], and
//! `internal/link` writes them with the same encoding under different magics so
//! one parser serves both.
//!
//! # Why a dlopen of an unknown path must FAIL
//!
//! The exec map's failure mode is a fallback: an unresolved execve path runs
//! program 0, which `execmap.rs` records as having caused four separate
//! incidents. There is no equivalent temptation here and there must not be one.
//! A `dlopen` naming a plugin this module does not contain has exactly one
//! honest answer -- NULL with an error -- because that is what a real loader
//! does for an absent object, and because the alternative was what shipped
//! before: a non-null sentinel handle whose every `dlsym` then returned NULL,
//! which postgres reports as `missing magic block`, a version mismatch by
//! appearance and an absent object in fact.

use crate::abi::EcvProgram;
use crate::trace::ecv_warn;
use crate::vfs::Vfs;
use std::collections::HashMap;

/// Well-known location of the dlopen map inside the rfs sidecar. Must match
/// `internal/link.DlPath` and `internal/rootfs.DlPath`.
pub const DL_PATH: &[u8] = b"/.raptormark/dlopen";

const MAGIC: &[u8] = b"RMDLOP01";

/// Resolves a guest `.so` path to the registry index of its unit.
///
/// ⚠️ IT STORES THE HASH, NOT THE INDEX, and that is a correctness requirement
/// rather than a convenience. A `hosted` backend places a side module MID-RUN
/// and can only call `ecv_register_program` once it has instantiated it, so a
/// lazily-loaded unit is not in the registry when this map is built. Resolving
/// to an index here would drop exactly the entries that dynamic loading exists
/// to serve, and their `dlopen` could never succeed -- while the map looked
/// perfectly well-formed.
pub struct DlMap {
    by_path: HashMap<Vec<u8>, Vec<u8>>,
}

impl DlMap {
    /// Builds the resolver from the registry and the optional sidecar bytes.
    ///
    /// With no map (an image with no dlopen-able units) `by_path` is empty and
    /// every `dlopen` of a unit fails, which is correct: there are none.
    pub fn load(regs: &[&'static EcvProgram], bytes: Option<&[u8]>) -> DlMap {
        let known: Vec<&[u8]> = regs.iter().map(|p| p.name_bytes()).collect();
        let mut by_path = HashMap::new();
        let mut unknown: Vec<(Vec<u8>, Vec<u8>)> = Vec::new();
        if let Some(b) = bytes {
            for (path, hash) in crate::execmap::parse(b, MAGIC) {
                if !known.contains(&hash.as_slice()) {
                    unknown.push((path.clone(), hash.clone()));
                }
                // Recorded either way. An entry naming a unit that is not
                // registered YET is exactly what a lazily-placed side module
                // looks like at startup, so dropping it would be wrong; the
                // resolution happens at dlopen time instead, and a name that is
                // still unknown then fails the dlopen with a real `dlerror`.
                by_path.insert(path, hash);
            }
        }
        // NOT gated on a debug flag, and not fatal, for the same reason the exec
        // map's equivalent is not: a mismatch between the sidecar and the linked
        // registry is always a build defect worth saying out loud, and the
        // module may still run whatever it does have. The symptom this replaces
        // is a `dlopen` that fails for a plugin the build believes it shipped.
        // ⚠️ INFORMATIONAL, not an error, and the wording matters. Under a
        // `preloaded` build every unit is registered before this runs, so an
        // unknown hash IS a build defect worth saying out loud. Under a `hosted`
        // build a unit is registered when the host places it, so the same
        // observation is the normal case -- which is why this says "not
        // registered yet" rather than "this module does not contain".
        if !unknown.is_empty() {
            ecv_warn!(
                ecvisor,
                "note: {} dlopen-map entr{} naming a unit not registered yet. \
                 Expected under a host-driven loader; under the default build it \
                 means the sidecar and the linked registry disagree, so rebuild \
                 the sidecar from the registry",
                unknown.len(),
                if unknown.len() == 1 { "y" } else { "ies" }
            );
            for (path, hash) in &unknown {
                ecv_warn!(
                    ecvisor,
                    "  {} -> {} (not in the registry at startup)",
                    String::from_utf8_lossy(path),
                    String::from_utf8_lossy(hash)
                );
            }
        }
        DlMap { by_path }
    }

    /// Every registry index this map can resolve to, right now.
    ///
    /// Used to keep dlopen-able units OUT of the eager library merge. See
    /// `EcvContext::new`.
    pub fn referenced_units(&self, progs: &crate::execmap::Programs) -> Vec<usize> {
        let mut out: Vec<usize> = self
            .by_path
            .values()
            .filter_map(|h| progs.index_of_name(h))
            .collect();
        out.sort_unstable();
        out.dedup();
        out
    }

    pub fn is_empty(&self) -> bool {
        self.by_path.is_empty()
    }

    pub fn len(&self) -> usize {
        self.by_path.len()
    }

    /// Resolves a guest `.so` path to a unit index: exact match, else through
    /// the VFS, else None.
    ///
    /// The VFS retry matters MORE here than for execve. A guest names a plugin
    /// through whatever path its own configuration holds -- postgres builds one
    /// from `$libdir`, python from `sys.path` -- so the spellings are far more
    /// varied than the handful libc spawns through, and a usr-merged Debian
    /// image resolves most of them through at least one symlink.
    pub fn resolve(
        &self,
        vfs: &Vfs,
        cwd: &[u8],
        path: &[u8],
        progs: &crate::execmap::Programs,
    ) -> Option<usize> {
        let hash = self.by_path.get(path).or_else(|| {
            vfs.resolve(cwd, path, true)
                .and_then(|r| self.by_path.get(&r.path))
        })?;
        // Against the registry as it is NOW, so a unit the host registered after
        // `_start` resolves.
        progs.index_of_name(hash)
    }

    /// The unit HASH this path names, whether or not that unit is registered.
    ///
    /// ❗ THE DIFFERENCE FROM `resolve` IS THE WHOLE POINT OF A HOSTED LOADER.
    /// `resolve` answers "which registry index serves this path", and under a
    /// host-driven loader the honest answer before the first `dlopen` is *none*
    /// -- the side module has not been instantiated, so nothing has registered
    /// its descriptor and there is no index to return. Treating that `None` as
    /// "no such plugin" is correct for `preloaded` and exactly backwards here:
    /// it is the state a lazily-placed unit is SUPPOSED to be in.
    ///
    /// This distinguishes the two cases the caller must not conflate:
    ///
    /// * `Some(hash)` -- the build shipped this plugin; ask the host for it.
    /// * `None`       -- the map has no such path; fail the `dlopen` for real.
    ///
    /// Returns an owned hash because the caller needs it across a `&mut self`
    /// borrow of the context to reach the loader.
    pub fn hash_for(&self, vfs: &Vfs, cwd: &[u8], path: &[u8]) -> Option<Vec<u8>> {
        self.by_path
            .get(path)
            .or_else(|| {
                vfs.resolve(cwd, path, true)
                    .and_then(|r| self.by_path.get(&r.path))
            })
            .cloned()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::execmap::Programs;

    /// A registry entry with just a name, which is all these tests need.
    fn program(name: &'static [u8]) -> &'static EcvProgram {
        Box::leak(Box::new(EcvProgram::for_test(name)))
    }

    /// Encodes a map the way `internal/link.DlMap` does.
    fn encode(magic: &[u8], entries: &[(&[u8], &[u8])]) -> Vec<u8> {
        let mut b = magic.to_vec();
        b.extend_from_slice(&(entries.len() as u32).to_le_bytes());
        for (path, hash) in entries {
            b.extend_from_slice(&(path.len() as u32).to_le_bytes());
            b.extend_from_slice(path);
            b.extend_from_slice(&(hash.len() as u32).to_le_bytes());
            b.extend_from_slice(hash);
        }
        b
    }

    #[test]
    fn resolves_a_path_whose_hash_is_in_the_registry() {
        let regs = vec![program(b"pgcrypto_a1b2\0")];
        let m = DlMap::load(
            &regs,
            Some(&encode(MAGIC, &[(b"/pg/pgcrypto.so", b"pgcrypto_a1b2")])),
        );
        let progs = Programs::load(regs, None);
        let vfs = Vfs::new(None);
        assert_eq!(m.resolve(&vfs, b"/", b"/pg/pgcrypto.so", &progs), Some(0));
        assert_eq!(m.resolve(&vfs, b"/", b"/pg/absent.so", &progs), None);
    }

    /// THE bug this map's shape exists to avoid.
    ///
    /// A `hosted` backend places a side module mid-run and can only register it
    /// once instantiated, so a lazily-loaded unit is NOT in the registry when
    /// the map is built. An index-resolving map would have dropped that entry at
    /// startup -- silently, and while looking perfectly well-formed -- so the one
    /// kind of plugin dynamic loading exists to serve could never be dlopen'd.
    #[test]
    fn resolves_a_unit_registered_after_the_map_was_built() {
        // At load time the registry holds ONLY the main program.
        let early = vec![program(b"main_0000\0")];
        let m = DlMap::load(
            &early,
            Some(&encode(MAGIC, &[(b"/pg/late.so", b"late_beef")])),
        );
        let vfs = Vfs::new(None);

        // Against that registry it does not resolve, which is correct.
        let before = Programs::load(early, None);
        assert_eq!(m.resolve(&vfs, b"/", b"/pg/late.so", &before), None);

        // The host places and registers it; now the SAME map resolves it.
        let after = Programs::load(vec![program(b"main_0000\0"), program(b"late_beef\0")], None);
        assert_eq!(m.resolve(&vfs, b"/", b"/pg/late.so", &after), Some(1));
    }

    /// `hash_for` must answer for a unit that is NOT registered, which is the
    /// state every lazily-placed unit is in before its first `dlopen`.
    ///
    /// ⚠️ Paired with a `resolve` on the SAME map and the SAME registry that
    /// returns None. Without that pairing this would pass on an implementation
    /// where the two are the same function, which is the bug it exists to
    /// prevent -- `resolve`'s None is what a hosted `dlopen` must NOT read as
    /// "no such plugin".
    #[test]
    fn hash_for_answers_before_the_unit_is_registered() {
        let vfs = Vfs::new(None);
        let early = vec![program(b"main_0000\0")];
        let m = DlMap::load(
            &early,
            Some(&encode(MAGIC, &[(b"/pg/late.so", b"late_beef")])),
        );
        let progs = Programs::load(early, None);

        assert_eq!(
            m.resolve(&vfs, b"/", b"/pg/late.so", &progs),
            None,
            "the unit is not registered, so resolve must not invent an index"
        );
        assert_eq!(
            m.hash_for(&vfs, b"/", b"/pg/late.so").as_deref(),
            Some(&b"late_beef"[..]),
            "hash_for must still name the unit the build shipped, or a hosted \
             dlopen has nothing to ask the host for"
        );
        // A path the map genuinely does not have stays absent in BOTH.
        assert_eq!(m.hash_for(&vfs, b"/", b"/pg/absent.so"), None);
    }

    /// An entry naming a hash NO registry ever has must stay unresolvable, or
    /// the lazy lookup above would just be a way of answering anything.
    #[test]
    fn an_unknown_hash_never_resolves() {
        let regs = vec![program(b"known_1111\0")];
        let m = DlMap::load(
            &regs,
            Some(&encode(
                MAGIC,
                &[(b"/pg/a.so", b"known_1111"), (b"/pg/b.so", b"absent_9999")],
            )),
        );
        let progs = Programs::load(regs, None);
        let vfs = Vfs::new(None);
        assert_eq!(m.resolve(&vfs, b"/", b"/pg/a.so", &progs), Some(0));
        assert_eq!(m.resolve(&vfs, b"/", b"/pg/b.so", &progs), None);
    }

    /// ⚠️ The two sidecar maps are byte-identical apart from the magic, so the
    /// ONLY thing stopping one being read as the other is the magic check. An
    /// exec map read as a dlopen map would map executable paths to units.
    #[test]
    fn refuses_an_exec_map() {
        let regs = vec![program(b"h1\0")];
        let exec = encode(b"RMEXEC01", &[(b"/bin/sh", b"h1")]);
        assert!(
            DlMap::load(&regs, Some(&exec)).is_empty(),
            "an EXEC map was accepted as a dlopen map"
        );
        // The control: the same entries under the right magic DO load, so the
        // assertion above is about the magic and not about the encoder.
        let dl = encode(MAGIC, &[(b"/bin/sh", b"h1")]);
        assert_eq!(DlMap::load(&regs, Some(&dl)).len(), 1);
    }

    #[test]
    fn no_map_means_no_units() {
        let regs = vec![program(b"h1\0")];
        assert!(DlMap::load(&regs, None).is_empty());
        assert!(DlMap::load(&regs, Some(b"")).is_empty());
    }
}
