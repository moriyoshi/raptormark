package translate

import (
	"os"
	"sort"
)

// The experimental switches that CHANGE THE EMITTED OBJECT, and therefore belong
// in the object-cache identity.
//
// # The hazard this closes
//
// `Link`'s comment states it plainly: these are read from the host environment
// rather than from Request, so none of them reached ObjectKey, "yet several
// change the emitted bytes -- shared naming rewrites symbol names, and the
// library ranges change how the module is partitioned. Two translations of one
// ELF with different settings therefore collide on the same object-cache key."
//
// That was tolerable while they were experiment-only switches whose users knew
// to pass a separate cache directory. It stops being tolerable the moment one is
// promoted -- and `RAPTORMARK_STABLE_SPLIT` is the one README item 1 now reduces
// to promoting. The same comment names the precondition: "Anything here that
// becomes a shipping default has to move onto Request and into ObjectKey first,
// or the cache will serve the wrong object."
//
// # Why this reads the environment instead of taking a parameter
//
// A deliberate trade of purity for the one property that matters here: the
// container flags and the cache key must be derived from the SAME reading, or
// they can disagree. Threading these through `Options` would be purer and would
// leave a caller free to set the environment, build `Options` by hand, and get a
// key that describes a build it did not do -- which is precisely the shape of
// both failures this project hit on 2026-08-18 (a stale prebuilt binary, and a
// cache key that claimed a pipeline it was not built by). One reader, no
// disagreement.
//
// # What is NOT here, and why
//
// A switch earns a place only by changing the emitted bytes:
//
//	RAPTORMARK_KEEP_SPLIT       keeps intermediates on disk
//	RAPTORMARK_LIFT_JOBS        concurrency, like Request.Jobs
//	RAPTORMARK_TRANSLATE_VERBOSE  logging
//	RAPTORMARK_LIB_CACHE        where cached library halves live; a hit and a
//	                            miss must produce the same object, and if they
//	                            do not, THAT is the defect
//	RAPTORMARK_NO_LIB_CACHE     the same switch, negated
//
// Adding one of those would invalidate every cached object for a change that
// cannot alter one -- the exact cost `internal/builder/toolsid.go` weighs when it
// declines to hash the toolchain.
var experimentalVars = []string{
	// Content-stable partitioner: a different assignment, so different
	// partitions and a different object.
	"RAPTORMARK_STABLE_SPLIT",
	// Restores the three separate whole-module passes. `stablesplit.go` records
	// that under llvm-split the final object DIFFERS from the merged path.
	"RAPTORMARK_NO_MERGED_PREPARE",
	// Rewrites lifted symbol names and their linkage.
	"RAPTORMARK_SHARED_NAMES",
	// The boundary between program-specific and shared code, i.e. WHICH names
	// the switch above rewrites.
	"RAPTORMARK_SHARED_MIN",
	// Where each library sits; changes which partition a symbol lands in.
	"RAPTORMARK_LIB_RANGES",
	// How many clusters share one library-scoped partition: a partition size
	// target, so it changes the partitioning.
	"RAPTORMARK_LIB_CHUNK",
}

// ExperimentalSettings returns the byte-affecting switches that are set, as
// sorted `NAME=VALUE` strings, or nil when none is.
//
// Nil for a default build is load-bearing: `TranslateID` appends nothing then,
// so every object cached before this existed keys the same as it did. A change
// that cannot alter an object must not invalidate one.
func ExperimentalSettings() []string {
	var out []string
	for _, name := range experimentalVars {
		if v := os.Getenv(name); v != "" {
			out = append(out, name+"="+v)
		}
	}
	sort.Strings(out)
	return out
}
