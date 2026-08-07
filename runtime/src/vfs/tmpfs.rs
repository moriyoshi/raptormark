//! In-memory read-write layer: the upper layer of the default overlay. Holds
//! files created/modified at runtime plus whiteout markers that hide lower
//! entries (overlayfs semantics). Directories are implicit — a path is a
//! directory if any stored file path has it as a prefix, or it was created via
//! `mkdir`.

use super::{Meta, NodeKind};
use std::collections::{HashMap, HashSet};

pub struct Tmpfs {
    files: HashMap<Vec<u8>, Vec<u8>>,
    dirs: HashSet<Vec<u8>>,
    /// Paths hidden from the lower layer.
    whiteouts: HashSet<Vec<u8>>,
    /// Paths bound by an AF_UNIX `bind`. Held separately from `files` because a
    /// socket node has no contents and must never be openable as one: a
    /// zero-length regular file would let a guest `open()` the rendezvous path
    /// and read EOF instead of getting ENXIO.
    socks: HashSet<Vec<u8>>,
    /// Permission bits set explicitly by chmod, keyed by absolute path.
    ///
    /// Separate from the nodes because a chmod can target a path that lives
    /// only in the LOWER layer -- which is the common case: PostgreSQL's initdb
    /// chmods the PGDATA directory that came from the image. Recording an
    /// override rather than copying the node up keeps the layers unchanged and
    /// lets `Vfs::stat_exact` apply it whichever layer answered.
    modes: HashMap<Vec<u8>, u32>,
}

impl Tmpfs {
    pub fn new() -> Tmpfs {
        let mut dirs = HashSet::new();
        dirs.insert(b"/".to_vec());
        Tmpfs {
            files: HashMap::new(),
            dirs,
            whiteouts: HashSet::new(),
            socks: HashSet::new(),
            modes: HashMap::new(),
        }
    }

    // NOTE: the uid/gid below are placeholders and always have been. Nothing
    // reads them: `sys_newfstatat` and `sys_fstat` fill `st_uid`/`st_gid` from
    // `ctx.uid`/`ctx.gid`, deliberately and for the reason recorded there --
    // the rootfs preserves no host ownership, so a guest checking
    // `st_uid == geteuid()` would fail whenever it runs as non-root. Teaching
    // this layer a real owner therefore changes nothing a guest can observe,
    // and it would put the decision in two places.

    /// Records permission bits for `path`. Only the low 12 bits are kept; the
    /// file type comes from the node itself and is never chmod-able.
    pub fn set_mode(&mut self, path: &[u8], perm: u32) {
        self.modes.insert(path.to_vec(), perm & 0o7777);
    }

    /// Permission bits set by a previous `set_mode`, if any.
    pub fn mode_override(&self, path: &[u8]) -> Option<u32> {
        self.modes.get(path).copied()
    }

    pub fn is_whiteout(&self, path: &[u8]) -> bool {
        self.whiteouts.contains(path)
    }

    /// Exact-path metadata for a node held by this layer, or None if the layer
    /// has nothing at `path` (the caller then consults the lower layer unless
    /// the path is whited out).
    pub fn stat_exact(&self, path: &[u8]) -> Option<Meta> {
        if let Some(content) = self.files.get(path) {
            return Some(Meta {
                kind: NodeKind::File,
                mode: 0o100644,
                uid: 0,
                gid: 0,
                size: content.len() as u64,
                mtime: 0,
                ino: 0,
            });
        }
        if self.socks.contains(path) {
            return Some(Meta {
                kind: NodeKind::Socket,
                // 0o140777: S_IFSOCK plus the mode a fresh bind leaves behind
                // (Linux applies the umask; nothing here has one, and postgres
                // chmods the socket itself right after binding anyway).
                mode: 0o140777,
                uid: 0,
                gid: 0,
                size: 0,
                mtime: 0,
                ino: 0,
            });
        }
        if self.dirs.contains(path) {
            return Some(Meta {
                kind: NodeKind::Dir,
                mode: 0o040755,
                uid: 0,
                gid: 0,
                size: 0,
                mtime: 0,
                ino: 0,
            });
        }
        None
    }

    pub fn read_file(&self, path: &[u8]) -> Option<Vec<u8>> {
        self.files.get(path).cloned()
    }

    pub fn write_file(&mut self, path: &[u8], content: Vec<u8>) {
        self.whiteouts.remove(path);
        self.ensure_parents(path);
        self.files.insert(path.to_vec(), content);
    }

    pub fn mkdir(&mut self, path: &[u8]) {
        self.whiteouts.remove(path);
        self.ensure_parents(path);
        self.dirs.insert(path.to_vec());
    }

    /// `mkdir` that keeps the requested permission bits.
    ///
    /// Plain `mkdir` leaves the node at the placeholder 0o755 that `stat_exact`
    /// hands back, which is right for the parents `ensure_parents` invents and
    /// wrong for a directory the guest asked for by mode. PostgreSQL creates
    /// PGDATA with 0700 and then refuses to start unless it reads back as 0700
    /// or 0750, so discarding the argument turned a supported call into a
    /// startup failure with no syscall error to point at.
    ///
    /// No umask is applied: nothing in the runtime tracks one (`umask` reports a
    /// conventional 022 and ignores its argument), so the requested mode is
    /// recorded verbatim -- the behaviour of a process whose umask is 0.
    pub fn mkdir_with_mode(&mut self, path: &[u8], mode: u32) {
        self.mkdir(path);
        self.set_mode(path, mode);
    }

    /// Creates an AF_UNIX socket node at `path`. Called by `bind`; the caller
    /// has already established that nothing exists there.
    pub fn mksock(&mut self, path: &[u8]) {
        self.whiteouts.remove(path);
        self.ensure_parents(path);
        self.socks.insert(path.to_vec());
    }

    pub fn whiteout(&mut self, path: &[u8]) {
        self.files.remove(path);
        self.dirs.remove(path);
        self.socks.remove(path);
        // The mode override dies with the node. It outlived it while `modes`
        // was only ever written by an explicit chmod and the leak was rare;
        // `mkdir_with_mode` writes one for EVERY created directory, so a
        // removed 0700 directory whose path is reused -- PostgreSQL removes and
        // recreates PGDATA subdirectories -- would hand its mode to whatever
        // took its place.
        self.modes.remove(path);
        self.whiteouts.insert(path.to_vec());
    }

    /// Direct children (name, kind) contributed by this layer for `dir`.
    pub fn readdir(&self, dir: &[u8]) -> Vec<(Vec<u8>, NodeKind)> {
        let prefix = dir_prefix(dir);
        let mut out = Vec::new();
        for (p, _) in &self.files {
            if let Some(name) = child_name(&prefix, p) {
                out.push((name, NodeKind::File));
            }
        }
        for p in &self.dirs {
            if let Some(name) = child_name(&prefix, p) {
                out.push((name, NodeKind::Dir));
            }
        }
        for p in &self.socks {
            if let Some(name) = child_name(&prefix, p) {
                out.push((name, NodeKind::Socket));
            }
        }
        out
    }

    fn ensure_parents(&mut self, path: &[u8]) {
        let mut cur: Vec<u8> = Vec::new();
        for comp in path.split(|&c| c == b'/') {
            if comp.is_empty() {
                continue;
            }
            cur.push(b'/');
            cur.extend_from_slice(comp);
            // Stop before inserting the leaf itself.
            if cur == path {
                break;
            }
            self.dirs.insert(cur.clone());
        }
    }
}

/// Normalizes a directory path to a "/"-terminated prefix for child matching:
/// "/" stays "/", "/etc" becomes "/etc/".
fn dir_prefix(dir: &[u8]) -> Vec<u8> {
    if dir == b"/" {
        return b"/".to_vec();
    }
    let mut p = dir.to_vec();
    p.push(b'/');
    p
}

/// If `path` is an immediate child of `prefix`, returns its final component.
fn child_name(prefix: &[u8], path: &[u8]) -> Option<Vec<u8>> {
    if path.len() <= prefix.len() || &path[..prefix.len()] != prefix {
        return None;
    }
    let rest = &path[prefix.len()..];
    if rest.is_empty() || rest.contains(&b'/') {
        return None;
    }
    Some(rest.to_vec())
}
