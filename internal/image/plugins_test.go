package image

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The JIT policy, apart from the ELF reading.
//
// The names are the ones measured on the real postgres:17 rootfs: of its 79
// extensions, `llvmjit.so` is the ONLY one naming libLLVM, and it names it
// directly. A wrong answer here is silent either way -- it excludes a plugin
// that works, or admits one that drags in 143 MiB and still cannot run.
func TestIsJITPluginMatchesTheVersionedSoname(t *testing.T) {
	for _, c := range []struct {
		name   string
		needed []string
		want   bool
	}{
		{"llvmjit.so as measured", []string{
			"libLLVM.so.19.1", "libstdc++.so.6", "libgcc_s.so.1", "libc.so.6",
			"ld-linux-aarch64.so.1"}, true},
		// The version moves with every LLVM release, so equality would stop
		// matching at the next one and the exclusion would silently lapse.
		{"a different LLVM version", []string{"libLLVM.so.21.0"}, true},
		{"unversioned", []string{"libLLVM.so"}, true},
		{"an ordinary extension", []string{"libc.so.6", "libssl.so.3"}, false},
		{"no dependencies", nil, false},
		// Must not fire on unrelated names that merely contain the substring:
		// the rule is a PREFIX on the soname.
		{"a library that only mentions llvm", []string{"libmyllvmhelper.so.1"}, false},
		{"z3 alone is not a JIT", []string{"libz3.so.4"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := IsJITPlugin(c.needed); got != c.want {
				t.Errorf("IsJITPlugin(%v) = %v, want %v", c.needed, got, c.want)
			}
		})
	}
}

// A rootfs with a plugin directory PluginDirs recognises.
func pgTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "usr/lib/postgresql/17/lib")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// THE regression this file exists for.
//
// The first version of Plugins resolved the root to an absolute path but passed
// the CALLER's spelling to PluginDirs, so the walked paths were relative while
// guestPath compared them against an absolute root. Every filepath.Rel failed,
// and a failure meant "skip", so a rootfs holding 79 plugins reported
// "discovered 0 plugin(s), excluded 0" -- clean, plausible and wrong.
func TestPluginsAgreeOnRelativeAndAbsoluteRoots(t *testing.T) {
	root := pgTree(t)
	so := filepath.Join(root, "usr/lib/postgresql/17/lib/thing.so")
	if err := os.WriteFile(so, []byte("not an elf"), 0o755); err != nil {
		t.Fatal(err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, root)
	if err != nil {
		t.Skipf("no relative path from %s to %s", wd, root)
	}

	af, ax, err := Plugins(root)
	if err != nil {
		t.Fatal(err)
	}
	rf, rx, err := Plugins(rel)
	if err != nil {
		t.Fatal(err)
	}
	if len(af)+len(ax) == 0 {
		t.Fatal("the absolute root found nothing at all, so the comparison is vacuous")
	}

	// ⚠️ COMPARE THE PATHS, NOT THE COUNTS. Counts do not distinguish the two
	// behaviours: with the bug present the relative call still produces one
	// entry, it is just filed under a broken guest path ("../../tmp/...") with a
	// different reason. The first version of this test compared lengths, passed
	// under a deliberately reintroduced bug, and was only caught by running the
	// neutralization.
	names := func(f []Plugin, x []ExcludedPlugin) []string {
		var out []string
		for _, p := range f {
			out = append(out, "found:"+p.Guest)
		}
		for _, e := range x {
			out = append(out, "excluded:"+e.Guest)
		}
		sort.Strings(out)
		return out
	}
	a, r := names(af, ax), names(rf, rx)
	if !slices.Equal(a, r) {
		t.Errorf("the two spellings of one rootfs disagree:\n absolute %v\n relative %v", a, r)
	}
	// Every guest path must be rooted, or it is not a guest path at all.
	for _, n := range a {
		if p := strings.SplitN(n, ":", 2)[1]; !strings.HasPrefix(p, "/") {
			t.Errorf("%q is not an absolute guest path", p)
		}
	}
}

// A file that is not a shared object must be REPORTED, not dropped. A silently
// empty plugin list is indistinguishable from an image with no plugins.
func TestPluginsReportsWhatItRefuses(t *testing.T) {
	root := pgTree(t)
	dir := filepath.Join(root, "usr/lib/postgresql/17/lib")
	if err := os.WriteFile(filepath.Join(dir, "junk.so"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Not named like a shared object at all: must not appear in EITHER list.
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	found, excluded, err := Plugins(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("a non-ELF file was admitted as a plugin: %v", found)
	}
	if len(excluded) != 1 {
		t.Fatalf("expected exactly the one .so to be reported, got %v", excluded)
	}
	if !strings.HasSuffix(excluded[0].Guest, "/junk.so") {
		t.Errorf("reported %q, want junk.so", excluded[0].Guest)
	}
	if !strings.Contains(excluded[0].Reason, "aarch64") {
		t.Errorf("reason %q does not say why", excluded[0].Reason)
	}
}

// CPython names extensions `_socket.cpython-311-aarch64-linux-gnu.so`, and a
// versioned library is `libfoo.so.3`. A HasSuffix(".so") check misses the
// second, and matching the whole final extension misses the first.
func TestSharedObjectNameSpellings(t *testing.T) {
	for _, c := range []struct {
		name string
		want bool
	}{
		{"pgcrypto.so", true},
		{"_socket.cpython-311-aarch64-linux-gnu.so", true},
		{"libfoo.so.3", true},
		{"libfoo.so.3.1.4", true},
		{"README", false},
		{"notes.sol", false},
		{"archive.tar.gz", false},
	} {
		if got := isSharedObjectName(c.name); got != c.want {
			t.Errorf("isSharedObjectName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
