package translate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// cacheEnv names the store. The cache is opt-in rather than on by default:
// serving a half-hour translation out of a directory instead of running it is
// exactly the behaviour a test should have to ask for.
const cacheEnv = "RAPTORMARK_OBJECT_CACHE"

// Cache is an on-disk store of translated objects, keyed by ObjectKey.
//
// builder/Dockerfile records this as the whole point of the two identity
// labels:
//
//	keying the .o cache on them lets an ecvisor-only change reuse every
//	translated object.
//
// That is the property that pays. libecvisor.a is consumed only by link-all,
// so a change confined to runtime/ leaves every translated object valid and
// only the final link has to re-run — minutes instead of the half hour a
// fused guest's codegen costs.
//
// Entries are <Dir>/<key[:2]>/<key>/, each holding the artifacts named by
// Request.artifacts. An entry is populated in a temporary directory and
// renamed into place, so an interrupted build cannot leave a half-written
// entry that a later Get would treat as a hit.
type Cache struct {
	// Dir is the store's root. It is created on first Put.
	Dir string
}

// CacheFromEnv returns the cache RAPTORMARK_OBJECT_CACHE names, or nil if the
// variable is unset or empty. A nil *Cache is valid everywhere a cache is
// accepted and disables caching.
func CacheFromEnv() *Cache {
	dir := os.Getenv(cacheEnv)
	if dir == "" {
		return nil
	}
	return &Cache{Dir: dir}
}

func (c Cache) entryDir(key string) (string, error) {
	if len(key) < 4 {
		return "", fmt.Errorf("translate: implausible cache key %q", key)
	}
	return filepath.Join(c.Dir, key[:2], key), nil
}

// artifacts are the outputs of a translation that anything downstream consumes:
// the object every ecvisor link takes, and — on the upstream path — the
// standalone module.
//
// The intermediates translate-one leaves beside them (.bc, .merged.bc, .mi.bc,
// .ns.bc) are deliberately not stored. The .bc alone is larger than the object
// (162 MB against 144 MB on the fused OpenSSL closure), and what it would save
// is the lift — about a minute of a translation whose codegen is half an hour.
func (r Request) artifacts() []string {
	if r.Options.withDefaults().Runtime == "ecvisor" {
		return []string{r.ModuleID + ".o"}
	}
	return []string{r.ModuleID + ".o", r.ModuleID + ".wasm"}
}

// Get links a cached translation's artifacts into r.OutDir, reporting whether
// the entry was present. A partially-present entry is treated as a miss.
func (c Cache) Get(key string, r Request) (bool, error) {
	dir, err := c.entryDir(key)
	if err != nil {
		return false, err
	}
	names := r.artifacts()
	for _, n := range names {
		switch _, err := os.Stat(filepath.Join(dir, n)); {
		case errors.Is(err, fs.ErrNotExist):
			return false, nil
		case err != nil:
			return false, err
		}
	}
	if err := os.MkdirAll(r.OutDir, 0o755); err != nil {
		return false, err
	}
	for _, n := range names {
		if err := linkOrCopy(filepath.Join(dir, n), filepath.Join(r.OutDir, n)); err != nil {
			return false, fmt.Errorf("translate: serving %s from the object cache: %w", n, err)
		}
	}
	return true, nil
}

// Put copies a completed translation's artifacts into the store.
func (c Cache) Put(key string, r Request) error {
	dir, err := c.entryDir(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(c.Dir, "tmp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for _, n := range r.artifacts() {
		if err := copyFile(filepath.Join(r.OutDir, n), filepath.Join(tmp, n)); err != nil {
			return fmt.Errorf("translate: caching %s: %w", n, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	// The key is content-addressed, so a concurrent writer that got there
	// first produced an equally valid entry. Rename onto a populated
	// directory reports ENOTEMPTY (EEXIST on some systems); both mean that.
	switch err := os.Rename(tmp, dir); {
	case err == nil, errors.Is(err, fs.ErrExist), isNotEmpty(err):
		return nil
	default:
		return err
	}
}

func isNotEmpty(err error) bool {
	var le *os.LinkError
	if !errors.As(err, &le) {
		return false
	}
	// syscall.ENOTEMPTY without importing syscall on every platform.
	return le.Err != nil && le.Err.Error() == "directory not empty"
}

// linkOrCopy hardlinks src to dst, falling back to a copy when the two are on
// different filesystems or hardlinks are unavailable. Cached artifacts are
// read-only, so sharing an inode with the store is safe; the fallback keeps
// that true by copying with the same mode.
func linkOrCopy(src, dst string) error {
	if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst)
}

// copyFile writes src to dst and leaves it read-only, so a stray write to a
// cached object fails loudly instead of corrupting the store through a
// hardlink.
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
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, 0o444)
}

// RunCached is Run with the object cache in front of it, reporting whether the
// translation was served from the store. A nil cache always runs.
func (b Builder) RunCached(ctx context.Context, c *Cache, r Request) (bool, error) {
	if c == nil {
		return false, b.Run(ctx, r)
	}
	key, err := b.ObjectKey(r)
	if err != nil {
		return false, err
	}
	switch hit, err := c.Get(key, r); {
	case err != nil:
		return false, err
	case hit:
		return true, nil
	}
	if err := b.Run(ctx, r); err != nil {
		return false, err
	}
	return false, c.Put(key, r)
}
