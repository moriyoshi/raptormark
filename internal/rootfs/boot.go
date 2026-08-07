package rootfs

import "encoding/binary"

// BootPath is where the boot record lives inside the image. Must match
// BOOT_PATH in runtime/src/boot.rs.
const BootPath = "/.raptormark/boot"

// ExecPath is where the exec map lives inside the image. Must match EXEC_PATH
// in runtime/src/execmap.rs and internal/link.ExecPath; TestExecPathAgrees
// pins the last of those.
const ExecPath = "/.raptormark/exec"

const bootMagic = "RMBOOT01"

// Boot is the container personality baked into the sidecar: what ecvisor hands
// the guest in place of a host command line. Without it the guest inherits the
// host's argv and an empty environment, which is why a fused `openssl` sees no
// OPENSSL_CONF and an argv[0] of "<module>.wasm".
//
// The encoding is consumed by Boot::parse in runtime/src/boot.rs.
type Boot struct {
	// Argv is the full command line including argv[0].
	Argv []string
	// Env entries are "KEY=VALUE", as in the image config.
	Env []string
	// Cwd is absolute; the guest starts here.
	Cwd string
	// UID/GID are the numeric ids the guest believes it runs as.
	UID, GID uint32
}

// Encode serialises the record. Layout, all little-endian:
//
//	magic "RMBOOT01"
//	u32 uid, u32 gid
//	u32 len + bytes            cwd
//	u32 count, then per entry: u32 len + bytes   argv
//	u32 count, then per entry: u32 len + bytes   env
func (b Boot) Encode() []byte {
	out := make([]byte, 0, 64)
	out = append(out, bootMagic...)
	out = binary.LittleEndian.AppendUint32(out, b.UID)
	out = binary.LittleEndian.AppendUint32(out, b.GID)
	cwd := b.Cwd
	if cwd == "" {
		cwd = "/"
	}
	out = appendLenPrefixed(out, cwd)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(b.Argv)))
	for _, a := range b.Argv {
		out = appendLenPrefixed(out, a)
	}
	out = binary.LittleEndian.AppendUint32(out, uint32(len(b.Env)))
	for _, e := range b.Env {
		out = appendLenPrefixed(out, e)
	}
	return out
}

func appendLenPrefixed(out []byte, s string) []byte {
	out = binary.LittleEndian.AppendUint32(out, uint32(len(s)))
	return append(out, s...)
}
