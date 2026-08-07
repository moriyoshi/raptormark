//go:build unix

package rootfs

import (
	"io/fs"
	"syscall"
)

// ownerUID/ownerGID read the numeric owner from the host stat. A rootfs
// exported from a container image is normally unpacked as root and keeps its
// real ownership; when it is not (an unprivileged export flattens everything to
// the invoking user) the guest sees that flattened ownership, which is correct
// -- it is what the files actually are.
func ownerUID(info fs.FileInfo) uint32 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Uid
	}
	return 0
}

func ownerGID(info fs.FileInfo) uint32 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Gid
	}
	return 0
}
