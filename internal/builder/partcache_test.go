package builder

import (
	"os"
	"path/filepath"
	"testing"
)

// newTestCache builds a cache with a fixed salt, so a test does not depend on a
// clang being present to answer --version.
func newTestCache(t *testing.T, salt string) *partCache {
	t.Helper()
	return &partCache{dir: t.TempDir(), salt: salt}
}

func mkPart(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The whole point is content addressing: two partitions with identical bytes
// must share a key even when they are different files in different directories,
// which is what makes a shifted program index reuse every unchanged partition.
func TestPartCacheKeyFollowsContentNotPath(t *testing.T) {
	c := newTestCache(t, "salt")
	dir := t.TempDir()
	a := mkPart(t, dir, "p0", []byte("identical bitcode"))
	b := mkPart(t, filepath.Join(dir), "p17", []byte("identical bitcode"))
	d := mkPart(t, dir, "p2", []byte("different bitcode"))

	ka, err := c.key(a)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := c.key(b)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := c.key(d)
	if err != nil {
		t.Fatal(err)
	}
	if ka != kb {
		t.Errorf("identical content got different keys:\n  %s\n  %s", ka, kb)
	}
	if ka == kd {
		t.Errorf("different content shared a key: %s", ka)
	}
}

// The salt is the compiler invocation. Serving an object compiled by a different
// clang, or at a different optimisation level, would be silent corruption of the
// build rather than a cache miss.
func TestPartCacheSaltSeparatesCompilers(t *testing.T) {
	dir := t.TempDir()
	p := mkPart(t, dir, "p0", []byte("same bitcode"))

	k1, err := newTestCache(t, "clang-16 -O1").key(p)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := newTestCache(t, "clang-16 -O0").key(p)
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k2 {
		t.Fatal("same key across different compiler salts")
	}
}

func TestPartCacheRoundTrip(t *testing.T) {
	c := newTestCache(t, "salt")
	dir := t.TempDir()
	src := mkPart(t, dir, "p0", []byte("bitcode"))
	obj := mkPart(t, dir, "p0.o", []byte("compiled object"))

	key, err := c.key(src)
	if err != nil {
		t.Fatal(err)
	}
	if c.get(key, filepath.Join(dir, "miss.o")) {
		t.Fatal("empty cache reported a hit")
	}
	c.put(key, obj)

	dst := filepath.Join(dir, "restored.o")
	if !c.get(key, dst) {
		t.Fatal("stored object did not come back")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "compiled object" {
		t.Fatalf("restored %q, want %q", got, "compiled object")
	}
	if c.hits != 1 || c.miss != 1 {
		t.Errorf("hits=%d miss=%d, want 1/1", c.hits, c.miss)
	}
}

// get must overwrite whatever is at the destination. compileParts names the
// object after the partition, and a stale object from a previous run sitting
// there would otherwise be linked into the final wasm-ld -r.
func TestPartCacheGetOverwritesStaleDestination(t *testing.T) {
	c := newTestCache(t, "salt")
	dir := t.TempDir()
	src := mkPart(t, dir, "p0", []byte("bitcode"))
	obj := mkPart(t, dir, "p0.o", []byte("fresh"))
	key, err := c.key(src)
	if err != nil {
		t.Fatal(err)
	}
	c.put(key, obj)

	dst := mkPart(t, dir, "dst.o", []byte("STALE"))
	if !c.get(key, dst) {
		t.Fatal("expected a hit")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fresh" {
		t.Fatalf("destination still holds %q", got)
	}
}

// A nil cache is the unconfigured state and every call site relies on it, so it
// must behave as a permanent miss rather than panicking.
func TestNilPartCacheIsInert(t *testing.T) {
	var c *partCache
	if c.get("key", filepath.Join(t.TempDir(), "x.o")) {
		t.Fatal("nil cache reported a hit")
	}
	c.put("key", "irrelevant") // must not panic
	if s := c.summary(); s != "" {
		t.Errorf("nil cache summarised as %q", s)
	}
}
