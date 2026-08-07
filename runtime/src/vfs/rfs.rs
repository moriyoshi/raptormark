//! Reader for the raptormark sidecar filesystem ("rfs"). The writer is
//! `internal/rootfs/rfs.go`; keep the two in lockstep. Read-only; used as the
//! lower layer of the default overlay.

use super::{Meta, NodeKind};

const MAGIC: &[u8; 8] = b"RAPTORFS";
const INODE_REC_LEN: usize = 48;
const DIRENT_LEN: usize = 16;

pub const KIND_DIR: u8 = 0;
pub const KIND_FILE: u8 = 1;
pub const KIND_SYMLINK: u8 = 2;

pub struct Rfs {
    data: Vec<u8>,
    inode_off: usize,
    dirent_off: usize,
    name_off: usize,
    data_blob_off: usize,
    inode_cnt: u32,
    root: u32,
}

struct Inode {
    kind: u8,
    mode: u32,
    uid: u32,
    gid: u32,
    size: u64,
    a: u64,
    b: u64,
    mtime: u64,
}

fn u32le(b: &[u8], off: usize) -> u32 {
    u32::from_le_bytes(b[off..off + 4].try_into().unwrap())
}
fn u64le(b: &[u8], off: usize) -> u64 {
    u64::from_le_bytes(b[off..off + 8].try_into().unwrap())
}

impl Rfs {
    /// Parses an rfs image. Returns None if the magic or header is malformed.
    pub fn parse(data: Vec<u8>) -> Option<Rfs> {
        if data.len() < 80 || &data[0..8] != MAGIC {
            return None;
        }
        let rfs = Rfs {
            inode_cnt: u32le(&data, 16),
            root: u32le(&data, 20),
            inode_off: u64le(&data, 24) as usize,
            dirent_off: u64le(&data, 40) as usize,
            name_off: u64le(&data, 56) as usize,
            data_blob_off: u64le(&data, 72) as usize,
            data,
        };
        Some(rfs)
    }

    pub fn root(&self) -> u32 {
        self.root
    }

    fn inode(&self, i: u32) -> Option<Inode> {
        if i >= self.inode_cnt {
            return None;
        }
        let off = self.inode_off + i as usize * INODE_REC_LEN;
        let b = &self.data;
        Some(Inode {
            kind: b[off],
            mode: u32le(b, off + 4),
            uid: u32le(b, off + 8),
            gid: u32le(b, off + 12),
            size: u64le(b, off + 16),
            a: u64le(b, off + 24),
            b: u64le(b, off + 32),
            mtime: u64le(b, off + 40),
        })
    }

    fn name(&self, off: u64, len: u64) -> &[u8] {
        let s = self.name_off + off as usize;
        &self.data[s..s + len as usize]
    }

    fn meta_of(&self, idx: u32, in_: &Inode) -> Meta {
        Meta {
            kind: match in_.kind {
                KIND_DIR => NodeKind::Dir,
                KIND_SYMLINK => NodeKind::Symlink,
                _ => NodeKind::File,
            },
            mode: in_.mode,
            uid: in_.uid,
            gid: in_.gid,
            size: in_.size,
            mtime: in_.mtime,
            ino: idx as u64 + 1,
        }
    }

    /// Looks up a single name in a directory inode, returning the child inode
    /// index. Names "." and ".." are not stored.
    fn lookup_child(&self, dir: u32, want: &[u8]) -> Option<u32> {
        let d = self.inode(dir)?;
        if d.kind != KIND_DIR {
            return None;
        }
        for i in 0..d.b {
            let off = self.dirent_off + (d.a + i) as usize * DIRENT_LEN;
            let b = &self.data;
            let name_off = u32le(b, off) as u64;
            let name_len = u32le(b, off + 4) as u64;
            let child = u32le(b, off + 8);
            if self.name(name_off, name_len) == want {
                return Some(child);
            }
        }
        None
    }

    /// Resolves an absolute path within the image WITHOUT following symlinks
    /// (the coordinator does cross-layer symlink resolution). `path` must be
    /// absolute; components "" and "." are ignored, ".." pops.
    pub fn resolve_exact(&self, path: &[u8]) -> Option<u32> {
        let mut stack: Vec<u32> = vec![self.root];
        for comp in path.split(|&c| c == b'/') {
            match comp {
                b"" | b"." => continue,
                b".." => {
                    if stack.len() > 1 {
                        stack.pop();
                    }
                }
                _ => {
                    let cur = *stack.last().unwrap();
                    let child = self.lookup_child(cur, comp)?;
                    stack.push(child);
                }
            }
        }
        Some(*stack.last().unwrap())
    }

    pub fn stat_exact(&self, path: &[u8]) -> Option<Meta> {
        let idx = self.resolve_exact(path)?;
        let in_ = self.inode(idx)?;
        Some(self.meta_of(idx, &in_))
    }

    pub fn read_file(&self, path: &[u8]) -> Option<Vec<u8>> {
        let idx = self.resolve_exact(path)?;
        let in_ = self.inode(idx)?;
        if in_.kind != KIND_FILE {
            return None;
        }
        self.read_inode_file(&in_)
    }

    fn read_inode_file(&self, in_: &Inode) -> Option<Vec<u8>> {
        let start = self.data_blob_off + in_.a as usize;
        let raw = &self.data[start..start + in_.b as usize];
        if in_.b == in_.size {
            // Stored (small or incompressible).
            Some(raw.to_vec())
        } else {
            // Raw DEFLATE, matching Go's compress/flate.
            miniz_oxide::inflate::decompress_to_vec(raw).ok()
        }
    }

    pub fn readlink(&self, path: &[u8]) -> Option<Vec<u8>> {
        let idx = self.resolve_exact(path)?;
        let in_ = self.inode(idx)?;
        if in_.kind != KIND_SYMLINK {
            return None;
        }
        Some(self.name(in_.a, in_.b).to_vec())
    }

    /// Directory entries (name, kind), excluding "." and "..".
    pub fn readdir(&self, path: &[u8]) -> Option<Vec<(Vec<u8>, NodeKind)>> {
        let idx = self.resolve_exact(path)?;
        let d = self.inode(idx)?;
        if d.kind != KIND_DIR {
            return None;
        }
        let mut out = Vec::new();
        for i in 0..d.b {
            let off = self.dirent_off + (d.a + i) as usize * DIRENT_LEN;
            let name_off = u32le(&self.data, off) as u64;
            let name_len = u32le(&self.data, off + 4) as u64;
            let child = u32le(&self.data, off + 8);
            let kind = match self.inode(child).map(|n| n.kind) {
                Some(KIND_DIR) => NodeKind::Dir,
                Some(KIND_SYMLINK) => NodeKind::Symlink,
                _ => NodeKind::File,
            };
            out.push((self.name(name_off, name_len).to_vec(), kind));
        }
        Some(out)
    }
}
