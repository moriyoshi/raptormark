package builder

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The generated patched-base Dockerfile was compared byte-for-byte against
// builder/build-image.sh --print-dockerfile at the time of the port, for both
// LLVM lines. These assertions guard the parts that would fail silently.

func TestPatchedDockerfileAppliesPatchesThenRebuilds(t *testing.T) {
	got := (&BuildImage{LLVM: "16"}).patchedDockerfile("raptormark-elfconv-base:pin")

	if !strings.HasPrefix(got, "FROM raptormark-elfconv-base:pin\n") {
		t.Errorf("must start with the FROM line, got:\n%s", got)
	}
	// The patch loop has to exit non-zero on a failed apply, or a mis-applied
	// series silently produces an unpatched lifter.
	if !strings.Contains(got, `git apply "$p" || exit 1`) {
		t.Error("the patch loop must fail the build on a bad patch")
	}
	// The decoder assertions are the second line of defence for the same
	// failure: a patch that applies but lands nothing.
	for _, sym := range []string{
		"TryDecodeSTLRB_SL32_LDSTEXCL",
		"TryDecodeMUL_ASIMDSAME_ONLY",
		"TryDecodeSTLLRB_SL32_LDSTEXCL",
	} {
		if !strings.Contains(got, sym) {
			t.Errorf("missing the %s decoder assertion", sym)
		}
	}
	if !strings.Contains(got, "cmake --build build --target elflift aarch64") {
		t.Error("LLVM 16 must rebuild elflift over the patched sources")
	}
	if strings.Contains(got, "llvm-toolchain-jammy-22") {
		t.Error("LLVM 16 must not install the 22 toolchain")
	}
}

// On the 22 line the toolchain swap has to come *after* the patches, so the
// rebuild compiles patched sources with clang-22 in one pass.
func TestPatchedDockerfileLLVM22SwapsToolchainAfterPatching(t *testing.T) {
	got := (&BuildImage{LLVM: "22"}).patchedDockerfile("base:t")
	patch := strings.Index(got, "git apply")
	apt := strings.Index(got, "llvm-toolchain-jammy-22")
	rebuild := strings.Index(got, "CMAKE_ELFCONV_AARCH64_BUILD=1")
	if patch < 0 || apt < 0 || rebuild < 0 {
		t.Fatalf("missing a stage:\n%s", got)
	}
	if !(patch < apt && apt < rebuild) {
		t.Errorf("stage order must be patch -> apt -> rebuild, got %d/%d/%d", patch, apt, rebuild)
	}
}

// TranslateSH keys the object cache. It must be stable for unchanged sources
// and must move when a translation source changes, or a stale object gets
// served for a pipeline that no longer produces it.
func TestTranslateSHIsStableAndSensitive(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range translateSources {
		write(rel, "original "+rel)
	}

	first, err := TranslateSH(root)
	if err != nil {
		t.Fatal(err)
	}
	again, err := TranslateSH(root)
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Errorf("not stable: %s vs %s", first, again)
	}

	// EVERY entry, not just the first. This used to check translateSources[0]
	// alone, which cannot tell a hashed file from a file merely listed — and the
	// list has since grown to carry the companion C++ tools, added because an
	// edit to one of them was leaving the cache key unmoved. A guard that only
	// ever exercised element 0 would have certified that addition without
	// testing it.
	seen := map[string]string{first: "unchanged"}
	for _, rel := range translateSources {
		for _, r := range translateSources {
			write(r, "original "+r) // reset, so each file is tested in isolation
		}
		write(rel, "changed")
		got, err := TranslateSH(root)
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("editing %s does not move TranslateSH (collides with %q); "+
				"it is listed but not hashed", rel, prev)
			continue
		}
		seen[got] = rel
	}
}

// The files named must exist, or every build silently keys the cache on an
// error path.
func TestTranslateSourcesExist(t *testing.T) {
	for _, rel := range translateSources {
		// The test runs in internal/builder; the paths are repo-relative.
		if _, err := os.Stat(filepath.Join("..", "..", rel)); err != nil {
			t.Errorf("translateSources names %s, which does not exist: %v", rel, err)
		}
	}
}

// buildimage.go and linkall.go must stay out: neither can change a cached
// object, and including them would throw away hours of CPU on every unrelated
// edit.
func TestTranslateSourcesExcludeTheNonTranslationFiles(t *testing.T) {
	for _, rel := range translateSources {
		if strings.HasSuffix(rel, "buildimage.go") || strings.HasSuffix(rel, "linkall.go") {
			t.Errorf("%s cannot affect a translated object and must not key the cache", rel)
		}
	}
}

// ⚠️ REPLACES TestBuildToolsWritesWhatTheDockerfileCopies, on that test's own
// instruction: "if the tools stopped being a prebuilt binary, delete
// build-tools and this test". They did, on 2026-08-23. It is worth recording
// that the old test is what NOTICED -- the Dockerfile was rewritten, and the
// guard failed with the reason and the remedy already written down.
//
// The invariant is now the opposite one, and it is stronger. The Dockerfile
// builds NOTHING: no `RUN` at all. Every file the image gains is a Bazel output
// staged by //builder:stage, which depends on the pipeline binary, so the image
// cannot ship a stale one.
//
// ⚠️ What a false pass would look like: this reads the real Dockerfile, so an
// empty or missing file would fail the read rather than pass vacuously, and the
// COPY assertion below fails if the file stops being a packaging step at all.
func TestDockerfileBuildsNothing(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "builder", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}

	var runs []string
	sawCopy := false
	for i, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		switch strings.ToUpper(f[0]) {
		case "RUN":
			runs = append(runs, fmt.Sprintf("line %d: %s", i+1, strings.TrimSpace(line)))
		case "COPY":
			sawCopy = true
		}
	}

	if len(runs) != 0 {
		t.Errorf("builder/Dockerfile has %d RUN line(s):\n  %s\n\n"+
			"The image is packaging only. Anything that BUILDS belongs in Bazel:\n"+
			"  //builder            the LLVM companion tools\n"+
			"  //runtime            the C shims, and net-wasmedge\n"+
			"  //runtime/loopback   net-loopback\n"+
			"  //runtime/browser    net-browser\n"+
			"  //cmd/raptormark     the pipeline binary\n"+
			"and reaches the image through //builder:stage.",
			len(runs), strings.Join(runs, "\n  "))
	}
	if !sawCopy {
		t.Error("builder/Dockerfile copies nothing; it is supposed to package " +
			"//builder:stage into the base image")
	}
}
