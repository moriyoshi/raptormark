package rootfs

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"raptormark/internal/link"
)

// The three copies of this path must agree or the runtime silently finds no
// exec map and falls back to program 0 -- an execve of a second program would
// then run the first one instead, which is a wrong answer rather than an error.
// runtime/src/execmap.rs holds the third copy and cannot be checked from here.
func TestExecPathAgrees(t *testing.T) {
	if ExecPath != link.ExecPath {
		t.Errorf("rootfs.ExecPath = %q, link.ExecPath = %q", ExecPath, link.ExecPath)
	}
}

// Build must place the exec map where the runtime looks for it. Nothing in the
// tree produced one into a sidecar before 2026-08-09: the encoder existed in
// internal/link and the consumer in runtime/src/execmap.rs, with no producer in
// between, so a multi-program module could be linked but never exercised.
func TestBuildPlacesTheExecMap(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello"), []byte("hi"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The two programs exist as files because Build now requires every exec map
	// path to be canonical in the image, and a path the image does not contain
	// cannot be. That is faithful rather than merely necessary: the runtime
	// resolves an execve through the VFS before consulting the map, so a real
	// image always has them.
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"alpha", "beta"} {
		if err := os.WriteFile(filepath.Join(dir, "bin", n), []byte("ELF"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	execMap, err := link.ExecMap(
		[]link.Program{{Name: "alpha", Index: 0}, {Name: "beta", Index: 1}},
		[]link.ExecEntry{{Path: "/bin/alpha", Hash: "alpha"}, {Path: "/bin/beta", Hash: "beta"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := Build(dir, Options{ExecMap: execMap})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	r := open(t, img)
	idx, ok := r.resolve(ExecPath)
	if !ok {
		t.Fatalf("%s is absent from the image", ExecPath)
	}
	if in := r.inode(idx); in.kind != kindFile {
		t.Fatalf("%s has kind %d, want a file", ExecPath, in.kind)
	}
	got := r.readFile(t, ExecPath)
	if !bytes.Equal(got, execMap) {
		t.Errorf("exec map round-tripped as %d bytes, want %d", len(got), len(execMap))
	}
}

// Omitting it must leave no stray entry, so a single-program image keeps the
// "no exec map -> fall back to program 0" path the runtime documents.
func TestBuildOmitsTheExecMapWhenUnset(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello"), []byte("hi"), 0o755); err != nil {
		t.Fatal(err)
	}
	img, _, err := Build(dir, Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := open(t, img).resolve(ExecPath); ok {
		t.Errorf("%s present in an image built without one", ExecPath)
	}
}

// The bug this check exists for, at the level Build sees it. A map keyed on the
// path the pipeline used to register is refused, and the message names the path
// that would have worked -- the whole point being that the old behaviour was to
// accept it and produce an image where every exec silently ran program 0.
func TestBuildRejectsANonCanonicalExecMap(t *testing.T) {
	root := usrMerged(t)
	execMap, err := link.ExecMap(
		[]link.Program{{Name: "dash", Index: 0}},
		[]link.ExecEntry{{Path: "/bin/dash", Hash: "dash"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = Build(root, Options{ExecMap: execMap})
	if err == nil {
		t.Fatal("Build accepted a non-canonical exec map")
	}
	for _, want := range []string{"/bin/dash", "/usr/bin/dash"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should name %q, got: %v", want, err)
		}
	}
}

// The other half, without which the test above would also pass if Build simply
// refused every exec map: the canonical form of the SAME map is accepted.
func TestBuildAcceptsACanonicalExecMap(t *testing.T) {
	root := usrMerged(t)
	execMap, err := link.ExecMap(
		[]link.Program{{Name: "dash", Index: 0}},
		[]link.ExecEntry{{Path: "/usr/bin/dash", Hash: "dash"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := Build(root, Options{ExecMap: execMap})
	if err != nil {
		t.Fatalf("Build rejected a canonical exec map: %v", err)
	}
	if got := open(t, img).readFile(t, ExecPath); !bytes.Equal(got, execMap) {
		t.Errorf("exec map round-tripped as %d bytes, want %d", len(got), len(execMap))
	}
}

// Every bad path is reported. One loop over one list produces one mistake in
// every entry, and finding that out one build at a time is the slowest way.
func TestBuildReportsEveryBadExecMapPath(t *testing.T) {
	root := usrMerged(t)
	symlink(t, root, "dash", "usr/bin/sh2")
	execMap, err := link.ExecMap(
		[]link.Program{{Name: "dash", Index: 0}},
		[]link.ExecEntry{{Path: "/bin/dash", Hash: "dash"}, {Path: "/bin/sh2", Hash: "dash"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = Build(root, Options{ExecMap: execMap})
	if err == nil {
		t.Fatal("Build accepted two non-canonical paths")
	}
	for _, want := range []string{"/bin/dash", "/bin/sh2", "2 exec map path"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should name %q, got: %v", want, err)
		}
	}
}

// What CanonicalExecEntries produces is what Build accepts. The two are only
// useful as a pair, and nothing else pins that they agree.
func TestCanonicalisedEntriesSatisfyBuild(t *testing.T) {
	root := usrMerged(t)
	entries, err := CanonicalExecEntries(root, []link.ExecEntry{{Path: "/bin/sh", Hash: "dash"}})
	if err != nil {
		t.Fatal(err)
	}
	execMap, err := link.ExecMap([]link.Program{{Name: "dash", Index: 0}}, entries)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Build(root, Options{ExecMap: execMap}); err != nil {
		t.Fatalf("Build rejected canonicalised entries: %v", err)
	}
}
