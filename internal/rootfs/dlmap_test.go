package rootfs

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"raptormark/internal/link"
)

// Three copies of one constant -- here, internal/link, and the runtime. A drift
// makes every dlopen of a unit fail with nothing naming the path.
func TestDlPathAgrees(t *testing.T) {
	if DlPath != link.DlPath {
		t.Errorf("rootfs.DlPath = %q, link.DlPath = %q", DlPath, link.DlPath)
	}
	if DlPath == ExecPath {
		t.Errorf("the dlopen map and the exec map would be written to the same path %q", DlPath)
	}
}

// A plugin rootfs, with the .so files present because Build requires every map
// path to be canonical in the image -- and a path the image does not contain
// cannot be.
func pluginTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "usr/lib/pg"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"pgcrypto.so", "amcheck.so"} {
		if err := os.WriteFile(filepath.Join(dir, "usr/lib/pg", n), []byte("ELF"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestBuildPlacesTheDlMap(t *testing.T) {
	dir := pluginTree(t)
	dlMap, err := link.DlMap(
		[]link.Program{{Name: "pgc", Index: 0}, {Name: "amc", Index: 1}},
		[]link.DlEntry{
			{Path: "/usr/lib/pg/pgcrypto.so", Hash: "pgc"},
			{Path: "/usr/lib/pg/amcheck.so", Hash: "amc"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := Build(dir, Options{DlMap: dlMap})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	r := open(t, img)
	idx, ok := r.resolve(DlPath)
	if !ok {
		t.Fatalf("%s is absent from the image", DlPath)
	}
	if in := r.inode(idx); in.kind != kindFile {
		t.Fatalf("%s has kind %d, want a file", DlPath, in.kind)
	}
	if got := r.readFile(t, DlPath); !bytes.Equal(got, dlMap) {
		t.Errorf("dlopen map round-tripped as %d bytes, want %d", len(got), len(dlMap))
	}
	// The exec map must NOT appear: the two are independent, and an image with
	// plugins but one entry point is ordinary.
	if _, ok := r.resolve(ExecPath); ok {
		t.Errorf("%s appeared although no ExecMap was given", ExecPath)
	}
}

func TestBuildOmitsTheDlMapWhenUnset(t *testing.T) {
	img, _, err := Build(pluginTree(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := open(t, img).resolve(DlPath); ok {
		t.Errorf("%s appeared in an image built without one", DlPath)
	}
}

// A non-canonical path never matches, so the guest reports a plugin absent that
// is present and translated. Build must refuse it rather than ship it.
func TestBuildRejectsANonCanonicalDlMap(t *testing.T) {
	dir := pluginTree(t)
	// usr-merged: /lib -> usr/lib, so /lib/pg/... resolves to /usr/lib/pg/...
	if err := os.Symlink("usr/lib", filepath.Join(dir, "lib")); err != nil {
		t.Fatal(err)
	}
	dlMap, err := link.DlMap(
		[]link.Program{{Name: "pgc", Index: 0}},
		[]link.DlEntry{{Path: "/lib/pg/pgcrypto.so", Hash: "pgc"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = Build(dir, Options{DlMap: dlMap})
	if err == nil {
		t.Fatal("Build accepted a non-canonical dlopen map; every dlopen through it would report the plugin absent")
	}
	if !strings.Contains(err.Error(), "canonical") {
		t.Errorf("error %q does not say what is wrong", err)
	}

	// And the canonical form of the SAME map is accepted -- without this the
	// test above passes for a Build that refuses every dlopen map.
	fixed, bad, err := CanonicalDlEntries(dir, []link.DlEntry{{Path: "/lib/pg/pgcrypto.so", Hash: "pgc"}})
	if err != nil || len(bad) != 0 {
		t.Fatalf("CanonicalDlEntries: %v %v", err, bad)
	}
	if fixed[0].Path != "/usr/lib/pg/pgcrypto.so" {
		t.Fatalf("canonicalised to %q", fixed[0].Path)
	}
	enc, err := link.DlMap([]link.Program{{Name: "pgc", Index: 0}}, fixed)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Build(dir, Options{DlMap: enc}); err != nil {
		t.Errorf("Build rejected the canonical form too: %v", err)
	}
}

// Two spellings of one file must not become two entries claiming one path.
func TestCanonicalDlEntriesCollapsesAliases(t *testing.T) {
	dir := pluginTree(t)
	if err := os.Symlink("usr/lib", filepath.Join(dir, "lib")); err != nil {
		t.Fatal(err)
	}
	got, bad, err := CanonicalDlEntries(dir, []link.DlEntry{
		{Path: "/usr/lib/pg/pgcrypto.so", Hash: "pgc"},
		{Path: "/lib/pg/pgcrypto.so", Hash: "pgc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("two spellings of one file produced %d entries; link.DlMap would refuse them", len(got))
	}
	if len(bad) != 1 || !strings.Contains(bad[0], "already claimed") {
		t.Errorf("the dropped alias was not reported: %v", bad)
	}
}
