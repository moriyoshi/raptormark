// Package rootfs builds the raptormark sidecar filesystem ("rfs") image that
// ecvisor mounts as the read-only lower layer of the guest's overlay.
//
// RECONSTRUCTED on 2026-08-07. The original was lost with the rest of the Go
// tree; this is rebuilt from the surviving reader, runtime/src/vfs/rfs.rs,
// which is complete and self-describing (see .agents/docs/JOURNAL.md). Keep
// the two in lockstep -- the reader is the specification, not this file.
//
// Layout, all little-endian:
//
//	header      80 bytes, fixed field offsets (see writeHeader)
//	inodes      inodeRecLen bytes each, indexed from 0; ino = index + 1
//	dirents     direntLen bytes each; a directory's children are contiguous
//	names       concatenated dirent names AND symlink targets, no separators
//	data        file contents, each either stored or raw DEFLATE
//
// An inode's `a`/`b` pair is overloaded by kind:
//
//	dir      a = first dirent index,     b = number of dirents
//	file     a = offset into data blob,  b = stored length
//	symlink  a = offset into name table, b = target length
//
// A file is DEFLATE-compressed exactly when its stored length differs from its
// logical size; that is how the reader decides, so a compressed encoding that
// happens to match the size must be written stored instead.
package rootfs

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

const (
	magic       = "RAPTORFS"
	formatVer   = 1
	headerLen   = 80
	inodeRecLen = 48
	direntLen   = 16
)

// Node kinds, matching KIND_* in runtime/src/vfs/rfs.rs.
const (
	kindDir     = 0
	kindFile    = 1
	kindSymlink = 2
)

// Options controls image construction.
type Options struct {
	// Boot, if non-nil, is encoded into the image at BootPath, giving the guest
	// its argv/env/cwd/uid/gid. Without it ecvisor falls back to the host's
	// argv and an empty environment.
	Boot *Boot
	// ExecMap, if non-empty, is placed at ExecPath. It is what lets an execve
	// of a guest path reach a program OTHER than index 0: without it
	// runtime/src/execmap.rs builds an empty `by_path` and every exec falls
	// back to the first program. Encode it with internal/link.ExecMap.
	ExecMap []byte
	// NoCompress stores every file verbatim. Compression is on by default; it
	// is worth roughly 3x on a Debian rootfs and costs nothing at read time
	// beyond the inflate.
	NoCompress bool
}

// Stats reports what a build covered, so callers can surface what was dropped
// rather than silently shipping an incomplete image.
type Stats struct {
	Dirs, Files, Symlinks int
	// Skipped counts entries that rfs cannot represent: device nodes, sockets
	// and FIFOs. They are omitted rather than failing the build, because a
	// real rootfs always has some under /dev and the guest gets its device
	// nodes from the syscall layer, not the image.
	Skipped int
	// Bytes is the logical (uncompressed) size of all file content.
	Bytes int64
}

// node is one entry in the tree being built.
type node struct {
	name     string // basename; empty for the root
	kind     uint8
	mode     uint32
	uid, gid uint32
	mtime    uint64

	children []*node // dirs, sorted by name
	target   string  // symlinks
	path     string  // files: source path on the host

	size uint64 // logical size
	// Filled during serialisation.
	index    uint32
	a, b     uint64
	nameOff  uint32
	blobData []byte
}

// Build walks the directory tree at root and returns a complete rfs image.
func Build(root string, opts Options) ([]byte, Stats, error) {
	var st Stats
	tree, err := scan(root, &st)
	if err != nil {
		return nil, st, err
	}
	if opts.Boot != nil {
		addBoot(tree, opts.Boot, &st)
	}
	if len(opts.ExecMap) > 0 {
		if err := CheckExecMap(root, opts.ExecMap); err != nil {
			return nil, st, err
		}
		addFile(tree, ExecPath, opts.ExecMap, &st)
	}
	img, err := serialize(tree, opts, &st)
	return img, st, err
}

// scan reads the host tree into memory. Symlinks are recorded, never followed:
// an absolute target inside a guest rootfs means the guest root, and resolving
// it here against the host would escape the image entirely.
func scan(root string, st *Stats) (*node, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("rootfs: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("rootfs: %s is not a directory", root)
	}
	r := dirNode("", info)
	st.Dirs++
	if err := scanInto(root, r, st); err != nil {
		return nil, err
	}
	return r, nil
}

func scanInto(dir string, parent *node, st *Stats) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		// An unreadable directory is a real gap in the image, not something to
		// paper over -- a rootfs exported as a non-root user hits this.
		return fmt.Errorf("rootfs: reading %s: %w", dir, err)
	}
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			return fmt.Errorf("rootfs: stat %s: %w", p, err)
		}
		switch {
		case info.IsDir():
			n := dirNode(e.Name(), info)
			st.Dirs++
			parent.children = append(parent.children, n)
			if err := scanInto(p, n, st); err != nil {
				return err
			}
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return fmt.Errorf("rootfs: readlink %s: %w", p, err)
			}
			st.Symlinks++
			parent.children = append(parent.children, &node{
				name: e.Name(), kind: kindSymlink, mode: perm(info),
				uid: ownerUID(info), gid: ownerGID(info),
				mtime:  uint64(info.ModTime().Unix()),
				target: target, size: uint64(len(target)),
			})
		case info.Mode().IsRegular():
			st.Files++
			st.Bytes += info.Size()
			parent.children = append(parent.children, &node{
				name: e.Name(), kind: kindFile, mode: perm(info),
				uid: ownerUID(info), gid: ownerGID(info),
				mtime: uint64(info.ModTime().Unix()),
				path:  p, size: uint64(info.Size()),
			})
		default:
			st.Skipped++
		}
	}
	sort.Slice(parent.children, func(i, j int) bool {
		return parent.children[i].name < parent.children[j].name
	})
	return nil
}

func dirNode(name string, info fs.FileInfo) *node {
	return &node{
		name: name, kind: kindDir, mode: perm(info),
		uid: ownerUID(info), gid: ownerGID(info),
		mtime: uint64(info.ModTime().Unix()),
	}
}

func perm(info fs.FileInfo) uint32 { return uint32(info.Mode().Perm()) }

// POSIX st_mode file-type bits, as <sys/stat.h> defines them.
const (
	sIFDIR   = 0o040000
	sIFREG   = 0o100000
	sIFLNK   = 0o120000
	permBits = 0o7777
)

// statMode combines a node's type with its permission bits, which is what
// st_mode means -- `perm` above deliberately keeps only the permissions, and
// the type has to come from the node's kind.
//
// Storing bare permissions made every `S_ISREG` on a file from this image false.
// That is invisible for a long time because the common checks ask the opposite
// question: nginx asks `S_ISDIR` and gets the right answer by accident. It
// surfaced on PostgreSQL, whose `validate_exec` requires `S_ISREG` to be TRUE
// before it will run its own binary, so `find_my_exec` reported
// `invalid binary "/usr/lib/postgresql/17/bin/postgres"` and the postmaster
// could not locate itself.
//
// The runtime's tmpfs upper layer already stored full modes (0o100644,
// 0o040755); this is the lower layer being brought into line with it, so the two
// halves of the same overlay finally agree on what a mode is.
func statMode(kind uint8, mode uint32) uint32 {
	var typ uint32
	switch kind {
	case kindDir:
		typ = sIFDIR
	case kindSymlink:
		typ = sIFLNK
	default:
		typ = sIFREG
	}
	return typ | (mode & permBits)
}

// addBoot injects the boot record at BootPath, creating its parent directory.
// It replaces any existing entry, so a rootfs that already carries one from a
// previous build does not end up with two.
func addBoot(root *node, b *Boot, st *Stats) {
	addFile(root, BootPath, b.Encode(), st)
}

// addFile injects `data` at `path`, creating parent directories. It replaces any
// existing entry, so a rootfs that already carries one from a previous build
// does not end up with two.
func addFile(root *node, path string, data []byte, st *Stats) {
	parent, leaf := filepath.Split(path)
	dir := root
	for _, comp := range splitPath(parent) {
		next := findChild(dir, comp)
		if next == nil || next.kind != kindDir {
			next = &node{name: comp, kind: kindDir, mode: 0o755}
			dir.children = append(dir.children, next)
			sort.Slice(dir.children, func(i, j int) bool {
				return dir.children[i].name < dir.children[j].name
			})
			st.Dirs++
		}
		dir = next
	}
	enc := data
	if old := findChild(dir, leaf); old != nil {
		old.kind, old.mode, old.blobData, old.size = kindFile, 0o644, enc, uint64(len(enc))
		old.path, old.target, old.children = "", "", nil
		return
	}
	dir.children = append(dir.children, &node{
		name: leaf, kind: kindFile, mode: 0o644,
		blobData: enc, size: uint64(len(enc)),
	})
	sort.Slice(dir.children, func(i, j int) bool {
		return dir.children[i].name < dir.children[j].name
	})
	st.Files++
	st.Bytes += int64(len(enc))
}

func findChild(dir *node, name string) *node {
	for _, c := range dir.children {
		if c.name == name {
			return c
		}
	}
	return nil
}

func splitPath(p string) []string {
	var out []string
	for _, c := range bytes.Split([]byte(p), []byte("/")) {
		if len(c) > 0 && string(c) != "." {
			out = append(out, string(c))
		}
	}
	return out
}

// serialize lays the tree out and emits the image.
func serialize(root *node, opts Options, st *Stats) ([]byte, error) {
	// Breadth-first numbering keeps a directory's children contiguous in the
	// dirent array, which is what the reader's (a, b) span assumes.
	var order []*node
	root.index = 0
	order = append(order, root)
	for i := 0; i < len(order); i++ {
		for _, c := range order[i].children {
			c.index = uint32(len(order))
			order = append(order, c)
		}
	}

	// Name table: dirent names and symlink targets share it. Deduplicating is
	// worth it on a real rootfs, where names like "lib" and identical relative
	// symlink targets recur thousands of times.
	var names bytes.Buffer
	offsets := map[string]uint32{}
	intern := func(s string) (uint32, error) {
		if off, ok := offsets[s]; ok {
			return off, nil
		}
		off := uint32(names.Len())
		if int(off) != names.Len() {
			return 0, fmt.Errorf("rootfs: name table exceeds 4 GiB")
		}
		offsets[s] = off
		names.WriteString(s)
		return off, nil
	}

	// Dirents, in the same breadth-first order.
	type dirent struct{ nameOff, nameLen, child uint32 }
	var dirents []dirent
	for _, n := range order {
		if n.kind != kindDir {
			continue
		}
		n.a = uint64(len(dirents))
		n.b = uint64(len(n.children))
		for _, c := range n.children {
			off, err := intern(c.name)
			if err != nil {
				return nil, err
			}
			dirents = append(dirents, dirent{off, uint32(len(c.name)), c.index})
		}
	}
	for _, n := range order {
		if n.kind != kindSymlink {
			continue
		}
		off, err := intern(n.target)
		if err != nil {
			return nil, err
		}
		n.a, n.b = uint64(off), uint64(len(n.target))
	}

	// Data blob.
	var blob bytes.Buffer
	for _, n := range order {
		if n.kind != kindFile {
			continue
		}
		raw := n.blobData
		if n.path != "" {
			b, err := os.ReadFile(n.path)
			if err != nil {
				return nil, fmt.Errorf("rootfs: reading %s: %w", n.path, err)
			}
			raw = b
		}
		// The on-host size can go stale between the walk and the read; the
		// bytes actually written are what the reader must be told about.
		n.size = uint64(len(raw))
		stored := raw
		if !opts.NoCompress {
			if z, err := deflate(raw); err == nil && len(z) < len(raw) {
				stored = z
			}
		}
		n.a = uint64(blob.Len())
		n.b = uint64(len(stored))
		blob.Write(stored)
	}

	inodeOff := uint64(headerLen)
	direntOff := inodeOff + uint64(len(order))*inodeRecLen
	nameOff := direntOff + uint64(len(dirents))*direntLen
	blobOff := nameOff + uint64(names.Len())

	out := make([]byte, 0, int(blobOff)+blob.Len())
	out = writeHeader(out, header{
		inodeCnt:  uint32(len(order)),
		root:      root.index,
		inodeOff:  inodeOff,
		direntCnt: uint64(len(dirents)),
		direntOff: direntOff,
		nameSize:  uint64(names.Len()),
		nameOff:   nameOff,
		blobSize:  uint64(blob.Len()),
		blobOff:   blobOff,
	})
	for _, n := range order {
		rec := make([]byte, inodeRecLen)
		rec[0] = n.kind
		binary.LittleEndian.PutUint32(rec[4:], statMode(n.kind, n.mode))
		binary.LittleEndian.PutUint32(rec[8:], n.uid)
		binary.LittleEndian.PutUint32(rec[12:], n.gid)
		binary.LittleEndian.PutUint64(rec[16:], n.size)
		binary.LittleEndian.PutUint64(rec[24:], n.a)
		binary.LittleEndian.PutUint64(rec[32:], n.b)
		binary.LittleEndian.PutUint64(rec[40:], n.mtime)
		out = append(out, rec...)
	}
	for _, d := range dirents {
		rec := make([]byte, direntLen)
		binary.LittleEndian.PutUint32(rec[0:], d.nameOff)
		binary.LittleEndian.PutUint32(rec[4:], d.nameLen)
		binary.LittleEndian.PutUint32(rec[8:], d.child)
		out = append(out, rec...)
	}
	out = append(out, names.Bytes()...)
	out = append(out, blob.Bytes()...)
	return out, nil
}

type header struct {
	inodeCnt  uint32
	root      uint32
	inodeOff  uint64
	direntCnt uint64
	direntOff uint64
	nameSize  uint64
	nameOff   uint64
	blobSize  uint64
	blobOff   uint64
}

// writeHeader emits the fixed 80-byte header. The reader consumes only the
// magic and the six fields at 16, 20, 24, 40, 56 and 72; the counts and sizes
// in the gaps are written so the image is self-describing and can be validated
// without walking it.
func writeHeader(out []byte, h header) []byte {
	b := make([]byte, headerLen)
	copy(b, magic)
	binary.LittleEndian.PutUint64(b[8:], formatVer)
	binary.LittleEndian.PutUint32(b[16:], h.inodeCnt)
	binary.LittleEndian.PutUint32(b[20:], h.root)
	binary.LittleEndian.PutUint64(b[24:], h.inodeOff)
	binary.LittleEndian.PutUint64(b[32:], h.direntCnt)
	binary.LittleEndian.PutUint64(b[40:], h.direntOff)
	binary.LittleEndian.PutUint64(b[48:], h.nameSize)
	binary.LittleEndian.PutUint64(b[56:], h.nameOff)
	binary.LittleEndian.PutUint64(b[64:], h.blobSize)
	binary.LittleEndian.PutUint64(b[72:], h.blobOff)
	return append(out, b...)
}

// deflate produces raw DEFLATE, which is what miniz_oxide's
// decompress_to_vec expects on the reader side (no zlib or gzip wrapper).
func deflate(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(raw); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// inflate is the reader's side, exposed for the round-trip test.
func inflate(z []byte) ([]byte, error) {
	return io.ReadAll(flate.NewReader(bytes.NewReader(z)))
}
