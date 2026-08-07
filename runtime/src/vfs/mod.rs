//! Pluggable virtual filesystem. The default mount is an overlay of a
//! read-only compressed rfs sidecar (lower) under an in-memory tmpfs (upper),
//! matching the container overlay model. Cross-layer symlink resolution and
//! path normalization live here; each backend only answers exact-path queries.
//!
//! Squashfs/erofs/passthrough/network backends slot in behind the same
//! coordinator later without touching the syscall layer.

mod rfs;
mod tmpfs;

pub use rfs::Rfs;
pub use tmpfs::Tmpfs;

/// Kernel symlink-follow limit.
const MAX_LINKS: u32 = 40;

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum NodeKind {
    Dir,
    File,
    Symlink,
    /// An AF_UNIX socket bound into the filesystem namespace by `bind`. It has
    /// no contents -- the endpoint itself lives in `EcvContext::unix_listeners`
    /// and this node is only the rendezvous NAME. A distinct kind rather than
    /// an empty file because `S_ISSOCK` is load-bearing: PostgreSQL's
    /// `RemoveSocketFiles`/stale-socket check and `unix_socket_directories`
    /// startup path both stat the path and refuse to unlink something that is
    /// not a socket.
    Socket,
}

#[derive(Clone, Copy, Debug)]
pub struct Meta {
    pub kind: NodeKind,
    pub mode: u32,
    pub uid: u32,
    pub gid: u32,
    pub size: u64,
    pub mtime: u64,
    pub ino: u64,
}

/// A resolved absolute path plus its metadata.
pub struct Resolved {
    pub path: Vec<u8>,
    pub meta: Meta,
}

pub struct Vfs {
    lower: Option<Rfs>,
    upper: Tmpfs,
}

impl Vfs {
    /// Builds the default overlay from the sidecar image bytes. Without a
    /// sidecar (bytes None or unparseable) the VFS is tmpfs-only.
    pub fn new(sidecar: Option<Vec<u8>>) -> Vfs {
        Vfs {
            lower: sidecar.and_then(Rfs::parse),
            upper: Tmpfs::new(),
        }
    }

    pub fn upper_mut(&mut self) -> &mut Tmpfs {
        &mut self.upper
    }

    // --- exact-path (single component, no symlink follow) ----------------

    fn stat_exact(&self, path: &[u8]) -> Option<Meta> {
        self.stat_exact_raw(path).map(|mut m| {
            // A chmod overrides whichever layer supplied the node, including a
            // lower-layer one. Applied here, after the layer rule below, so it
            // also wins over "the lower directory is authoritative" -- that rule
            // exists to stop an implicit copy-up parent's placeholder 0o755 from
            // shadowing the image, not to make a directory's mode immutable.
            // initdb chmods PGDATA to 0700 and PostgreSQL then refuses to start
            // unless it reads back as 0700.
            if let Some(perm) = self.upper.mode_override(path) {
                m.mode = (m.mode & !0o7777) | perm;
            }
            m
        })
    }

    fn stat_exact_raw(&self, path: &[u8]) -> Option<Meta> {
        if self.upper.is_whiteout(path) {
            return None;
        }
        if let Some(m) = self.upper.stat_exact(path) {
            // A directory present in BOTH layers: the lower (image) layer's
            // attributes are authoritative. The upper entry is an implicit
            // copy-up parent -- created by `ensure_parents` when a runtime file
            // is written under it -- carrying a placeholder 0o755; letting that
            // shadow the lower would corrupt a mode-sensitive directory (e.g.
            // PostgreSQL's PGDATA, which must stay 0700). A genuinely new
            // upper-only dir has no lower entry and keeps the upper Meta.
            if m.kind == NodeKind::Dir {
                if let Some(lm) = self.lower.as_ref().and_then(|r| r.stat_exact(path)) {
                    if lm.kind == NodeKind::Dir {
                        return Some(lm);
                    }
                }
            }
            return Some(m);
        }
        self.lower.as_ref().and_then(|r| r.stat_exact(path))
    }

    fn readlink_exact(&self, path: &[u8]) -> Option<Vec<u8>> {
        if self.upper.is_whiteout(path) {
            return None;
        }
        // tmpfs holds no symlinks in this version; go straight to lower.
        self.lower.as_ref().and_then(|r| r.readlink(path))
    }

    // --- coordinator resolution ------------------------------------------

    /// Resolves `path` (made absolute against `cwd`) to a real node, following
    /// symlinks; the final component is followed only when `follow_final`.
    pub fn resolve(&self, cwd: &[u8], path: &[u8], follow_final: bool) -> Option<Resolved> {
        let start = if path.first() == Some(&b'/') {
            path.to_vec()
        } else {
            join(cwd, path)
        };
        let mut pending: Vec<Vec<u8>> = split_rev(&start);
        let mut resolved: Vec<u8> = vec![b'/'];
        let mut links = 0u32;

        while let Some(comp) = pending.pop() {
            match comp.as_slice() {
                b"" | b"." => continue,
                b".." => {
                    truncate_to_parent(&mut resolved);
                    continue;
                }
                _ => {}
            }
            let candidate = join(&resolved, &comp);
            let meta = self.stat_exact(&candidate)?;
            let is_final = pending.iter().all(|c| c.is_empty() || c == b".");
            if meta.kind == NodeKind::Symlink && (!is_final || follow_final) {
                links += 1;
                if links > MAX_LINKS {
                    return None;
                }
                let target = self.readlink_exact(&candidate)?;
                if target.first() == Some(&b'/') {
                    resolved = vec![b'/'];
                }
                for c in split_rev(&target) {
                    pending.push(c);
                }
            } else {
                resolved = candidate;
            }
        }

        let meta = self.stat_exact(&resolved)?;
        Some(Resolved {
            path: resolved,
            meta,
        })
    }

    pub fn read(&self, cwd: &[u8], path: &[u8]) -> Option<Vec<u8>> {
        let r = self.resolve(cwd, path, true)?;
        if r.meta.kind != NodeKind::File {
            return None;
        }
        if let Some(v) = self.upper.read_file(&r.path) {
            return Some(v);
        }
        self.lower.as_ref().and_then(|l| l.read_file(&r.path))
    }

    pub fn readlink(&self, cwd: &[u8], path: &[u8]) -> Option<Vec<u8>> {
        let r = self.resolve(cwd, path, false)?;
        if r.meta.kind != NodeKind::Symlink {
            return None;
        }
        self.readlink_exact(&r.path)
    }

    /// Merged directory listing (name, kind): upper entries shadow lower ones,
    /// whiteouts hide lower entries.
    pub fn readdir(&self, cwd: &[u8], path: &[u8]) -> Option<Vec<(Vec<u8>, NodeKind)>> {
        let r = self.resolve(cwd, path, true)?;
        if r.meta.kind != NodeKind::Dir {
            return None;
        }
        let mut out: Vec<(Vec<u8>, NodeKind)> = Vec::new();
        let mut seen: std::collections::HashSet<Vec<u8>> = std::collections::HashSet::new();
        for (name, kind) in self.upper.readdir(&r.path) {
            seen.insert(name.clone());
            out.push((name, kind));
        }
        if let Some(l) = self.lower.as_ref() {
            if let Some(entries) = l.readdir(&r.path) {
                for (name, kind) in entries {
                    let child = join(&r.path, &name);
                    if self.upper.is_whiteout(&child) || seen.contains(&name) {
                        continue;
                    }
                    out.push((name, kind));
                }
            }
        }
        Some(out)
    }
}

// --- path helpers --------------------------------------------------------

/// Joins base and one-or-more components, keeping a single leading slash.
fn join(base: &[u8], rest: &[u8]) -> Vec<u8> {
    let mut out = base.to_vec();
    if out.last() != Some(&b'/') {
        out.push(b'/');
    }
    for &c in rest {
        out.push(c);
    }
    out
}

/// Splits a path into components in reverse order (so `Vec::pop` yields them
/// left-to-right).
fn split_rev(path: &[u8]) -> Vec<Vec<u8>> {
    let mut v: Vec<Vec<u8>> = path
        .split(|&c| c == b'/')
        .filter(|c| !c.is_empty())
        .map(|c| c.to_vec())
        .collect();
    v.reverse();
    v
}

/// Truncates `resolved` (an absolute path) to its parent, never above "/".
fn truncate_to_parent(resolved: &mut Vec<u8>) {
    while resolved.last() == Some(&b'/') && resolved.len() > 1 {
        resolved.pop();
    }
    while resolved.last() != Some(&b'/') {
        resolved.pop();
    }
    if resolved.len() > 1 {
        resolved.pop();
    }
    if resolved.is_empty() {
        resolved.push(b'/');
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // Builds an rfs image in-process using the same on-disk format the Go
    // writer emits, so the reader + coordinator can be exercised on the host.
    fn rfs_image() -> Vec<u8> {
        // /etc/os-release (file), /bin/busybox (file), /bin/sh -> busybox.
        // Hand-assemble via the documented layout with stored (uncompressed)
        // files to keep the fixture dependency-free.
        let mut names: Vec<u8> = Vec::new();
        let intern = |names: &mut Vec<u8>, s: &[u8]| -> (u32, u32) {
            let off = names.len() as u32;
            names.extend_from_slice(s);
            (off, s.len() as u32)
        };
        // inode 0: root dir -> [etc(1), bin(2)]
        // inode 1: etc dir  -> [os-release(3)]
        // inode 2: bin dir  -> [busybox(4), sh(5)]
        // inode 3: os-release file
        // inode 4: busybox file
        // inode 5: sh symlink -> busybox
        let osr = b"NAME=raptormark\n";
        let bb = b"ELFDATA";
        let mut data: Vec<u8> = Vec::new();
        let osr_off = data.len() as u64;
        data.extend_from_slice(osr);
        let bb_off = data.len() as u64;
        data.extend_from_slice(bb);

        let (etc_no, etc_nl) = intern(&mut names, b"etc");
        let (bin_no, bin_nl) = intern(&mut names, b"bin");
        let (osr_no, osr_nl) = intern(&mut names, b"os-release");
        let (bbx_no, bbx_nl) = intern(&mut names, b"busybox");
        let (sh_no, sh_nl) = intern(&mut names, b"sh");
        let (tgt_no, tgt_nl) = intern(&mut names, b"busybox");

        let mut dirents: Vec<u8> = Vec::new();
        let push_dirent = |dirents: &mut Vec<u8>, no: u32, nl: u32, child: u32| {
            dirents.extend_from_slice(&no.to_le_bytes());
            dirents.extend_from_slice(&nl.to_le_bytes());
            dirents.extend_from_slice(&child.to_le_bytes());
            dirents.extend_from_slice(&0u32.to_le_bytes());
        };
        // root dirents [0,2)
        push_dirent(&mut dirents, etc_no, etc_nl, 1);
        push_dirent(&mut dirents, bin_no, bin_nl, 2);
        // etc dirents [2,3)
        push_dirent(&mut dirents, osr_no, osr_nl, 3);
        // bin dirents [3,5)
        push_dirent(&mut dirents, bbx_no, bbx_nl, 4);
        push_dirent(&mut dirents, sh_no, sh_nl, 5);

        let mut inodes: Vec<u8> = Vec::new();
        let push_inode = |inodes: &mut Vec<u8>, kind: u8, mode: u32, size: u64, a: u64, b: u64| {
            let mut rec = [0u8; 48];
            rec[0] = kind;
            rec[4..8].copy_from_slice(&mode.to_le_bytes());
            rec[16..24].copy_from_slice(&size.to_le_bytes());
            rec[24..32].copy_from_slice(&a.to_le_bytes());
            rec[32..40].copy_from_slice(&b.to_le_bytes());
            inodes.extend_from_slice(&rec);
        };
        push_inode(&mut inodes, 0, 0o040755, 0, 0, 2); // root: dirents [0,2)
        push_inode(&mut inodes, 0, 0o040755, 0, 2, 1); // etc:  dirents [2,3)
        push_inode(&mut inodes, 0, 0o040755, 0, 3, 2); // bin:  dirents [3,5)
        push_inode(
            &mut inodes,
            1,
            0o100644,
            osr.len() as u64,
            osr_off,
            osr.len() as u64,
        );
        push_inode(
            &mut inodes,
            1,
            0o100755,
            bb.len() as u64,
            bb_off,
            bb.len() as u64,
        );
        push_inode(
            &mut inodes,
            2,
            0o120777,
            tgt_nl as u64,
            tgt_no as u64,
            tgt_nl as u64,
        );

        let inode_off = 80u64;
        let dirent_off = inode_off + inodes.len() as u64;
        let name_off = dirent_off + dirents.len() as u64;
        let data_off = name_off + names.len() as u64;

        let mut img = Vec::new();
        let mut hdr = [0u8; 80];
        hdr[0..8].copy_from_slice(b"RAPTORFS");
        hdr[8..12].copy_from_slice(&1u32.to_le_bytes());
        hdr[12..16].copy_from_slice(&1u32.to_le_bytes());
        hdr[16..20].copy_from_slice(&6u32.to_le_bytes()); // inode count
        hdr[20..24].copy_from_slice(&0u32.to_le_bytes()); // root
        hdr[24..32].copy_from_slice(&inode_off.to_le_bytes());
        hdr[32..40].copy_from_slice(&(inodes.len() as u64).to_le_bytes());
        hdr[40..48].copy_from_slice(&dirent_off.to_le_bytes());
        hdr[48..56].copy_from_slice(&(dirents.len() as u64).to_le_bytes());
        hdr[56..64].copy_from_slice(&name_off.to_le_bytes());
        hdr[64..72].copy_from_slice(&(names.len() as u64).to_le_bytes());
        hdr[72..80].copy_from_slice(&data_off.to_le_bytes());
        img.extend_from_slice(&hdr);
        img.extend_from_slice(&inodes);
        img.extend_from_slice(&dirents);
        img.extend_from_slice(&names);
        img.extend_from_slice(&data);
        let _ = (bbx_no, bbx_nl);
        img
    }

    #[test]
    fn reads_file_from_lower() {
        let vfs = Vfs::new(Some(rfs_image()));
        let got = vfs.read(b"/", b"/etc/os-release").unwrap();
        assert_eq!(got, b"NAME=raptormark\n");
    }

    #[test]
    fn follows_symlink() {
        let vfs = Vfs::new(Some(rfs_image()));
        // /bin/sh -> busybox, so reading it yields busybox's bytes.
        let got = vfs.read(b"/", b"/bin/sh").unwrap();
        assert_eq!(got, b"ELFDATA");
        let tgt = vfs.readlink(b"/", b"/bin/sh").unwrap();
        assert_eq!(tgt, b"busybox");
    }

    /// `resolve` must report the path AFTER following a relative symlink, not
    /// the path it was asked about. `Programs::resolve` (execmap.rs) depends on
    /// exactly this: the exec map is keyed by the program's real path, so
    /// `execve("/bin/sh")` reaches the program registered as `/bin/dash` only
    /// if the resolved path comes back rewritten.
    #[test]
    fn resolve_reports_the_symlink_target_path() {
        let vfs = Vfs::new(Some(rfs_image()));
        let r = vfs
            .resolve(b"/", b"/bin/sh", true)
            .expect("/bin/sh resolves");
        assert_eq!(
            r.path, b"/bin/busybox",
            "resolve returned the requested path, not the target"
        );
        // follow_final = false stops at the link itself, which is what lstat
        // and readlink need.
        let l = vfs.resolve(b"/", b"/bin/sh", false).expect("lstat /bin/sh");
        assert_eq!(l.path, b"/bin/sh");
    }

    #[test]
    fn relative_path_uses_cwd() {
        let vfs = Vfs::new(Some(rfs_image()));
        let got = vfs.read(b"/etc", b"os-release").unwrap();
        assert_eq!(got, b"NAME=raptormark\n");
    }

    #[test]
    fn dotdot_is_confined() {
        let vfs = Vfs::new(Some(rfs_image()));
        let got = vfs.read(b"/", b"/etc/../etc/os-release").unwrap();
        assert_eq!(got, b"NAME=raptormark\n");
        // Escaping above root clamps to root.
        assert!(vfs.read(b"/", b"/../../etc/os-release").is_some());
    }

    #[test]
    fn upper_shadows_and_whiteouts_lower() {
        let mut vfs = Vfs::new(Some(rfs_image()));
        vfs.upper_mut()
            .write_file(b"/etc/os-release", b"OVERRIDDEN\n".to_vec());
        assert_eq!(vfs.read(b"/", b"/etc/os-release").unwrap(), b"OVERRIDDEN\n");
        vfs.upper_mut().whiteout(b"/etc/os-release");
        assert!(vfs.read(b"/", b"/etc/os-release").is_none());
    }

    // A bound AF_UNIX socket publishes a filesystem node, and the whole reason
    // it is a distinct NodeKind rather than an empty file is that S_ISSOCK is
    // load-bearing: PostgreSQL stats its socket path before unlinking a stale
    // one and refuses to remove something that is not a socket. These run on the
    // host, so they cost nothing next to the 40-minute run that would otherwise
    // be the only thing checking it.
    #[test]
    fn mksock_publishes_a_socket_node() {
        let mut vfs = Vfs::new(Some(rfs_image()));
        vfs.upper_mut().mksock(b"/tmp/.s.PGSQL.5432");
        let r = vfs.resolve(b"/", b"/tmp/.s.PGSQL.5432", true).unwrap();
        assert_eq!(r.meta.kind, NodeKind::Socket);
        // S_IFSOCK. Asserted as the file-type bits rather than the whole mode,
        // because the permission bits are chmod-able and the type is not.
        assert_eq!(
            r.meta.mode & 0o170000,
            0o140000,
            "the node must be S_IFSOCK"
        );
    }

    // PGDATA is the case these exist for. `initdb` creates it with 0700 and
    // PostgreSQL's `checkDataDir` then refuses to start unless it reads back as
    // 0700 or 0750, on a directory the guest had just created correctly.
    //
    // Only the MODE was ever lost. The same check also requires `st_uid ==
    // geteuid()`, and that half already worked -- `sys_newfstatat` reports
    // `ctx.uid` rather than the layer's placeholder, deliberately. Measured
    // against the pre-fix runtime: `MODE=0755 UID=1000 GID=1000 EUID=1000`.
    #[test]
    fn mkdir_keeps_the_requested_mode() {
        let mut vfs = Vfs::new(Some(rfs_image()));
        vfs.upper_mut().mkdir_with_mode(b"/pgdata", 0o700);
        let r = vfs.resolve(b"/", b"/pgdata", true).unwrap();
        assert_eq!(r.meta.kind, NodeKind::Dir);
        assert_eq!(
            r.meta.mode & 0o7777,
            0o700,
            "mkdir(0700) must not read back as the placeholder 0755"
        );
    }

    #[test]
    fn an_invented_parent_keeps_the_placeholder_mode() {
        // Bounds the claim above. `ensure_parents` invents directories nobody
        // asked for by mode; giving THOSE 0700 would be the over-correction, and
        // it would hide a real image directory behind a stricter mode.
        let mut vfs = Vfs::new(Some(rfs_image()));
        vfs.upper_mut()
            .write_file(b"/var/run/pg/pid", b"1\n".to_vec());
        let r = vfs.resolve(b"/", b"/var/run/pg", true).unwrap();
        assert_eq!(r.meta.kind, NodeKind::Dir);
        assert_eq!(r.meta.mode & 0o7777, 0o755);
    }

    #[test]
    fn removing_a_node_drops_its_mode_override() {
        let mut vfs = Vfs::new(Some(rfs_image()));
        vfs.upper_mut().mkdir_with_mode(b"/pgdata", 0o700);
        vfs.upper_mut().whiteout(b"/pgdata");
        // Same path, now an ordinary file. It must not inherit 0700 from the
        // directory that used to be there.
        vfs.upper_mut().write_file(b"/pgdata", b"x".to_vec());
        let r = vfs.resolve(b"/", b"/pgdata", true).unwrap();
        assert_eq!(r.meta.kind, NodeKind::File);
        assert_eq!(r.meta.mode & 0o7777, 0o644);
    }

    #[test]
    fn a_socket_node_is_not_readable_as_a_file() {
        // If `mksock` had been implemented as an empty file -- the tempting
        // shortcut -- this would return Some(empty) and a guest opening the
        // rendezvous path would get EOF instead of ENXIO.
        let mut vfs = Vfs::new(Some(rfs_image()));
        vfs.upper_mut().mksock(b"/tmp/sock");
        assert!(
            vfs.read(b"/", b"/tmp/sock").is_none(),
            "a socket has no contents to read"
        );
    }

    #[test]
    fn unlinking_a_socket_node_removes_it() {
        // PostgreSQL unlinks its socket on shutdown and unlinks a stale one
        // before binding, so a whiteout that missed the socket set would leave
        // a path that can never be rebound.
        let mut vfs = Vfs::new(Some(rfs_image()));
        vfs.upper_mut().mksock(b"/tmp/sock");
        assert!(vfs.resolve(b"/", b"/tmp/sock", true).is_some());
        vfs.upper_mut().whiteout(b"/tmp/sock");
        assert!(
            vfs.resolve(b"/", b"/tmp/sock", true).is_none(),
            "unlink must remove the socket node"
        );
    }

    #[test]
    fn readdir_reports_a_socket_child() {
        let mut vfs = Vfs::new(Some(rfs_image()));
        vfs.upper_mut().mksock(b"/tmp/sock");
        let got = vfs.readdir(b"/", b"/tmp").unwrap();
        let kind = got
            .iter()
            .find(|(name, _)| name == b"sock")
            .map(|(_, k)| *k);
        assert_eq!(
            kind,
            Some(NodeKind::Socket),
            "getdents64 maps this to DT_SOCK; a wrong kind makes `ls` lie"
        );
    }

    #[test]
    fn readdir_merges_layers() {
        let mut vfs = Vfs::new(Some(rfs_image()));
        vfs.upper_mut().write_file(b"/etc/extra", b"x".to_vec());
        let mut names: Vec<Vec<u8>> = vfs
            .readdir(b"/", b"/etc")
            .unwrap()
            .into_iter()
            .map(|(n, _)| n)
            .collect();
        names.sort();
        assert_eq!(names, vec![b"extra".to_vec(), b"os-release".to_vec()]);
    }

    #[test]
    fn missing_path_is_none() {
        let vfs = Vfs::new(Some(rfs_image()));
        assert!(vfs.read(b"/", b"/nope").is_none());
        assert!(vfs.resolve(b"/", b"/etc/nope", true).is_none());
    }
}
