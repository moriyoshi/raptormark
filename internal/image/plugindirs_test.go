package image

import (
	"os"
	"path/filepath"
	"testing"
)

// A glob that matches nothing must be skipped rather than returned, and only
// DIRECTORIES may come back -- a file named like a plugin directory would be
// handed to the fuser as a directory and fail there instead of here.
func TestPluginDirsSkipsAbsentAndNonDirs(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, "usr/local/lib/python3.14/lib-dynload")
	if err := os.MkdirAll(want, 0o755); err != nil {
		t.Fatal(err)
	}
	// A FILE at another pattern's location: matches the glob, is not a dir.
	if err := os.MkdirAll(filepath.Join(root, "usr/lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "usr/lib/ossl-modules"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := PluginDirs(root)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %v, want exactly [%s]", got, want)
	}
}

// An empty rootfs yields nothing, not an error and not a list of paths that do
// not exist -- the caller passes these straight to the fuser.
func TestPluginDirsEmptyRootfs(t *testing.T) {
	if got := PluginDirs(t.TempDir()); len(got) != 0 {
		t.Errorf("got %v for an empty rootfs, want none", got)
	}
}
