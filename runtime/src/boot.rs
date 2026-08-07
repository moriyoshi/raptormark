//! Parser for the boot record (`internal/rootfs/boot.go`), which carries the
//! container personality baked into the sidecar at [`BOOT_PATH`].

/// Well-known location of the boot record inside the rfs image.
pub const BOOT_PATH: &[u8] = b"/.raptormark/boot";

const MAGIC: &[u8] = b"RMBOOT01";

pub struct Boot {
    pub argv: Vec<Vec<u8>>,
    pub env: Vec<Vec<u8>>,
    pub cwd: Vec<u8>,
    pub uid: u32,
    pub gid: u32,
}

struct Cursor<'a> {
    b: &'a [u8],
    pos: usize,
}

impl<'a> Cursor<'a> {
    fn u32(&mut self) -> Option<u32> {
        let end = self.pos.checked_add(4)?;
        if end > self.b.len() {
            return None;
        }
        let v = u32::from_le_bytes(self.b[self.pos..end].try_into().ok()?);
        self.pos = end;
        Some(v)
    }
    fn bytes(&mut self, n: usize) -> Option<Vec<u8>> {
        let end = self.pos.checked_add(n)?;
        if end > self.b.len() {
            return None;
        }
        let v = self.b[self.pos..end].to_vec();
        self.pos = end;
        Some(v)
    }
    fn len_prefixed(&mut self) -> Option<Vec<u8>> {
        let n = self.u32()? as usize;
        self.bytes(n)
    }
}

impl Boot {
    pub fn parse(b: &[u8]) -> Option<Boot> {
        if b.len() < MAGIC.len() || &b[..MAGIC.len()] != MAGIC {
            return None;
        }
        let mut c = Cursor {
            b,
            pos: MAGIC.len(),
        };
        let uid = c.u32()?;
        let gid = c.u32()?;
        let cwd = c.len_prefixed()?;
        let argc = c.u32()? as usize;
        let mut argv = Vec::with_capacity(argc);
        for _ in 0..argc {
            argv.push(c.len_prefixed()?);
        }
        let envc = c.u32()? as usize;
        let mut env = Vec::with_capacity(envc);
        for _ in 0..envc {
            env.push(c.len_prefixed()?);
        }
        Some(Boot {
            argv,
            env,
            cwd,
            uid,
            gid,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_a_record() {
        // magic, uid=0, gid=0, cwd="/", argc=2 ["/minicat","/etc/os-release"], envc=1 ["PATH=/bin"].
        let mut b = Vec::new();
        b.extend_from_slice(MAGIC);
        b.extend_from_slice(&0u32.to_le_bytes());
        b.extend_from_slice(&0u32.to_le_bytes());
        let put = |b: &mut Vec<u8>, s: &[u8]| {
            b.extend_from_slice(&(s.len() as u32).to_le_bytes());
            b.extend_from_slice(s);
        };
        put(&mut b, b"/");
        b.extend_from_slice(&2u32.to_le_bytes());
        put(&mut b, b"/minicat");
        put(&mut b, b"/etc/os-release");
        b.extend_from_slice(&1u32.to_le_bytes());
        put(&mut b, b"PATH=/bin");

        let boot = Boot::parse(&b).unwrap();
        assert_eq!(
            boot.argv,
            vec![b"/minicat".to_vec(), b"/etc/os-release".to_vec()]
        );
        assert_eq!(boot.env, vec![b"PATH=/bin".to_vec()]);
        assert_eq!(boot.cwd, b"/");
    }

    #[test]
    fn rejects_bad_magic() {
        assert!(Boot::parse(b"nope").is_none());
    }
}
