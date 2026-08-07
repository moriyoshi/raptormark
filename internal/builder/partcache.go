package builder

// Content-addressed cache for compiled bitcode partitions.
//
// Codegen is the dominant term of a translation once the lift is fixed, and it
// is tail-bound: measured on bash-glibc, 80 partitions cost 239 s of CPU but the
// wall is 68.7 s because one partition (glibc's `__vfscanf_internal`, 8,795
// instructions in a single function) takes that long on its own and llvm-split
// keeps a function whole. Nothing schedules around that; the only way not to pay
// it is not to compile it again.
//
// A partition's object is a pure function of the partition's bitcode and the
// compiler invocation, so it is content-addressable.
//
// WHAT THIS CAN AND CANNOT SHARE. Rewritten 2026-08-13; the previous text
// described three limits, two of which have since been lifted, and read as
// current.
//
//   - Changing `Keep`/`Fragment` alone -- what happens when a program's INDEX
//     shifts because another program joined the closure -- leaves every partition
//     identical except the one holding the fragment. This was the original win.
//   - A lifter patch no longer invalidates everything. `ModuleID` is the ELF name
//     plus its SHA and nothing else; it stopped folding in `TranslateID`, so a
//     patch that does not change the emitted bytes leaves partitions valid.
//   - TWO PROGRAMS OF ONE CLOSURE NOW SHARE. With a closure-wide address layout,
//     address-derived lifted names and partitions scoped to a single library
//     (ECV_LIB_RANGES), the libraries a pair of programs have in common compile
//     once. Measured on postgres/initdb/dash: initdb's codegen fell from 2,354 s
//     of CPU to 56 s, dash's from 1,940 s to 12 s.
//
// What still cannot be shared, and why:
//
//   - A program's own executable code, by construction -- every executable in a
//     closure starts at the same base with different contents.
//   - Anything across two different IMAGES. `PlanLayout` places libraries above
//     the highest executable top in the closure, so the same glibc sits at a
//     different address elsewhere and its lifted IR differs entirely. A constant
//     library base would fix that; measured as affordable (a 24 MiB base clears
//     every fixture with 31.7 MiB of library headroom) but not adopted.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// partCacheEnv names the directory the host mounts in. Unset disables the
// cache, so a plain `docker run` of translate-one behaves exactly as before.
const partCacheEnv = "ECV_PART_CACHE"

// partCache stores objects under <dir>/<key[:2]>/<key>.o.
type partCache struct {
	dir  string
	salt string // compiler identity; folded into every key

	mu   sync.Mutex
	hits int
	miss int
}

// newPartCache returns nil when the cache is not configured, which every method
// below tolerates.
func newPartCache(t llvmTools, level string) *partCache {
	dir := os.Getenv(partCacheEnv)
	if dir == "" {
		return nil
	}
	return &partCache{dir: dir, salt: t.salt(level)}
}

// salt identifies the compiler invocation. Two objects may only share a key if
// the same compiler would have produced both, so the version string, target,
// sysroot and optimisation level all go in. Hashing the clang BINARY would be
// more precise but costs a read of ~100 MB per translation; the version string
// changes with any toolchain the image could plausibly carry.
func (t llvmTools) salt(level string) string {
	ver, err := output(t.cc, "--version")
	if err != nil {
		// No version, no confident identity: fall back to something that cannot
		// collide across images by accident.
		ver = "unknown-" + t.cc + "-" + t.ver
	}
	h := sha256.New()
	for _, p := range []string{"raptormark-part-cache-v1", ver, level, t.sysroot,
		strings.Join(t.target, ","), t.ver} {
		fmt.Fprintf(h, "%d:%s", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// key is the cache key for one partition: its bytes under the compiler salt.
func (c *partCache) key(partPath string) (string, error) {
	f, err := os.Open(partPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	h.Write([]byte(c.salt))
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (c *partCache) path(key string) string {
	return filepath.Join(c.dir, key[:2], key+".o")
}

// get links a cached object into place, reporting whether it was present.
// A hardlink keeps the store single-copy; a cross-device store falls back to a
// copy rather than failing the build.
func (c *partCache) get(key, dst string) bool {
	if c == nil {
		return false
	}
	src := c.path(key)
	if _, err := os.Stat(src); err != nil {
		c.record(false)
		return false
	}
	os.Remove(dst)
	if err := os.Link(src, dst); err != nil {
		if err := copyFile(src, dst); err != nil {
			c.record(false)
			return false
		}
	}
	c.record(true)
	return true
}

// put stores a freshly compiled object. Written to a temporary name in the same
// directory and renamed, so a concurrent reader never sees a partial object and
// two workers racing on the same key cannot corrupt it.
func (c *partCache) put(key, src string) {
	if c == nil {
		return
	}
	dst := c.path(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".part-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	tmp.Close()
	if err := copyFile(src, tmpName); err != nil {
		os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
	}
}

func (c *partCache) record(hit bool) {
	c.mu.Lock()
	if hit {
		c.hits++
	} else {
		c.miss++
	}
	c.mu.Unlock()
}

// summary is printed to stderr, which internal/translate keeps only on failure —
// so it is for a human watching a direct `docker run`, like the rest of
// translate-one's output.
func (c *partCache) summary() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return fmt.Sprintf("partition cache: %d hit, %d miss", c.hits, c.miss)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
