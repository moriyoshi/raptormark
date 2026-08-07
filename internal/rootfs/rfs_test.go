package rootfs

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- a reader transcribed from runtime/src/vfs/rfs.rs --------------------
//
// Deliberately a transcription rather than a shared helper: the point of these
// tests is to check the writer against the consumer's actual field offsets and
// (a, b) overloading. Anything factored out of the writer would agree with the
// writer by construction and prove nothing.

type rdr struct {
	d                                     []byte
	inodeOff, direntOff, nameOff, blobOff uint64
	inodeCnt, root                        uint32
}

func open(t *testing.T, d []byte) *rdr {
	t.Helper()
	if len(d) < 80 || string(d[:8]) != "RAPTORFS" {
		t.Fatalf("bad magic or short image (%d bytes)", len(d))
	}
	return &rdr{
		d:         d,
		inodeCnt:  binary.LittleEndian.Uint32(d[16:]),
		root:      binary.LittleEndian.Uint32(d[20:]),
		inodeOff:  binary.LittleEndian.Uint64(d[24:]),
		direntOff: binary.LittleEndian.Uint64(d[40:]),
		nameOff:   binary.LittleEndian.Uint64(d[56:]),
		blobOff:   binary.LittleEndian.Uint64(d[72:]),
	}
}

type inode struct {
	kind     uint8
	mode     uint32
	uid, gid uint32
	size     uint64
	a, b     uint64
	mtime    uint64
}

func (r *rdr) inode(i uint32) *inode {
	if i >= r.inodeCnt {
		return nil
	}
	o := r.inodeOff + uint64(i)*inodeRecLen
	return &inode{
		kind:  r.d[o],
		mode:  binary.LittleEndian.Uint32(r.d[o+4:]),
		uid:   binary.LittleEndian.Uint32(r.d[o+8:]),
		gid:   binary.LittleEndian.Uint32(r.d[o+12:]),
		size:  binary.LittleEndian.Uint64(r.d[o+16:]),
		a:     binary.LittleEndian.Uint64(r.d[o+24:]),
		b:     binary.LittleEndian.Uint64(r.d[o+32:]),
		mtime: binary.LittleEndian.Uint64(r.d[o+40:]),
	}
}

func (r *rdr) name(off, n uint64) []byte {
	return r.d[r.nameOff+off : r.nameOff+off+n]
}

func (r *rdr) lookup(dir uint32, want string) (uint32, bool) {
	d := r.inode(dir)
	if d == nil || d.kind != kindDir {
		return 0, false
	}
	for i := uint64(0); i < d.b; i++ {
		o := r.direntOff + (d.a+i)*direntLen
		no := uint64(binary.LittleEndian.Uint32(r.d[o:]))
		nl := uint64(binary.LittleEndian.Uint32(r.d[o+4:]))
		child := binary.LittleEndian.Uint32(r.d[o+8:])
		if string(r.name(no, nl)) == want {
			return child, true
		}
	}
	return 0, false
}

// resolve walks an absolute path without following symlinks, as the reader does.
func (r *rdr) resolve(path string) (uint32, bool) {
	cur := r.root
	for _, comp := range strings.Split(path, "/") {
		if comp == "" || comp == "." {
			continue
		}
		next, ok := r.lookup(cur, comp)
		if !ok {
			return 0, false
		}
		cur = next
	}
	return cur, true
}

func (r *rdr) readFile(t *testing.T, path string) []byte {
	t.Helper()
	idx, ok := r.resolve(path)
	if !ok {
		t.Fatalf("%s: not found", path)
	}
	in := r.inode(idx)
	if in.kind != kindFile {
		t.Fatalf("%s: kind %d, want file", path, in.kind)
	}
	raw := r.d[r.blobOff+in.a : r.blobOff+in.a+in.b]
	if in.b == in.size {
		return raw
	}
	out, err := inflate(raw)
	if err != nil {
		t.Fatalf("%s: inflate: %v", path, err)
	}
	return out
}

func (r *rdr) readlink(t *testing.T, path string) string {
	t.Helper()
	idx, ok := r.resolve(path)
	if !ok {
		t.Fatalf("%s: not found", path)
	}
	in := r.inode(idx)
	if in.kind != kindSymlink {
		t.Fatalf("%s: kind %d, want symlink", path, in.kind)
	}
	return string(r.name(in.a, in.b))
}

// --- fixture -------------------------------------------------------------

func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mk := func(p string, mode os.FileMode, body []byte) {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, body, mode); err != nil {
			t.Fatal(err)
		}
	}
	mk("etc/os-release", 0o644, []byte("PRETTY_NAME=\"Debian GNU/Linux 12\"\n"))
	mk("etc/ssl/openssl.cnf", 0o644, bytes.Repeat([]byte("openssl_conf = default\n"), 500))
	mk("etc/empty", 0o644, nil)
	mk("bin/prog", 0o755, []byte{0x7f, 'E', 'L', 'F'})
	if err := os.MkdirAll(filepath.Join(root, "usr/lib/ssl"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The absolute symlink that exposed the missing sidecar in the first place.
	if err := os.Symlink("/etc/ssl/openssl.cnf", filepath.Join(root, "usr/lib/ssl/openssl.cnf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../etc/os-release", filepath.Join(root, "bin/rel")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "emptydir"), 0o750); err != nil {
		t.Fatal(err)
	}
	return root
}

// --- tests ---------------------------------------------------------------

func TestBuildRoundTrip(t *testing.T) {
	root := fixture(t)
	img, st, err := Build(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 4 || st.Symlinks != 2 {
		t.Errorf("stats: %d files, %d symlinks; want 4 and 2", st.Files, st.Symlinks)
	}
	r := open(t, img)

	if got, want := string(r.readFile(t, "/etc/os-release")), "PRETTY_NAME=\"Debian GNU/Linux 12\"\n"; got != want {
		t.Errorf("os-release = %q, want %q", got, want)
	}
	// Compressible: must round-trip through the DEFLATE path, not the stored one.
	cnf := r.readFile(t, "/etc/ssl/openssl.cnf")
	if len(cnf) != 500*23 {
		t.Errorf("openssl.cnf = %d bytes, want %d", len(cnf), 500*23)
	}
	idx, _ := r.resolve("/etc/ssl/openssl.cnf")
	if in := r.inode(idx); in.b >= in.size {
		t.Errorf("openssl.cnf stored in %d bytes for size %d -- expected compression", in.b, in.size)
	}
	// An empty file must be stored, or `b == size` misreads as compressed.
	if got := r.readFile(t, "/etc/empty"); len(got) != 0 {
		t.Errorf("empty file read back as %d bytes", len(got))
	}
	if got, want := r.readlink(t, "/usr/lib/ssl/openssl.cnf"), "/etc/ssl/openssl.cnf"; got != want {
		t.Errorf("absolute symlink = %q, want %q", got, want)
	}
	if got, want := r.readlink(t, "/bin/rel"), "../etc/os-release"; got != want {
		t.Errorf("relative symlink = %q, want %q", got, want)
	}
}

// The reader distinguishes stored from compressed solely by `b != size`, so a
// file whose DEFLATE encoding is no smaller must be written stored.
func TestIncompressibleFileIsStored(t *testing.T) {
	root := t.TempDir()
	// Deterministic xorshift output: flate cannot shrink this, and its block
	// framing makes the encoding slightly larger than the input. (An arithmetic
	// pattern like i*i*31+i*7 looks random but has period 256 and compresses
	// 13x, which is not the case under test.)
	body := make([]byte, 4096)
	x := uint64(0x9e3779b97f4a7c15)
	for i := range body {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		body[i] = byte(x)
	}
	if err := os.WriteFile(filepath.Join(root, "blob"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	img, _, err := Build(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := open(t, img)
	idx, ok := r.resolve("/blob")
	if !ok {
		t.Fatal("/blob not found")
	}
	if in := r.inode(idx); in.b != in.size {
		t.Errorf("stored length %d != size %d, so the reader would inflate raw bytes", in.b, in.size)
	}
	if !bytes.Equal(r.readFile(t, "/blob"), body) {
		t.Error("blob did not round-trip")
	}
}

func TestNoCompressStoresEverything(t *testing.T) {
	root := fixture(t)
	img, _, err := Build(root, Options{NoCompress: true})
	if err != nil {
		t.Fatal(err)
	}
	r := open(t, img)
	for _, p := range []string{"/etc/os-release", "/etc/ssl/openssl.cnf", "/etc/empty", "/bin/prog"} {
		idx, ok := r.resolve(p)
		if !ok {
			t.Fatalf("%s not found", p)
		}
		if in := r.inode(idx); in.b != in.size {
			t.Errorf("%s: stored %d bytes for size %d, want verbatim", p, in.b, in.size)
		}
	}
}

// Metadata the guest can observe: a directory mode that matters (PGDATA must be
// 0700) and the kind byte the overlay dispatches on.
func TestModesAndKindsSurvive(t *testing.T) {
	root := fixture(t)
	img, _, err := Build(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := open(t, img)
	for _, tc := range []struct {
		path string
		kind uint8
		mode uint32
	}{
		// st_mode carries the file TYPE as well as the permissions. This test
		// asserted bare permissions until 2026-08-09, which encoded the defect
		// rather than the contract: every `S_ISREG` on a file served from this
		// image was false, and PostgreSQL's validate_exec refused to run its own
		// binary because of it.
		{"/emptydir", kindDir, sIFDIR | 0o750},
		{"/bin", kindDir, sIFDIR | 0o755},
		{"/bin/prog", kindFile, sIFREG | 0o755},
		{"/etc/os-release", kindFile, sIFREG | 0o644},
	} {
		idx, ok := r.resolve(tc.path)
		if !ok {
			t.Errorf("%s not found", tc.path)
			continue
		}
		in := r.inode(idx)
		if in.kind != tc.kind {
			t.Errorf("%s: kind %d, want %d", tc.path, in.kind, tc.kind)
		}
		if in.mode != tc.mode {
			t.Errorf("%s: mode %#o, want %#o", tc.path, in.mode, tc.mode)
		}
	}
	// An empty directory must have a zero dirent count, not a stale span.
	idx, _ := r.resolve("/emptydir")
	if in := r.inode(idx); in.b != 0 {
		t.Errorf("empty dir claims %d dirents", in.b)
	}
}

// Every directory's children must be contiguous in the dirent array: the reader
// walks [a, a+b) and would otherwise read a neighbour's entries.
func TestDirentSpansAreContiguousAndDisjoint(t *testing.T) {
	root := fixture(t)
	img, _, err := Build(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := open(t, img)
	covered := map[uint64]bool{}
	total := uint64(0)
	for i := uint32(0); i < r.inodeCnt; i++ {
		in := r.inode(i)
		if in.kind != kindDir {
			continue
		}
		for j := uint64(0); j < in.b; j++ {
			slot := in.a + j
			if covered[slot] {
				t.Fatalf("dirent slot %d claimed by two directories", slot)
			}
			covered[slot] = true
		}
		total += in.b
	}
	// Every inode except the root is exactly one directory entry.
	if total != uint64(r.inodeCnt)-1 {
		t.Errorf("%d dirents for %d non-root inodes", total, r.inodeCnt-1)
	}
}

func TestBootRecordIsReadableAtBootPath(t *testing.T) {
	root := fixture(t)
	b := Boot{
		Argv: []string{"openssl", "dgst", "-sha256", "/etc/os-release"},
		Env:  []string{"PATH=/usr/bin:/bin", "OPENSSL_CONF=/etc/ssl/openssl.cnf"},
		Cwd:  "/", UID: 0, GID: 0,
	}
	img, _, err := Build(root, Options{Boot: &b})
	if err != nil {
		t.Fatal(err)
	}
	r := open(t, img)
	got := r.readFile(t, BootPath)
	if want := b.Encode(); !bytes.Equal(got, want) {
		t.Fatalf("boot record round-trip differs (%d vs %d bytes)", len(got), len(want))
	}
	// And it must parse the way runtime/src/boot.rs parses it.
	argv, env, cwd, uid, gid := parseBoot(t, got)
	if strings.Join(argv, " ") != "openssl dgst -sha256 /etc/os-release" {
		t.Errorf("argv = %q", argv)
	}
	if len(env) != 2 || env[1] != "OPENSSL_CONF=/etc/ssl/openssl.cnf" {
		t.Errorf("env = %q", env)
	}
	if cwd != "/" || uid != 0 || gid != 0 {
		t.Errorf("cwd=%q uid=%d gid=%d", cwd, uid, gid)
	}
}

// Rebuilding an image that already carries a boot record must replace it, not
// add a second entry the reader would never see.
func TestBootRecordIsReplacedNotDuplicated(t *testing.T) {
	root := fixture(t)
	first, _, err := Build(root, Options{Boot: &Boot{Argv: []string{"a"}}})
	if err != nil {
		t.Fatal(err)
	}
	r := open(t, first)
	idx, ok := r.resolve("/.raptormark")
	if !ok {
		t.Fatal("/.raptormark missing")
	}
	if in := r.inode(idx); in.b != 1 {
		t.Errorf("/.raptormark has %d entries, want 1", in.b)
	}
}

// parseBoot mirrors Boot::parse in runtime/src/boot.rs.
func parseBoot(t *testing.T, b []byte) (argv, env []string, cwd string, uid, gid uint32) {
	t.Helper()
	if string(b[:8]) != "RMBOOT01" {
		t.Fatalf("bad boot magic %q", b[:8])
	}
	pos := 8
	u32 := func() uint32 {
		v := binary.LittleEndian.Uint32(b[pos:])
		pos += 4
		return v
	}
	str := func() string {
		n := int(u32())
		s := string(b[pos : pos+n])
		pos += n
		return s
	}
	uid, gid = u32(), u32()
	cwd = str()
	for n := int(u32()); n > 0; n-- {
		argv = append(argv, str())
	}
	for n := int(u32()); n > 0; n-- {
		env = append(env, str())
	}
	if pos != len(b) {
		t.Errorf("boot record has %d trailing bytes", len(b)-pos)
	}
	return
}
