package rootfs

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"raptormark/internal/link"
)

// maxLinks matches MAX_LINKS in runtime/src/vfs/mod.rs. Keep the two equal: a
// host-side limit that is looser accepts a chain the guest will refuse, and one
// that is tighter rejects an image that would have worked.
const maxLinks = 40

// Resolve resolves an absolute GUEST path through the host directory tree at
// root -- which is the tree Build will turn into the rfs lower layer -- and
// returns the canonical guest path, fully symlink-resolved including the final
// component.
//
// # Why this is not filepath.EvalSymlinks
//
// It mirrors `Vfs::resolve(cwd, path, follow_final = true)` in
// runtime/src/vfs/mod.rs, which is the function the runtime actually reaches
// through `Programs::resolve` when an execve misses the exec map. Three of its
// rules are not the host's:
//
//   - An ABSOLUTE symlink target names the GUEST root, so it restarts at root
//     rather than at the host's /. EvalSymlinks would follow /usr/bin/dash out
//     of the image and into the host filesystem, where it would either not
//     exist or -- worse -- exist and be a different file.
//   - ".." is applied lexically to the already-resolved prefix and never climbs
//     above "/", so no path can escape the image.
//   - Every intermediate component must EXIST. A path naming a directory that
//     the image does not contain does not resolve, it fails.
//
// Following the final component is what makes /bin/sh useful: on a usr-merged
// Debian image it resolves to /usr/bin/dash, which is the program that actually
// runs.
func Resolve(root, guestPath string) (string, error) {
	if !strings.HasPrefix(guestPath, "/") {
		return "", fmt.Errorf("rootfs: exec map path %q is not absolute", guestPath)
	}
	// Reverse order, so the tail of the slice is the next component -- the same
	// shape as `split_rev` + `Vec::pop` in the runtime, which is what makes the
	// symlink expansion below a push rather than a splice.
	pending := splitRev(guestPath)
	resolved := "/"
	links := 0

	for len(pending) > 0 {
		comp := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if comp == "." {
			continue
		}
		if comp == ".." {
			resolved = parentOf(resolved)
			continue
		}
		candidate := path.Join(resolved, comp)
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(candidate)))
		if err != nil {
			return "", fmt.Errorf("rootfs: resolving %q: %q is not in the image", guestPath, candidate)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			resolved = candidate
			continue
		}
		// A symlink. The runtime follows it whether or not it is the final
		// component, because `Programs::resolve` passes follow_final = true.
		links++
		if links > maxLinks {
			return "", fmt.Errorf("rootfs: resolving %q: more than %d symlinks", guestPath, maxLinks)
		}
		target, err := os.Readlink(filepath.Join(root, filepath.FromSlash(candidate)))
		if err != nil {
			return "", fmt.Errorf("rootfs: reading symlink %q: %w", candidate, err)
		}
		target = filepath.ToSlash(target)
		if strings.HasPrefix(target, "/") {
			resolved = "/" // absolute target: the GUEST root, not the host's
		}
		pending = append(pending, splitRev(target)...)
	}

	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(resolved))); err != nil {
		return "", fmt.Errorf("rootfs: resolving %q: %q is not in the image", guestPath, resolved)
	}
	return resolved, nil
}

// splitRev splits a path into non-empty components in reverse order, matching
// `split_rev` in runtime/src/vfs/mod.rs. "." survives the split and is dropped
// by the caller, exactly as it is there.
func splitRev(p string) []string {
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != "" {
			out = append(out, parts[i])
		}
	}
	return out
}

// parentOf is `truncate_to_parent`: the parent of an absolute path, never above
// "/".
func parentOf(p string) string {
	q := path.Dir(p)
	if q == "." || q == "" {
		return "/"
	}
	return q
}

// CheckExecMap rejects an encoded exec map whose paths are not canonical in the
// image rooted at root. Build calls it, so the guarantee holds for every caller
// rather than only for the ones that remembered.
//
// This is the enforcement `ExecEntry.Path` documented and nothing performed.
// `ExecMap` cannot do it -- it has no filesystem, only paths and hashes -- and
// Build takes the encoded map as opaque bytes, so the check needs both halves
// brought together: link.ParseExecMap to read it back, and the tree Build is
// about to serialise to resolve against.
//
// It reports EVERY bad entry, not the first. A map is usually built by one loop
// over one list, so one mistake tends to affect all of them, and fixing them one
// build at a time is the slowest possible way to find that out.
func CheckExecMap(root string, encoded []byte) error {
	entries, err := link.ParseExecMap(encoded)
	if err != nil {
		return err
	}
	var bad []string
	for _, e := range entries {
		canon, err := Resolve(root, e.Path)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s (%v)", e.Path, err))
			continue
		}
		if canon != e.Path {
			bad = append(bad, fmt.Sprintf("%s -> should be keyed %s", e.Path, canon))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf(
			"rootfs: %d exec map path(s) are not canonical in this image, so the runtime "+
				"would resolve past them and fall back to program 0: %s "+
				"(canonicalise with rootfs.CanonicalExecEntries before encoding)",
			len(bad), strings.Join(bad, "; "))
	}
	return nil
}

// CanonicalExecEntries rewrites every entry's path to its canonical form in the
// image rooted at root, and reports the ones that cannot be canonicalised.
//
// # The failure this exists to prevent
//
// `ExecEntry.Path` documents that the path must be canonical, and nothing
// enforced it. A non-canonical entry is not a broken map -- it encodes, it
// validates, and every hash in it names a real program. It simply matches
// nothing: the runtime tries an exact match and then a VFS-resolved one, so an
// entry keyed /bin/dash is reachable only from a literal execve("/bin/dash"),
// which nothing does. libc spawns through /bin/sh, and on a usr-merged image
// (postgres:17 among them) that resolves to /usr/bin/dash. The exec then falls
// back to program 0 and the guest runs the wrong program under the right argv:
// initdb's popen child exited 127 having run no program at all.
//
// Two entries that collapse onto one canonical path are rejected rather than
// deduplicated when they name DIFFERENT programs, because at that point the
// image cannot express what the caller asked for and picking one silently is
// the same class of mistake. Collapsing onto the same program is fine and the
// duplicate is dropped.
func CanonicalExecEntries(root string, entries []link.ExecEntry) ([]link.ExecEntry, error) {
	out := make([]link.ExecEntry, 0, len(entries))
	byPath := make(map[string]link.ExecEntry, len(entries))
	for _, e := range entries {
		canon, err := Resolve(root, e.Path)
		if err != nil {
			return nil, err
		}
		if prev, ok := byPath[canon]; ok {
			if prev.Hash != e.Hash {
				return nil, fmt.Errorf(
					"rootfs: exec map paths %q and %q both resolve to %q but name different programs (%q, %q)",
					prev.Path, e.Path, canon, prev.Hash, e.Hash)
			}
			continue
		}
		c := link.ExecEntry{Path: canon, Hash: e.Hash}
		byPath[canon] = link.ExecEntry{Path: e.Path, Hash: e.Hash}
		out = append(out, c)
	}
	return out, nil
}

// CheckDlMap rejects an encoded dlopen map whose paths are not canonical in the
// image rooted at root. Build calls it, so the guarantee holds for every caller.
//
// The exec-map version of this exists because a non-canonical path makes execve
// fall back to program 0. Here the consequence differs and is no better: a
// non-canonical entry simply never matches, so the guest's dlopen fails and
// reports the plugin absent when it is present and translated.
//
// ⚠️ Canonical form matters MORE here. A guest names a plugin through whatever
// path its own configuration holds -- postgres builds one from `$libdir`, python
// from `sys.path` -- so the spellings are far more varied than the handful of
// paths libc spawns through, and a usr-merged Debian image resolves most of them
// through at least one symlink.
//
// Reports EVERY bad entry rather than the first, as CheckExecMap does: one loop
// building the map means one mistake affects all of them.
func CheckDlMap(root string, encoded []byte) error {
	entries, err := link.ParseDlMap(encoded)
	if err != nil {
		return err
	}
	var bad []string
	for _, e := range entries {
		canon, err := Resolve(root, e.Path)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s (%v)", e.Path, err))
			continue
		}
		if canon != e.Path {
			bad = append(bad, fmt.Sprintf("%s -> should be keyed %s", e.Path, canon))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf(
			"rootfs: %d dlopen map path(s) are not canonical in this image, so the guest's "+
				"dlopen would never match them and would report the plugin absent: %s "+
				"(canonicalise with rootfs.CanonicalDlEntries before encoding)",
			len(bad), strings.Join(bad, "; "))
	}
	return nil
}

// CanonicalDlEntries rewrites every entry's path to its canonical form in the
// image rooted at root, and reports the ones that cannot be canonicalised.
//
// The counterpart of CanonicalExecEntries, and the intended way to build a map
// that CheckDlMap will accept.
func CanonicalDlEntries(root string, entries []link.DlEntry) ([]link.DlEntry, []string, error) {
	out := make([]link.DlEntry, 0, len(entries))
	var bad []string
	seen := map[string]bool{}
	for _, e := range entries {
		canon, err := Resolve(root, e.Path)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s (%v)", e.Path, err))
			continue
		}
		// Two spellings can canonicalise to ONE path, and link.DlMap refuses a
		// duplicate. Collapsing here rather than letting the encode fail keeps
		// the diagnostic on the input the caller can act on.
		if seen[canon] {
			bad = append(bad, fmt.Sprintf("%s -> %s, already claimed by another entry", e.Path, canon))
			continue
		}
		seen[canon] = true
		out = append(out, link.DlEntry{Path: canon, Hash: e.Hash})
	}
	return out, bad, nil
}
