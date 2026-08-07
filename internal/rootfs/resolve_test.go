package rootfs

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"raptormark/internal/link"
)

// usrMerged builds the shape that produced the bug: a Debian-style usr-merged
// image where /bin is a relative symlink to usr/bin and /usr/bin/sh is a
// relative symlink to dash. Returns the image root.
func usrMerged(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mkdir(t, root, "usr/bin")
	write(t, root, "usr/bin/dash", "ELF")
	symlink(t, root, "dash", "usr/bin/sh")
	symlink(t, root, "usr/bin", "bin")
	return root
}

func mkdir(t *testing.T, root, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, root, p, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, p), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, root, target, p string) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(root, p)); err != nil {
		t.Fatal(err)
	}
}

// The headline case. /bin/sh is what libc spawns, and on a usr-merged image it
// is two symlinks away from the program that actually runs.
func TestResolveFollowsBothSymlinksToTheRealProgram(t *testing.T) {
	root := usrMerged(t)
	got, err := Resolve(root, "/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/usr/bin/dash" {
		t.Fatalf("Resolve(/bin/sh) = %q, want /usr/bin/dash", got)
	}
}

// The final component is followed, which is the part `Programs::resolve`
// depends on: it passes follow_final = true.
func TestResolveFollowsTheFinalComponent(t *testing.T) {
	root := usrMerged(t)
	got, err := Resolve(root, "/usr/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/usr/bin/dash" {
		t.Fatalf("Resolve(/usr/bin/sh) = %q, want /usr/bin/dash", got)
	}
}

// An already-canonical path is returned unchanged, so canonicalising twice is
// the same as canonicalising once.
func TestResolveIsIdempotentOnACanonicalPath(t *testing.T) {
	root := usrMerged(t)
	got, err := Resolve(root, "/usr/bin/dash")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/usr/bin/dash" {
		t.Fatalf("Resolve(/usr/bin/dash) = %q, want it unchanged", got)
	}
}

// An ABSOLUTE symlink target names the guest root. Both halves matter and they
// fail in opposite directions, so both are checked here:
//
//   - a target that exists only in the IMAGE must resolve (a host-rooted
//     implementation would report it missing), and
//   - a target that exists only on the HOST must NOT (a host-rooted
//     implementation would happily resolve it and put a host path in the map).
//
// /etc/passwd is the second case: present on any Linux host running this test,
// deliberately absent from the image built here.
func TestResolveRerootsAnAbsoluteSymlinkTargetAtTheImage(t *testing.T) {
	root := usrMerged(t)
	mkdir(t, root, "rapt")
	write(t, root, "rapt/only", "in the image alone")
	symlink(t, root, "/rapt/only", "usr/bin/inimage")
	symlink(t, root, "/etc/passwd", "usr/bin/onhost")

	got, err := Resolve(root, "/bin/inimage")
	if err != nil {
		t.Fatalf("an absolute target inside the image must resolve: %v", err)
	}
	if got != "/rapt/only" {
		t.Fatalf("Resolve(/bin/inimage) = %q, want /rapt/only", got)
	}

	if _, err := os.Lstat("/etc/passwd"); err != nil {
		t.Skip("this host has no /etc/passwd; the escape half of this test cannot run")
	}
	if got, err := Resolve(root, "/bin/onhost"); err == nil {
		t.Fatalf("Resolve(/bin/onhost) = %q, want an error: /etc/passwd is not in the image", got)
	}
}

// ".." is lexical on the resolved prefix and never climbs above the image root,
// so no entry can name a host path by walking up.
func TestResolveNeverClimbsAboveTheImageRoot(t *testing.T) {
	root := usrMerged(t)
	for _, p := range []string{"/usr/../bin/sh", "/../bin/sh", "/usr/bin/../../../../bin/sh"} {
		got, err := Resolve(root, p)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", p, err)
		}
		if got != "/usr/bin/dash" {
			t.Fatalf("Resolve(%q) = %q, want /usr/bin/dash", p, got)
		}
	}
}

// A path the image does not contain is an error, not a pass-through. The
// runtime's resolve returns None here, and an entry the runtime cannot resolve
// is exactly the silent fallback this whole check exists to prevent.
func TestResolveRejectsAPathTheImageDoesNotContain(t *testing.T) {
	root := usrMerged(t)
	for _, p := range []string{"/bin/nosuch", "/nosuch/dir/sh", "/usr/bin/dash/deeper"} {
		if got, err := Resolve(root, p); err == nil {
			t.Fatalf("Resolve(%q) = %q, want an error", p, got)
		}
	}
}

func TestResolveRejectsARelativePath(t *testing.T) {
	root := usrMerged(t)
	if got, err := Resolve(root, "bin/sh"); err == nil {
		t.Fatalf("Resolve(bin/sh) = %q, want an error", got)
	}
}

// A symlink cycle terminates instead of spinning, at the same depth the runtime
// gives up at.
func TestResolveBoundsASymlinkCycle(t *testing.T) {
	root := t.TempDir()
	symlink(t, root, "b", "a")
	symlink(t, root, "a", "b")
	got, err := Resolve(root, "/a")
	if err == nil {
		t.Fatalf("Resolve(/a) = %q, want an error", got)
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error should name the symlink limit, got: %v", err)
	}
}

// maxLinks is meaningless on its own: it is a claim ABOUT runtime/src/vfs/mod.rs,
// so it is checked against that file rather than against itself.
//
// Written after the first version of the boundary test below failed to catch
// anything. That test spelled the limit `maxLinks`, so changing the constant
// moved the test with it and both directions passed -- it pinned the limit's
// self-consistency, which nothing threatens, and not its agreement with the
// runtime, which is the entire point. The literal 40 below is deliberate for
// the same reason.
func TestMaxLinksMatchesTheRuntime(t *testing.T) {
	const runtimeSrc = "../../runtime/src/vfs/mod.rs"
	b, err := os.ReadFile(runtimeSrc)
	if err != nil {
		t.Fatalf("the runtime resolver is the specification for this file: %v", err)
	}
	m := regexp.MustCompile(`(?m)^const MAX_LINKS: u32 = (\d+);`).FindSubmatch(b)
	if m == nil {
		t.Fatalf("MAX_LINKS not found in %s; if it was renamed, this constant needs re-checking, not deleting", runtimeSrc)
	}
	want, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	if maxLinks != want {
		t.Fatalf("maxLinks = %d but runtime MAX_LINKS = %d: a host limit that disagrees "+
			"either accepts a chain the guest refuses or rejects an image that works", maxLinks, want)
	}
}

// Both sides of the limit, spelled as literals so that moving `maxLinks` fails
// here rather than silently moving the test with it. Exactly 40 resolves; 41
// does not.
func TestResolveAcceptsExactly40LinksAndNoMore(t *testing.T) {
	// chain builds `n` symlinks ending at a real file and returns the head.
	chain := func(t *testing.T, n int) (string, string) {
		t.Helper()
		root := t.TempDir()
		write(t, root, "end", "x")
		prev := "end"
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("l%03d", i)
			symlink(t, root, prev, name)
			prev = name
		}
		return root, prev
	}

	root, head := chain(t, 40)
	got, err := Resolve(root, "/"+head)
	if err != nil {
		t.Fatalf("a chain of exactly 40 links must resolve: %v", err)
	}
	if got != "/end" {
		t.Fatalf("Resolve(/%s) = %q, want /end", head, got)
	}

	root, head = chain(t, 41)
	if got, err := Resolve(root, "/"+head); err == nil {
		t.Fatalf("a chain of 41 links must not resolve, got %q", got)
	}
}

// The bug, at the level the caller sees it: an entry written the way the
// pipeline used to write it comes back keyed on the path the runtime will
// actually look up.
func TestCanonicalExecEntriesRewritesANonCanonicalPath(t *testing.T) {
	root := usrMerged(t)
	got, err := CanonicalExecEntries(root, []link.ExecEntry{{Path: "/bin/dash", Hash: "h1"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []link.ExecEntry{{Path: "/usr/bin/dash", Hash: "h1"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Two spellings of one program collapse to one entry rather than tripping
// ExecMap's duplicate-path check.
func TestCanonicalExecEntriesDropsADuplicateNamingTheSameProgram(t *testing.T) {
	root := usrMerged(t)
	got, err := CanonicalExecEntries(root, []link.ExecEntry{
		{Path: "/bin/sh", Hash: "h1"},
		{Path: "/usr/bin/dash", Hash: "h1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "/usr/bin/dash" {
		t.Fatalf("got %v, want a single /usr/bin/dash entry", got)
	}
	// And the result is something ExecMap accepts, which is the point of
	// collapsing rather than passing both through.
	if _, err := link.ExecMap([]link.Program{{Name: "h1", Index: 0}}, got); err != nil {
		t.Fatalf("canonicalised entries must encode: %v", err)
	}
}

// Two spellings that collapse onto one path while naming DIFFERENT programs
// cannot both be honoured. Refusing beats picking one: the image cannot express
// what the caller asked for, and silently choosing is the same class of failure
// as the non-canonical entry itself.
func TestCanonicalExecEntriesRejectsACollisionBetweenDifferentPrograms(t *testing.T) {
	root := usrMerged(t)
	_, err := CanonicalExecEntries(root, []link.ExecEntry{
		{Path: "/bin/sh", Hash: "h1"},
		{Path: "/usr/bin/dash", Hash: "h2"},
	})
	if err == nil {
		t.Fatal("want an error when two spellings of one path name different programs")
	}
	for _, want := range []string{"/bin/sh", "/usr/bin/dash", "h1", "h2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should name %q so the caller can find both entries, got: %v", want, err)
		}
	}
}

// An entry naming a path the image does not contain fails the build instead of
// shipping a map with an unreachable key.
func TestCanonicalExecEntriesRejectsAPathOutsideTheImage(t *testing.T) {
	root := usrMerged(t)
	if _, err := CanonicalExecEntries(root, []link.ExecEntry{{Path: "/bin/nosuch", Hash: "h1"}}); err == nil {
		t.Fatal("want an error for a path the image does not contain")
	}
}
