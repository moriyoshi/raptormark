//! Program registry lookup: maps guest paths to translated-program indices.
//!
//! The registry table (`ecv_programs`) names each program by its content hash;
//! the exec map (`internal/link` writes it into the sidecar at [`EXEC_PATH`])
//! lists every in-scope guest path with the hash of the program that serves it.
//! Combined, they resolve a path an entrypoint or execve names to a program.

use crate::abi::EcvProgram;
// Ungated, so it survives every flag being off -- which is the point: this module's
// one message says the sidecar and the linked registry disagree, and a guest that
// silently runs program 0 under the right argv is the failure it exists to name.
// See the correction at the top of `context.rs` for why an unconditional notice can
// go through `trace` at all.
use crate::trace::ecv_warn;
use crate::vfs::Vfs;
use std::collections::HashMap;

/// Well-known location of the exec map inside the rfs sidecar.
pub const EXEC_PATH: &[u8] = b"/.raptormark/exec";

const MAGIC: &[u8] = b"RMEXEC01";

pub struct Programs {
    regs: Vec<&'static EcvProgram>,
    /// ⚠️ PATH -> HASH, not path -> index, and that is a correctness
    /// requirement rather than a style choice.
    ///
    /// This was `HashMap<Vec<u8>, usize>`, resolved at construction. Under a
    /// host-driven loader a program's descriptor lives inside its side module,
    /// so it is not registered when this is built -- and resolving to an index
    /// here DROPPED exactly the entries that deferred loading exists to serve.
    /// `execve` to one then returned ENOEXEC, and startup shouted the
    /// "WRONG PROGRAM" warning about what is, under `hosted`, the normal case.
    ///
    /// `crate::dlmap` was written this way from the start and its doc comment
    /// gives the reason; the exec map kept the old shape because nothing had
    /// exercised execve on a split artifact. Same defect, one map later.
    by_path: HashMap<Vec<u8>, Vec<u8>>,
}

impl Programs {
    /// Builds the resolver from the registry and the optional exec-map bytes.
    /// With no exec map (single-program module), `by_path` is empty and callers
    /// fall back to program 0.
    pub fn load(regs: Vec<&'static EcvProgram>, exec_map: Option<&[u8]>) -> Programs {
        let known: Vec<&[u8]> = regs.iter().map(|p| p.name_bytes()).collect();
        let mut by_path = HashMap::new();
        // An entry naming a hash the registry does not contain used to be
        // dropped in silence, and the path then fell back to program 0 -- so the
        // guest ran the WRONG PROGRAM under the right argv, and the symptom
        // surfaced wherever that program first disagreed with its arguments
        // (`unrecognized configuration parameter "username"` when postgres ran
        // initdb's flags; `dash: invalid argument` when dash ran postgres's).
        //
        // It has caused four separate incidents -- a stale sidecar after a
        // re-lift, a non-canonical guest path, a sidecar built with a different
        // builder tag, and a change to how module ids are derived. Every one of
        // them was minutes of confusion that this one line answers.
        //
        // NOT gated on the debug flag, and not fatal: a mismatch is always a
        // build defect worth saying out loud, but the module may still be able
        // to run whatever it does have.
        let mut unknown: Vec<(Vec<u8>, Vec<u8>)> = Vec::new();
        if let Some(b) = exec_map {
            for (path, hash) in parse(b, MAGIC) {
                if !known.contains(&hash.as_slice()) {
                    unknown.push((path.clone(), hash.clone()));
                }
                // Recorded EITHER WAY. An entry naming a unit that is not
                // registered yet is what a lazily-placed side module looks like
                // at startup; dropping it is what made `execve` to one
                // impossible. Resolution happens at execve time instead.
                by_path.insert(path, hash);
            }
        }
        if !unknown.is_empty() {
            // ⚠️ WORDING, and it changed for a reason. This used to say the
            // paths "fall back to program 0, so the guest runs the WRONG
            // PROGRAM" -- true under a `preloaded` build, where every program is
            // linked in and an unknown hash really is a build defect. Under a
            // `hosted` build a program is registered when the host places it, so
            // the same observation is the NORMAL state at startup, and the old
            // text made a correct deferred load look like a corrupt sidecar.
            ecv_warn!(
                ecvisor,
                "note: {} exec-map {} naming a program not registered yet. \
                 Expected under a host-driven loader, which registers a program \
                 when it places its side module. Under the default build every \
                 program is linked in, so this instead means the sidecar and the \
                 linked registry disagree -- rebuild the sidecar from the \
                 registry that was actually linked.",
                unknown.len(),
                if unknown.len() == 1 {
                    "entry is"
                } else {
                    "entries are"
                }
            );
            for (path, hash) in unknown.iter().take(8) {
                ecv_warn!(
                    ecvisor,
                    "  {} -> {} (not in the registry at startup)",
                    String::from_utf8_lossy(path),
                    String::from_utf8_lossy(hash)
                );
            }
            ecv_warn!(
                ecvisor,
                "  registry has: {}",
                regs.iter()
                    .map(|p| String::from_utf8_lossy(p.name_bytes()).into_owned())
                    .collect::<Vec<_>>()
                    .join(", ")
            );
        }
        Programs { regs, by_path }
    }

    pub fn len(&self) -> usize {
        self.regs.len()
    }

    /// Registry indices that are LIBRARY units: programs with NO exec-map path, so
    /// they are never an entry/execve target but are merged into whichever program
    /// runs (the shared-libc superset unit — one lifted libc for the whole userland,
    /// see .agents/docs/PERF.md Lever B). Empty when there is no exec map at all
    /// (a single-program module, or the legacy RAPTORMARK_SHARED_UNITS spike which
    /// merges everything-but-entry instead), so this never mis-classifies the
    /// full-fuse execve model (every unit there has an exec-map path -> no libraries).
    pub fn library_indices(&self) -> Vec<usize> {
        if self.by_path.is_empty() {
            return Vec::new();
        }
        // Through `index_of_name`, because `by_path` holds hashes now. A path
        // whose unit is not registered yet contributes no index, which is
        // correct: it cannot be a library unit if it is not in the registry.
        let programs: std::collections::HashSet<usize> = self
            .by_path
            .values()
            .filter_map(|h| self.index_of_name(h))
            .collect();
        (0..self.regs.len())
            .filter(|i| !programs.contains(i))
            .collect()
    }

    /// The registry as a slice, for consumers that need to key on unit NAMES
    /// rather than resolve a path -- `crate::dlmap::DlMap::load` is the one.
    pub fn regs(&self) -> &[&'static EcvProgram] {
        &self.regs
    }

    /// Appends a unit registered AFTER `_start`, returning its index.
    ///
    /// The registry is otherwise fixed at construction. This is the one way it
    /// grows, and only a host-driven loader uses it: a side module's descriptor
    /// address does not exist until the module is instantiated, so a lazily
    /// placed unit cannot be registered before the guest starts.
    ///
    /// `by_path` is deliberately NOT updated. That map is the EXEC map -- which
    /// guest paths are entry points -- and a dlopen'd plugin is not one. The
    /// dlopen map keys on the hash and resolves through `index_of_name`, so it
    /// picks this up with no help.
    pub fn push_late(&mut self, p: &'static EcvProgram) -> usize {
        self.regs.push(p);
        self.regs.len() - 1
    }

    /// The registry index of the unit with this content hash, if it is
    /// registered NOW.
    ///
    /// "now" is load-bearing. A unit placed by a host mid-run is registered
    /// after `_start`, so a caller that resolved a name to an index at
    /// construction would have missed it -- see `crate::dlmap`, which keys on
    /// the hash for exactly this reason.
    pub fn index_of_name(&self, name: &[u8]) -> Option<usize> {
        self.regs.iter().position(|p| p.name_bytes() == name)
    }

    pub fn get(&self, i: usize) -> &'static EcvProgram {
        self.regs[i]
    }

    /// Resolves a guest path to a program index: exact match, else via VFS
    /// symlink resolution, else None.
    pub fn resolve(&self, vfs: &Vfs, cwd: &[u8], path: &[u8]) -> Option<usize> {
        // Against the registry as it is NOW, so a program the host registered
        // after `_start` resolves. See the note on `by_path`.
        self.hash_for(vfs, cwd, path)
            .and_then(|h| self.index_of_name(&h))
    }

    /// The program HASH this path names, whether or not it is registered.
    ///
    /// The exec-map twin of `DlMap::hash_for`, and it separates the two cases
    /// `resolve`'s `None` conflates: a path this module was never built with
    /// (ENOENT/ENOEXEC) versus one whose side module the host has simply not
    /// placed yet (ask the host, park, retry).
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

/// Parses a sidecar path map: `magic`, u32 count, then count × (u32 pathlen,
/// path, u32 hashlen, hash).
///
/// Shared with the DLOPEN map (`crate::dlmap`), whose encoding is byte-identical
/// apart from its magic -- `internal/link.TestDlMapEncodesLikeTheExecMap` pins
/// that on the producing side. A second copy of this here would be a second
/// thing to keep in step, and the copy nobody exercises is the one that rots.
pub(crate) fn parse(b: &[u8], magic: &[u8]) -> Vec<(Vec<u8>, Vec<u8>)> {
    let mut out = Vec::new();
    if b.len() < magic.len() + 4 || &b[..magic.len()] != magic {
        return out;
    }
    let mut pos = magic.len();
    let count = match read_u32(b, &mut pos) {
        Some(c) => c,
        None => return out,
    };
    for _ in 0..count {
        let path = match read_bytes(b, &mut pos) {
            Some(p) => p,
            None => break,
        };
        let hash = match read_bytes(b, &mut pos) {
            Some(h) => h,
            None => break,
        };
        out.push((path, hash));
    }
    out
}

fn read_u32(b: &[u8], pos: &mut usize) -> Option<u32> {
    let end = pos.checked_add(4)?;
    if end > b.len() {
        return None;
    }
    let v = u32::from_le_bytes(b[*pos..end].try_into().ok()?);
    *pos = end;
    Some(v)
}

fn read_bytes(b: &[u8], pos: &mut usize) -> Option<Vec<u8>> {
    let n = read_u32(b, pos)? as usize;
    let end = pos.checked_add(n)?;
    if end > b.len() {
        return None;
    }
    let v = b[*pos..end].to_vec();
    *pos = end;
    Some(v)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn encode(entries: &[(&[u8], &[u8])]) -> Vec<u8> {
        let mut b = Vec::new();
        b.extend_from_slice(MAGIC);
        b.extend_from_slice(&(entries.len() as u32).to_le_bytes());
        for (p, h) in entries {
            b.extend_from_slice(&(p.len() as u32).to_le_bytes());
            b.extend_from_slice(p);
            b.extend_from_slice(&(h.len() as u32).to_le_bytes());
            b.extend_from_slice(h);
        }
        b
    }

    #[test]
    fn parses_entries() {
        let b = encode(&[(b"/bin/ls", b"hashA"), (b"/bin/sh", b"hashB")]);
        let got = parse(&b, MAGIC);
        assert_eq!(got.len(), 2);
        assert_eq!(got[0], (b"/bin/ls".to_vec(), b"hashA".to_vec()));
    }

    fn program(name: &'static [u8]) -> &'static EcvProgram {
        Box::leak(Box::new(EcvProgram::for_test(name)))
    }

    /// ❗ AN EXEC TARGET REGISTERED AFTER THE MAP WAS BUILT MUST RESOLVE.
    ///
    /// Under a host-driven loader a program's `EcvProgram` descriptor lives
    /// inside its side module, so it is not in the registry when `load` runs.
    /// This map used to resolve path -> index at construction and DROP such
    /// entries, which made `execve` to a deferred program return ENOEXEC --
    /// while `library_indices` and the startup warning both treated the normal
    /// case as a corrupt sidecar.
    ///
    /// `crate::dlmap` was built hash-keyed from the start for exactly this; the
    /// exec map kept the old shape because nothing had exercised execve on a
    /// split artifact.
    #[test]
    fn an_exec_target_registered_after_the_map_resolves() {
        let vfs = Vfs::new(None);
        let early = vec![program(b"main_0000\0")];
        let m = Programs::load(
            early,
            Some(&encode(&[
                (b"/usr/bin/main", b"main_0000"),
                (b"/usr/bin/late", b"late_beef"),
            ])),
        );

        // Before the host places it: no index, and that is CORRECT.
        assert_eq!(m.resolve(&vfs, b"/", b"/usr/bin/late"), None);
        // But the map must still know which program the build shipped for it,
        // or execve has nothing to ask the host for.
        assert_eq!(
            m.hash_for(&vfs, b"/", b"/usr/bin/late").as_deref(),
            Some(&b"late_beef"[..]),
            "the entry was dropped at construction, so execve to a deferred \
             program can only ever be ENOEXEC"
        );
        // The already-registered one still resolves, or this proves nothing.
        assert_eq!(m.resolve(&vfs, b"/", b"/usr/bin/main"), Some(0));

        // The host places and registers it; the SAME map now resolves it.
        let after = Programs::load(
            vec![program(b"main_0000\0"), program(b"late_beef\0")],
            Some(&encode(&[
                (b"/usr/bin/main", b"main_0000"),
                (b"/usr/bin/late", b"late_beef"),
            ])),
        );
        assert_eq!(after.resolve(&vfs, b"/", b"/usr/bin/late"), Some(1));

        // A path the map genuinely lacks stays absent in BOTH, or `hash_for`
        // would just be a way of answering anything.
        assert_eq!(m.hash_for(&vfs, b"/", b"/usr/bin/nope"), None);
        assert_eq!(m.resolve(&vfs, b"/", b"/usr/bin/nope"), None);
    }

    /// A deferred exec target must NOT be classified as a library unit.
    ///
    /// `library_indices` reads `by_path`, which now holds hashes. A program that
    /// is not registered yet contributes no index -- correct, since it cannot be
    /// a library unit if it is not in the registry -- and the registered ones
    /// must still be excluded exactly as before.
    #[test]
    fn library_indices_ignore_a_not_yet_registered_exec_target() {
        let regs = vec![program(b"main_0000\0"), program(b"libs_1111\0")];
        let m = Programs::load(
            regs,
            Some(&encode(&[
                (b"/usr/bin/main", b"main_0000"),
                (b"/usr/bin/late", b"late_beef"), // not registered
            ])),
        );
        assert_eq!(
            m.library_indices(),
            vec![1],
            "index 0 is an exec target and index 1 has no exec path, so only 1 \
             is a library unit -- the unregistered entry must not change that"
        );
    }

    #[test]
    fn load_resolves_a_path_whose_hash_is_in_the_registry() {
        let regs = vec![program(b"dash_fused_aaaa\0"), program(b"psql_fused_bbbb\0")];
        let map = encode(&[
            (b"/bin/dash", b"dash_fused_aaaa"),
            (b"/bin/psql", b"psql_fused_bbbb"),
        ]);
        let p = Programs::load(regs, Some(&map));
        let vfs = crate::vfs::Vfs::new(None);
        assert_eq!(p.resolve(&vfs, b"/", b"/bin/dash"), Some(0));
        assert_eq!(p.resolve(&vfs, b"/", b"/bin/psql"), Some(1));
    }

    // The silent-failure path, and the reason `for_test` exists. An entry naming
    // a hash the registry does not contain is DROPPED, and the caller then falls
    // back to program 0 -- so the guest runs the wrong program under the right
    // argv. Four incidents came through here. `resolve` must return None so the
    // caller's fallback is a decision it makes, not one this table made for it.
    #[test]
    fn an_entry_naming_an_unknown_program_never_resolves() {
        let regs = vec![program(b"dash_fused_aaaa\0")];
        let map = encode(&[
            (b"/bin/dash", b"dash_fused_aaaa"),
            (b"/bin/initdb", b"initdb_fused_STALE"),
        ]);
        let p = Programs::load(regs, Some(&map));
        let vfs = crate::vfs::Vfs::new(None);
        assert_eq!(p.resolve(&vfs, b"/", b"/bin/dash"), Some(0));
        assert_eq!(
            p.resolve(&vfs, b"/", b"/bin/initdb"),
            None,
            "a stale hash must not resolve to SOME program"
        );

        // ⚠️ RENAMED from `load_drops_an_entry_naming_an_unknown_program`,
        // because `load` no longer drops it -- and the distinction matters.
        //
        // The entry is now RETAINED so a host-driven loader can be asked for it
        // (see `hash_for`); what must never happen is it resolving to some other
        // program, which is the four-incident failure the assertion above holds.
        // The two are independent, and only the second was ever the point.
        assert_eq!(
            p.hash_for(&vfs, b"/", b"/bin/initdb").as_deref(),
            Some(&b"initdb_fused_STALE"[..]),
            "the entry must be retained for a host to be asked about, even though \
             it resolves to nothing"
        );
    }

    #[test]
    fn with_no_exec_map_nothing_resolves_and_there_are_no_library_units() {
        // A single-program module. `library_indices` keys off an empty path map
        // and must not classify the only program as a library unit, which would
        // merge away the thing that runs.
        let regs = vec![program(b"solo_fused_cccc\0")];
        let p = Programs::load(regs, None);
        let vfs = crate::vfs::Vfs::new(None);
        assert_eq!(p.resolve(&vfs, b"/", b"/bin/anything"), None);
        assert!(p.library_indices().is_empty());
    }

    #[test]
    fn a_program_with_no_exec_map_path_is_a_library_unit() {
        let regs = vec![program(b"main_fused_dddd\0"), program(b"libc_fused_eeee\0")];
        let map = encode(&[(b"/bin/main", b"main_fused_dddd")]);
        let p = Programs::load(regs, Some(&map));
        assert_eq!(p.library_indices(), vec![1]);
    }

    #[test]
    fn a_descriptor_without_program_headers_reports_no_writable_segments() {
        // Guards the null-check ORDER in `writable_loads`/`tls_phdr`: they used
        // to dereference `e_phent_p` and `e_phnum_p` before testing `e_ph`, so
        // this call faulted rather than returning empty.
        let p = program(b"noph_fused_ffff\0");
        assert!(p.writable_loads().is_empty());
        assert!(p.tls_phdr().is_none());
        // Also goes through find_data_section, which dereferenced its own count
        // pointer before checking it.
        assert_eq!(p.tls_extent_above_tp(), 0);
    }

    #[test]
    fn rejects_bad_magic() {
        assert!(parse(b"XXXX", MAGIC).is_empty());
    }
}
