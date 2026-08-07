package builder

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// translateSources are the files whose contents determine what translate-one
// produces. Their combined hash is the `raptormark.translate_sh` label, which
// internal/translate.TranslateID folds into the per-binary object cache key.
//
// The list is deliberately narrow and deliberately explicit. When this was a
// shell script the label was sha256(builder/translate-one.sh) — note that
// link-all.sh was *not* in it, because link-all cannot change a cached object,
// only the final module. The same reasoning selects these two files:
//
//   - translateone.go — the pipeline itself.
//   - exec.go — the process/environment helpers it runs everything through.
//   - stablesplit.go — which pipeline runs. Added 2026-08-13 with the
//     ecv-prepare default: it holds the DEFAULT of a switch that changes the
//     emitted object on the llvm-split path, and a default that changes bytes
//     but not the cache key is precisely the "serves a stale object" failure
//     this list exists to prevent. It also holds ECV_STABLE_SPLIT, which
//     selects the partitioner, so it belonged here already.
//
// The companion C++ tools joined the list on 2026-08-13. They are not the Go
// pipeline but they ARE the pipeline: each one reads the module and writes the
// bytes that become the cached object.
//
//   - ecv-prepare.cpp   — link + internalize/globaldce + namespacing (default)
//   - ecv-namespace.h   — the namespacing decision both tools below share
//   - namespace-object.cpp — the same step, for ECV_NO_MERGED_PREPARE
//   - ecv-split.cpp     — the partitioner, for ECV_STABLE_SPLIT
//   - ecv-promote.cpp   — the register-promotion pass, for --promote
//
// They reach the builder image through NEITHER this hash NOR `BASE_ID`, which
// covers only the patched elfconv base, so before this an edit to any of them
// left every cached object keyed as though nothing had changed — a stale object
// for a pipeline that no longer produces it, which is the exact failure this
// list exists to prevent. Found while making ecv-prepare the default; the hazard
// pre-dated that work by as long as ecv-split has existed.
//
// The cost is real and was accepted deliberately: every object invalidates
// whenever any of these files changes, and an object costs hours. That is the
// right side to err on. A tool edit that genuinely cannot change the bytes can
// still be shipped cheaply — `raptormark build-image --translate-sh <old>` pins
// the label — and that escape hatch is the reason the conservative default is
// affordable.
//
// buildimage.go and linkall.go are excluded on purpose. Including them would
// invalidate every cached object whenever the image build or the final link
// changed, and neither can change a cached object. builder/Dockerfile is
// excluded for the same reason with one caveat worth knowing: it carries the
// compile flags for the tools above, so rebuilding them at a different
// optimisation level would not move the key. It is not in the list because it
// also carries the ecvisor runtime build, which changes often and reaches only
// the final link.
//
// Over-invalidating is safe; under-invalidating serves a stale object for a
// pipeline that no longer produces it. When in doubt, add the file.
var translateSources = []string{
	"internal/builder/translateone.go",
	"internal/builder/exec.go",
	"internal/builder/stablesplit.go",
	"builder/ecv-prepare.cpp",
	"builder/ecv-namespace.h",
	"builder/namespace-object.cpp",
	"builder/ecv-split.cpp",
	"builder/ecv-promote.cpp",
}

// TranslateSH is the toolchain-identity hash for the translation pipeline.
//
// It hashes the *sources* rather than the compiled binary on purpose: the Go
// compiler version reaches a binary's bytes but not the objects the pipeline
// emits, so hashing the binary would discard a cache worth hours of CPU on
// every unrelated toolchain bump.
func TranslateSH(repoRoot string) (string, error) {
	h := sha256.New()
	writePart(h, "raptormark-translate-sh-v1")
	for _, rel := range translateSources {
		b, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			return "", fmt.Errorf("hashing translation sources: %w", err)
		}
		writePart(h, rel)
		writePart(h, string(b))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writePart length-prefixes each field so no two distinct field sets can
// produce the same byte stream, matching internal/translate.hashParts.
func writePart(h interface{ Write([]byte) (int, error) }, s string) {
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(len(s)))
	h.Write(n[:])
	h.Write([]byte(s))
}
