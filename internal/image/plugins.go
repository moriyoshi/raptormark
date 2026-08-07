package image

import (
	"debug/elf"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

// Discovering the dlopen-able objects in an image.
//
// `PluginDirs` returns DIRECTORIES; `fuse.Options.Extra` takes FILES. Nothing
// bridged the two, so every caller globbed by hand -- which is how the postgres
// measurements in JOURNAL.md were taken, and it is not a step a build should
// leave to whoever remembers the file extension.
//
// ⚠️ This is DISCOVERY, not admission. A plugin named here still has to fuse.
//
// ❗ **CORRECTION 2026-08-26: THE SAFETY NET NAMED HERE IS NOT ON THE BUILD
// PATH, and `raptormark build python:3-slim` FAILS because of it.** This comment
// used to end "`FuseClosure` reports the ones whose dependencies the image does
// not contain through `Report.SkippedExtras`, which is a different and equally
// visible list." Both halves are true about `FuseClosure` and neither protects a
// build: `internal/pipeline.build` never calls it. It calls `FuseWithUnits` for
// the entry and `Fuse` for every other program, and BOTH turn `load`'s
// `SkippedExtra` into a fatal error -- deliberately, see their doc comments.
//
// python:3-slim's `_tkinter` is still the standing example and is now the
// standing FAILURE. It DT_NEEDEDs `libtk8.6.so`, which the image does not ship
// (verified: no `libtk*`/`libtcl*` anywhere in the rootfs, and `import tkinter`
// raises the identical `cannot open shared object file` in the stock container).
// So a module that could never have loaded takes the whole build down with:
//
//	fuse: cannot satisfy dlopen'd plugin .../_tkinter...so:
//	fuse: cannot find libtk8.6.so in [...]
//
// ⚠️ The asymmetry `Fuse` documents is right for an EXPLICIT `Options.Extra` --
// somebody named that plugin and a silently absent one is the failure Extra
// exists to prevent. Under `--plugins auto` nobody named it; discovery found it
// by walking a directory. That is the distinction the fix has to turn on, and
// where it belongs is an open question -- see `.agents/docs/TODO.md`.

// Plugin is one dlopen-able object found under an image's plugin directories.
type Plugin struct {
	// Guest is the path as the guest names it, with a leading slash. This is
	// what a dlopen map keys on.
	Guest string
	// Host is the path under the exported rootfs, which fuse.Options.Extra takes.
	Host string
}

// ExcludedPlugin names an object discovery deliberately dropped, and why.
//
// Reported rather than silently skipped, for the reason `SkippedExtra` exists:
// a plugin that is not in the image is a capability the guest does not have,
// and the difference between "we chose not to" and "it failed to fuse" is one
// the caller has to be able to see.
type ExcludedPlugin struct {
	Guest  string
	Reason string
}

// jitSonamePrefix identifies a plugin that exists to generate machine code.
//
// WHY EXCLUDE BY DEPENDENCY AND NOT BY NAME. `llvmjit.so` is postgres's JIT and
// the only extension of the 79 that names libLLVM, so a name check would work
// today and silently stop working for the next image. The dependency is the
// actual property: an object linked against LLVM's codegen is doing codegen.
//
// WHY EXCLUDE AT ALL, and it is not about size. raptormark translates ahead of
// time and README puts JIT out of scope; there is no lifter in the module, so a
// guest address that was generated at run time reaches `func_at -> None ->
// fatal!`. An extension whose purpose cannot be served is not made useful by
// fusing it -- and fusing this one measurably costs the whole build: it drags in
// `libLLVM.so.19.1` (117.5 MiB) and, through it, `libz3.so.4` (25.6 MiB), which
// is 143 of the 145 MiB that pushes the postgres closure past the 156 MiB fused
// region. With it excluded, postgres + initdb + 78 extensions plan SHARED at
// 89.7%; with it, they fall back to per-image packing and lose all sharing.
const jitSonamePrefix = "libLLVM"

// Plugins enumerates the dlopen-able objects under `root`'s plugin directories,
// and the ones deliberately excluded.
//
// Both lists are sorted by guest path so a build is reproducible. An object that
// cannot be read as an aarch64 shared library is excluded with the reason rather
// than dropped: a stray file in a plugin directory is worth knowing about, and a
// plugin the fuser would reject is better named here than at fuse time.
func Plugins(root string) ([]Plugin, []ExcludedPlugin, error) {
	// Absolute FIRST, and PluginDirs is called with the absolute root.
	//
	// ⚠️ The first version resolved the root but passed the caller's spelling to
	// PluginDirs, so the walked paths were relative while `guestPath` compared
	// them against an absolute root. Every filepath.Rel then failed, and because
	// a failure meant "skip this entry" the result was "discovered 0 plugin(s),
	// excluded 0" on a rootfs holding 79 -- a clean, plausible, wrong answer with
	// no diagnostic. The `cannot be placed` exclusion below exists so that shape
	// of bug reports itself next time.
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, fmt.Errorf("image: resolving rootfs %s: %w", root, err)
	}
	dirs := PluginDirs(abs)
	if len(dirs) == 0 {
		return nil, nil, nil
	}

	var found []Plugin
	var excluded []ExcludedPlugin
	seen := map[string]bool{}

	for _, dir := range dirs {
		// Walked rather than globbed: `lib-dynload` is flat but `site-packages`
		// nests one extension per package directory, and a flat glob finds none
		// of those.
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !isSharedObjectName(d.Name()) {
				return nil //nolint:nilerr // an unreadable entry is not a build failure
			}
			guest, gerr := guestPath(abs, p)
			if gerr != nil {
				// Never silent. A path that cannot be expressed as a guest path
				// means the walk and the root disagree, and dropping it quietly
				// yields an empty plugin list that looks like an image with no
				// plugins.
				excluded = append(excluded, ExcludedPlugin{p, gerr.Error()})
				return nil
			}
			if seen[guest] {
				return nil
			}
			seen[guest] = true

			needed, kind := describe(p)
			switch kind {
			case notELF:
				excluded = append(excluded, ExcludedPlugin{guest, "not a readable aarch64 shared object"})
				return nil
			case usesJIT:
				excluded = append(excluded, ExcludedPlugin{guest,
					"links " + jitSonamePrefix + " (" + strings.Join(needed, ", ") +
						"); a JIT extension cannot work under an ahead-of-time runtime"})
				return nil
			}
			found = append(found, Plugin{Guest: guest, Host: p})
			return nil
		})
		if err != nil {
			return nil, nil, fmt.Errorf("image: walking plugin dir %s: %w", dir, err)
		}
	}

	sort.Slice(found, func(i, j int) bool { return found[i].Guest < found[j].Guest })
	sort.Slice(excluded, func(i, j int) bool { return excluded[i].Guest < excluded[j].Guest })
	return found, excluded, nil
}

// isSharedObjectName matches the spellings a dlopen-able object actually uses.
//
// Not `strings.HasSuffix(".so")`: CPython names its extensions
// `_socket.cpython-311-aarch64-linux-gnu.so` and a versioned library is
// `libfoo.so.3`, so the marker is the `.so` COMPONENT, not the final extension.
func isSharedObjectName(name string) bool {
	return strings.HasSuffix(name, ".so") || strings.Contains(name, ".so.")
}

type pluginKind int

const (
	ordinary pluginKind = iota
	notELF
	usesJIT
)

// describe reads an object's DT_NEEDED and classifies it.
func describe(path string) ([]string, pluginKind) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, notELF
	}
	defer f.Close()
	if f.Machine != elf.EM_AARCH64 || f.Type != elf.ET_DYN {
		return nil, notELF
	}
	needed, err := f.DynString(elf.DT_NEEDED)
	if err != nil {
		// A shared object with no .dynamic is odd but not a reason to refuse it;
		// the fuser will say so more precisely if it matters.
		return nil, ordinary
	}
	if IsJITPlugin(needed) {
		return needed, usesJIT
	}
	return needed, ordinary
}

// IsJITPlugin reports whether an object's DT_NEEDED list marks it as a
// code-generating extension.
//
// Split from the ELF reading so the POLICY can be tested without a hand-built
// aarch64 shared object. The parsing half is exercised by discovery on a real
// rootfs; this half is where a wrong answer would be silent -- excluding a
// plugin that works, or admitting one that drags in 143 MiB and cannot run.
func IsJITPlugin(needed []string) bool {
	for _, n := range needed {
		// Prefix, not equality: the soname carries a version
		// (`libLLVM.so.19.1`) that moves with every LLVM release.
		if strings.HasPrefix(n, jitSonamePrefix) {
			return true
		}
	}
	return false
}

// guestPath turns a host path under the rootfs into the path the guest uses.
func guestPath(root, p string) (string, error) {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("image: %s is outside the rootfs %s", p, root)
	}
	return "/" + filepath.ToSlash(rel), nil
}
