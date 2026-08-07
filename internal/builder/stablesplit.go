package builder

// Content-stable partitioning, via the ecv-split companion tool.
//
// `llvm-split` bundles two jobs: it ASSIGNS by balancing size over the whole
// module, and it PROMOTES locals that cross a partition boundary. We need the
// promotion and must replace the assignment -- balancing over the whole module
// means any change reshuffles every bucket, so no partition is ever
// byte-identical and the cache in partcache.go cannot hit (measured: 0/80 across
// a registry index shift).
//
// ecv-split keeps the promotion and assigns by a stable hash of the symbol NAME,
// so a partition's contents change only when a symbol it holds changes. See
// builder/ecv-split.cpp for why the assignment must still respect blockaddress
// clustering, and why `llvm-extract` cannot be used instead.
//
// Enabled by ECV_STABLE_SPLIT=1; unset, translate-one uses llvm-split unchanged.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const stableSplitEnv = "ECV_STABLE_SPLIT"

func stableSplitEnabled() bool { return os.Getenv(stableSplitEnv) != "" }

// noMergedPrepareEnv turns OFF builder/ecv-prepare.cpp and restores the three
// separate whole-module passes it replaces: llvm-link, opt -internalize,globaldce
// and namespace-object.
//
// DEFAULT ON since 2026-08-13. Those three each parsed and re-serialized the
// whole module for work measured in hundredths of a second -- on bash-glibc a
// bare `opt -passes=` round trip is 4.999 s against the real internalize+globaldce
// pass's 5.057 s. Merged they cost 3.25 s instead of 13.30 s, and a
// partition-cache-warm translation drops 32.51 s -> 22.85 s.
//
// WHAT THE FLIP CHANGED, stated plainly because the two splitters differ:
//
//   - With ECV_STABLE_SPLIT (ecv-split), partitions and the final object are
//     BYTE-IDENTICAL to the three-tool chain -- verified on bash-glibc in three
//     configurations and on postgres's 520 MB module, 80/80 and 124/124.
//   - With the default llvm-split, the final object DIFFERS. ecv-prepare's
//     `.ns.bc` holds the same content in a different global order, and llvm-split
//     assigns by balancing size in module-walk order, so its buckets reshuffle.
//     That is not a regression in kind: llvm-split's assignment is already
//     unstable under any change, which is the entire reason ecv-split exists.
//     The evidence for the new default on this path is therefore the E2E suite
//     (49 PASS / 4 SKIP / 0 FAIL on exactly this combination), not byte identity.
//
// The escape hatch stays so a future regression can be bisected against the old
// pipeline without rebuilding an image.
const noMergedPrepareEnv = "ECV_NO_MERGED_PREPARE"

func mergedPrepareEnabled() bool { return os.Getenv(noMergedPrepareEnv) == "" }

// stableSplit writes partitions into splitDir and returns them.
func (c *TranslateOne) stableSplit(n int, nsbc, splitDir string) ([]part, error) {
	prefix := filepath.Join(splitDir, "p")
	if err := run("ecv-split", nsbc, prefix, fmt.Sprint(n)); err != nil {
		return nil, fmt.Errorf("ecv-split on %s: %w", nsbc, err)
	}
	return partsAt(prefix, "ecv-split", nsbc)
}

// prepareAndSplit runs ecv-prepare with --split, so the link, internalize,
// namespacing AND partitioning all happen against one parse of the module. The
// .ns.bc is not written at all unless ECV_KEEP_SPLIT asks for it -- skipping
// that ~28 MB write, and the parse of it that ecv-split would have done, is the
// entire saving. The partitions themselves are byte-identical either way, which
// is what makes the shortcut safe for the content-addressed cache.
func (c *TranslateOne) prepareAndSplit(n int, bc, fragbc, nsbc, splitDir string, libBCs []string) ([]part, error) {
	prefix := filepath.Join(splitDir, "p")
	args := []string{bc, fragbc, nsbc, c.ModuleID, c.Keep}
	for _, lib := range libBCs {
		args = append(args, "--merge", lib)
	}
	args = append(args, "--split", prefix, fmt.Sprint(n))
	if err := run("ecv-prepare", args...); err != nil {
		return nil, fmt.Errorf("ecv-prepare --split on %s: %w", bc, err)
	}
	return partsAt(prefix, "ecv-prepare --split", bc)
}

// partsAt collects the partitions a splitter wrote under prefix. Exit 0 with no
// parts is the worse failure -- nothing to report, but the module silently
// vanishes -- so it is an error here rather than an empty slice.
func partsAt(prefix, tool, in string) ([]part, error) {
	matches, err := filepath.Glob(prefix + "*")
	if err != nil {
		return nil, err
	}
	var parts []part
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil || info.IsDir() {
			continue
		}
		parts = append(parts, part{path: m, size: info.Size()})
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("%s produced no partitions from %s", tool, in)
	}
	// Deterministic order in, deterministic order out: compileParts re-sorts by
	// size, but wasm-ld -r takes the object list in this order and an unstable
	// one would change the final object for no reason.
	sort.Slice(parts, func(i, j int) bool { return parts[i].path < parts[j].path })
	return parts, nil
}
