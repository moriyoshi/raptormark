package rootfs

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"raptormark/internal/link"
)

// The sidecar map paths exist in THREE places, and only two of them were pinned.
//
// # ❗ How this gap was found, because it is the interesting part
//
// `internal/rootfs/boot.go` carried a comment dated 2026-08-23 saying the
// dlopen map was "A PRODUCER WITH NO CONSUMER -- the runtime does not read it
// yet", and that `TestDlPathAgrees` "pins the two that exist today". Both halves
// went stale together: the runtime side landed (`runtime/src/dlmap.rs`
// defines `DL_PATH` and `context.rs` reads it at boot), so there are now three
// copies of each path -- and the third, the one in Rust, was guarded by nothing.
//
// The comment was accurate when written. That is exactly what makes it
// dangerous: it said "the two that exist today" and nobody re-read it on the day
// a third appeared.
//
// # What drift would actually look like
//
// Nothing would fail to build, and no test would fail. `Vfs::read` would return
// None for a path nothing wrote, `DlMap::load` would get `None`, and every guest
// `dlopen` would fail with "cannot open shared object file".
//
// ⚠️ That symptom is ALREADY DOCUMENTED IN `AGENTS.md` AS COMING FROM A
// DIFFERENT CAUSE -- a `RAPTORMARK_ROOTFS` that names a host path instead of a
// guest one. So the diagnosis would be led straight to the wrong subsystem,
// which is worse than an unexplained failure.
//
// # Why a source scan rather than a shared definition
//
// There is no build step that could hand a Rust `const` to Go or the reverse:
// the two are compiled by different toolchains, one of them inside a container.
// `//runtime:cshim_equivalence_test` solves the same problem the same way, by
// comparing against a transcription rather than by sharing a definition.
//
// ❌ It must FAIL, never skip, when the constant cannot be found. A skip here
// would recreate precisely the silence this file exists to remove -- and this
// tree has already paid for that twice in one week, with a lost fixture that
// announced itself as a skip and a lost directory that announced itself as
// nothing at all.

// rustByteConst extracts a byte-string constant from Rust source, in either of
// the two forms this tree uses: `pub const N: &[u8] = b"..."` (the sidecar map
// paths) and `const N: &[u8; K] = b"..."` (the rfs magic, which is fixed-width
// and private to its module).
//
// ⚠️ One pattern covering both is deliberate. Two patterns means two ways to
// stop matching, and a pattern that stops matching passes VACUOUSLY -- the
// failure mode the rename neutralization below exists to catch.
var rustByteConst = regexp.MustCompile(`const ([A-Z_]+): &\[u8(?:; *\d+)?\] = b"([^"]*)";`)

// repoRoot resolves the tree from this package's location.
//
// `internal/rootfs` is tagged `manual` in Bazel precisely because it reads the
// repo by relative path, so this is the established arrangement here rather
// than a new liberty.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "..", "..")
}

// rustConst reads one `pub const NAME: &[u8] = b"..."` out of a Rust file.
func rustConst(t *testing.T, relPath, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), relPath))
	if err != nil {
		t.Fatalf("reading %s: %v\n"+
			"This test pins a Rust constant against its Go twins by scanning the "+
			"source. If the file moved, the constants are no longer being compared "+
			"at all -- fix the path rather than deleting the test.", relPath, err)
	}
	for _, m := range rustByteConst.FindAllStringSubmatch(string(b), -1) {
		if m[1] == name {
			return m[2]
		}
	}
	t.Fatalf("%s does not define `pub const %s: &[u8] = b\"...\"`.\n"+
		"Either it was renamed or its form changed. ❌ Do not relax the pattern "+
		"until you have checked which: a pattern that matches nothing passes "+
		"vacuously, and this test exists because an unguarded copy of this "+
		"constant is invisible until every guest dlopen fails.", relPath, name)
	return ""
}

// TestRuntimeAgreesOnTheSidecarABI is the third leg the Go-only agreement tests
// were missing: every value the Go writer and the Rust reader must share.
//
// Each case carries what DRIFT WOULD COST, because the three fail in visibly
// different ways and only one of them is loud.
//
// It compares against `link.*` rather than this package's own constants because
// `TestDlPathAgrees` already pins rootfs against link; chaining through the same
// anchor means all four definitions are tied to one value rather than to each
// other pairwise.
func TestRuntimeAgreesOnTheSidecarABI(t *testing.T) {
	for _, c := range []struct {
		file, name, want, cost string
	}{
		{"runtime/src/execmap.rs", "EXEC_PATH", link.ExecPath,
			"the runtime reads no exec map, so every execve falls back to program 0 -- " +
				"a silent wrong-program run that AGENTS.md records as having caused four incidents"},
		{"runtime/src/dlmap.rs", "DL_PATH", link.DlPath,
			"the runtime reads no dlopen map, so every guest dlopen fails with " +
				"\"cannot open shared object file\" -- which AGENTS.md attributes to a bad " +
				"RAPTORMARK_ROOTFS, leading the diagnosis to the wrong subsystem"},
		// ❗ The rfs MAGIC, which was pinned only against a hand-copy. `rfs.go`
		// says "Keep the two in lockstep -- the reader is the specification",
		// and `rfs_test.go` implements a reader "transcribed from
		// runtime/src/vfs/rfs.rs". A transcription is a genuinely useful
		// independent implementation, but it is a SNAPSHOT: if the real reader's
		// magic changed, the Go test would keep passing against its own copy
		// while every sidecar was rejected at boot.
		{"runtime/src/vfs/rfs.rs", "MAGIC", magic,
			"the runtime rejects the sidecar outright at boot -- the loudest of the three, " +
				"and the only one that fails immediately rather than at first use"},
	} {
		got := rustConst(t, c.file, c.name)
		if got != c.want {
			t.Errorf("%s defines %s = %q, but the Go side writes %q.\nIf they drift, %s.",
				c.file, c.name, got, c.want, c.cost)
		}
	}
}

// TestTheSidecarMapPathsAreDistinct guards the other way a triplicated constant
// goes wrong: two of them converging. The Go pair is checked by
// `TestDlPathAgrees`; this covers the Rust side, where nothing was looking.
func TestTheSidecarMapPathsAreDistinct(t *testing.T) {
	exec := rustConst(t, "runtime/src/execmap.rs", "EXEC_PATH")
	dl := rustConst(t, "runtime/src/dlmap.rs", "DL_PATH")
	if exec == dl {
		t.Errorf("the runtime reads the exec map and the dlopen map from the same "+
			"path %q, so one of them silently gets the other's bytes", exec)
	}
}

// The rfs header LAYOUT, which is inline integer literals on both sides.
//
// The Go writer puts ten fields at fixed offsets; the Rust reader picks six of
// them back out with `u32le(&data, N)` / `u64le(&data, N)`. Neither side names
// those offsets, so a field that MOVES is caught by nothing: the reader keeps
// reading the old offset and gets whatever now lives there.
//
// ❗ That is worse than a parse failure. Reading `dirent_off` where `name_off`
// was expected yields a plausible-looking number, and the guest sees a VFS whose
// files have the wrong contents rather than a sidecar that fails to load.
//
// ⚠️ This deliberately does NOT try to map Rust field names to Go ones -- that
// correspondence would itself be a transcription, and a wrong transcription
// passes. It asserts the weaker, checkable thing: every offset the reader reads
// is an offset the writer WRITES. If a Go field moves or a new one shifts the
// others, some reader offset stops being in the written set and this fires.
var (
	rustHeaderRead = regexp.MustCompile(`u(?:32|64)le\(&data, (\d+)\)`)
	goHeaderWrite  = regexp.MustCompile(`binary\.LittleEndian\.PutUint(?:32|64)\(b\[(\d+):\]`)
)

func TestTheRfsHeaderOffsetsAgree(t *testing.T) {
	rustSrc := mustRead(t, "runtime/src/vfs/rfs.rs")
	goSrc := mustRead(t, "internal/rootfs/rfs.go")

	written := map[int]bool{0: true} // offset 0 is the magic, written by `copy`
	for _, m := range goHeaderWrite.FindAllStringSubmatch(goSrc, -1) {
		n, _ := strconv.Atoi(m[1])
		written[n] = true
	}
	read := map[int]bool{}
	for _, m := range rustHeaderRead.FindAllStringSubmatch(rustSrc, -1) {
		n, _ := strconv.Atoi(m[1])
		read[n] = true
	}
	// ❌ Empty on either side is breakage, not agreement.
	if len(written) <= 1 || len(read) == 0 {
		t.Fatalf("header scan found %d written and %d read offsets; a pattern "+
			"matched nothing and this guard is comparing empty sets", len(written)-1, len(read))
	}
	for off := range read {
		if !written[off] {
			t.Errorf("runtime/src/vfs/rfs.rs reads the header at offset %d, but "+
				"internal/rootfs/rfs.go writes no field there.\nThe reader would pick up "+
				"whatever now occupies that offset -- a plausible number, and a guest VFS "+
				"whose files have the wrong contents rather than a sidecar that fails to load.",
				off)
		}
	}
	t.Logf("rfs header: reader takes %d offsets, all within the writer's %d", len(read), len(written)-1)
}

// TestTheRfsHeaderLengthAgrees pins the reader's minimum against the writer's
// fixed size. They are separate literals: 80 in `rfs.rs`'s bounds check,
// `headerLen` in `rfs.go`.
func TestTheRfsHeaderLengthAgrees(t *testing.T) {
	m := regexp.MustCompile(`data\.len\(\) < (\d+)`).FindStringSubmatch(
		mustRead(t, "runtime/src/vfs/rfs.rs"))
	if m == nil {
		t.Fatal("runtime/src/vfs/rfs.rs no longer bounds-checks with `data.len() < N`; " +
			"the header length is no longer being compared at all")
	}
	got, _ := strconv.Atoi(m[1])
	if got != headerLen {
		t.Errorf("the reader requires %d header bytes, the writer emits %d", got, headerLen)
	}
}

// mustRead reads a repo-relative file, failing loudly rather than skipping.
func mustRead(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}
