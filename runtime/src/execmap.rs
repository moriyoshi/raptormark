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
    by_path: HashMap<Vec<u8>, usize>,
}

impl Programs {
    /// Builds the resolver from the registry and the optional exec-map bytes.
    /// With no exec map (single-program module), `by_path` is empty and callers
    /// fall back to program 0.
    pub fn load(regs: Vec<&'static EcvProgram>, exec_map: Option<&[u8]>) -> Programs {
        let mut by_hash: HashMap<Vec<u8>, usize> = HashMap::new();
        for (i, p) in regs.iter().enumerate() {
            by_hash.insert(p.name_bytes().to_vec(), i);
        }
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
            for (path, hash) in parse(b) {
                match by_hash.get(&hash) {
                    Some(&i) => {
                        by_path.insert(path, i);
                    }
                    None => unknown.push((path, hash)),
                }
            }
        }
        if !unknown.is_empty() {
            ecv_warn!(
                ecvisor,
                "WARNING: {} exec-map {} naming a program this module \
                 does not contain. Those paths fall back to program 0, so the guest \
                 runs the WRONG PROGRAM under the right argv. The sidecar and the \
                 linked registry disagree -- rebuild the sidecar from the registry \
                 that was actually linked.",
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
                    "  {} -> {} (not in the registry)",
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
        let programs: std::collections::HashSet<usize> = self.by_path.values().copied().collect();
        (0..self.regs.len())
            .filter(|i| !programs.contains(i))
            .collect()
    }

    pub fn get(&self, i: usize) -> &'static EcvProgram {
        self.regs[i]
    }

    /// Resolves a guest path to a program index: exact match, else via VFS
    /// symlink resolution, else None.
    pub fn resolve(&self, vfs: &Vfs, cwd: &[u8], path: &[u8]) -> Option<usize> {
        if let Some(&i) = self.by_path.get(path) {
            return Some(i);
        }
        if let Some(r) = vfs.resolve(cwd, path, true) {
            if let Some(&i) = self.by_path.get(&r.path) {
                return Some(i);
            }
        }
        None
    }
}

/// Parses the exec map: magic, u32 count, then count × (u32 pathlen, path,
/// u32 hashlen, hash).
fn parse(b: &[u8]) -> Vec<(Vec<u8>, Vec<u8>)> {
    let mut out = Vec::new();
    if b.len() < MAGIC.len() + 4 || &b[..MAGIC.len()] != MAGIC {
        return out;
    }
    let mut pos = MAGIC.len();
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
        let got = parse(&b);
        assert_eq!(got.len(), 2);
        assert_eq!(got[0], (b"/bin/ls".to_vec(), b"hashA".to_vec()));
    }

    fn program(name: &'static [u8]) -> &'static EcvProgram {
        Box::leak(Box::new(EcvProgram::for_test(name)))
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
    fn load_drops_an_entry_naming_an_unknown_program() {
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
        assert!(parse(b"XXXX").is_empty());
    }
}
