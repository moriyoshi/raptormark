package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"raptormark/internal/fuse"
	"raptormark/internal/image"
	"raptormark/internal/link"
	"raptormark/internal/translate"
)

// Cross-program partition reuse, end to end.
//
// The shared-name path (ECV_SHARED_NAMES + ECV_SHARED_MIN) lets two programs of
// one closure compile a library partition ONCE and serve it to both. Everything
// it depends on is invisible from the outside: a closure-wide address layout, a
// lifted name that carries its guest address, a rule for which definitions may
// share that name, an ODR linkage the compiler must honour, and a partition
// whose bytes do not depend on the order its module was walked in. Any one of
// them regressing turns the whole thing back into two independent builds, and
// nothing else in the suite would notice.
//
// WHY THE EMPTY-OBJECT CHECK IS HERE. The first working version of this path
// used linkonce_odr, which is discardable-if-unused. A shared partition holds
// library BODIES while every caller sits in another partition, so `clang -O1`
// dropped every one of them: 70 of 80 partition objects came out at 314 bytes and
// the library code was compiled nowhere. The partition cache then served 69 empty
// objects across the two programs, and the hit count -- the number this work was
// being steered by -- reported that as success. A reuse test that only counts
// keys would have passed. See JOURNAL.md 2026-08-12.
//
// Gated with the slow fixtures rather than the default arm: the first program is
// a cold translate of a fused glibc closure, which is minutes, and the second
// program is the whole point so neither can be skipped.
//
// WHY THE PAIR IS DIFFERENT SIZES. An earlier version of this test fused
// /bin/echo and /bin/cat, which define 7,613 symbols each, and passed while
// cross-program reuse was in fact impossible for any real closure. A partition's
// identity is its whole membership, so when membership comes from hashing names
// over a fixed bucket count the mean is |symbols| / buckets and two programs of
// DIFFERENT sizes can never produce an equal bucket -- however identical their
// code. Same-size pairs are the one regime where that scheme appears to work.
//
// /bin/echo (7,613 symbols) against /bin/bash (11,838) is the regime that
// matters. Before library-scoped partitioning it reused 0 of 80 partitions while
// sharing 100% of echo's library symbols with bash; after, the second program
// costs ~2 minutes against ~9. See JOURNAL.md 2026-08-12.

// closureFixture is a stock image rather than a pinned one because this test
// needs only two dynamically linked programs over a shared libc, and any Debian
// provides that. It is never pulled -- a missing image skips.
const closureFixture = "debian:trixie-slim"

// closureEntries are the two programs. /bin/echo goes first because it becomes
// program 0, which is what a module runs without an exec, and it reports its own
// argv so the run is checkable without a rootfs. /bin/bash is second because it
// is ~20x larger over the same libc, which is the asymmetry this test exists for.
var closureEntries = []string{"/bin/echo", "/bin/bash"}

func TestSharedNamesReuseAcrossAClosure(t *testing.T) {
	img := requireE2E(t)
	if os.Getenv("RAPTORMARK_E2E_SLOW") != "1" {
		t.Skip("set RAPTORMARK_E2E_SLOW=1: the first program is a cold translate of a fused glibc closure")
	}
	if err := exec.Command("docker", "image", "inspect", closureFixture).Run(); err != nil {
		t.Skipf("fixture %s not present locally (this test does not pull)", closureFixture)
	}
	ctx := ctxFor(t)

	root := t.TempDir()
	if err := image.ExportRootfs(ctx, closureFixture, root); err != nil {
		t.Fatalf("exporting %s: %v", closureFixture, err)
	}

	// FUSE AS A CLOSURE. Fusing one program at a time cannot produce a shared
	// layout: assignBases packs each image densely, so a library's base depends
	// on the other objects in that image alone.
	opts := fuse.Options{LibraryPaths: []string{
		filepath.Join(root, "lib"),
		filepath.Join(root, "usr/lib"),
		filepath.Join(root, "lib/aarch64-linux-gnu"),
		filepath.Join(root, "usr/lib/aarch64-linux-gnu"),
	}}
	var exePaths []string
	for _, e := range closureEntries {
		p := filepath.Join(root, strings.TrimPrefix(e, "/"))
		if _, err := os.Stat(p); err != nil {
			t.Skipf("%s has no %s: %v", closureFixture, e, err)
		}
		exePaths = append(exePaths, p)
	}
	images, rep, err := fuse.FuseClosure(exePaths, opts)
	if err != nil {
		t.Fatalf("FuseClosure: %v", err)
	}
	// A fallback here is not an error -- it is correct, and it silently disables
	// everything below, which is exactly the outcome that must not pass quietly.
	if !rep.Shared {
		t.Fatalf("closure fell back to per-image packing, so nothing can be shared: %s", rep.Reason)
	}
	if rep.SharedMin == 0 {
		t.Fatal("shared layout reports no library boundary; every address would look program-specific")
	}
	t.Logf("shared layout: %d libraries, first library at %#x, top %#x",
		rep.Libraries, rep.SharedMin, rep.Top)

	// The caches. The partition cache must be EMPTY at the start: the claim is
	// about what the second program does not have to compile, and a warm cache
	// cannot show that.
	work := t.TempDir()
	partCache := filepath.Join(work, "partcache")
	if err := os.MkdirAll(partCache, 0o755); err != nil {
		t.Fatal(err)
	}
	// The builder runs as root, so it creates the cache's fan-out directories as
	// root and t.TempDir's own cleanup cannot empty them. Remove them the same
	// way they were made.
	t.Cleanup(func() { removeAsRoot(work, "partcache") })

	t.Setenv("RAPTORMARK_PART_CACHE", partCache)
	// RAPTORMARK_STABLE_SPLIT was set here until 2026-08-23. The stable
	// partitioner is the default now, and this test's claim — that the second
	// program's partitions come from cache — only holds under it, so leaving the
	// default unset is the stronger assertion: it fails if the promotion is ever
	// reverted, which setting the switch would have hidden.
	t.Setenv("RAPTORMARK_SHARED_NAMES", "1")
	t.Setenv("RAPTORMARK_SHARED_MIN", fmt.Sprintf("%#x", rep.SharedMin))
	// Without the ranges the partitioner falls back to hashing names over a fixed
	// bucket count, which is exactly the arrangement this test was written to
	// catch. Set it from the plan rather than the environment, so a stale shell
	// cannot quietly turn this into a test of the old behaviour.
	t.Setenv("RAPTORMARK_LIB_RANGES", fuse.FormatLibRanges(rep.LibRanges))

	b, err := translate.BuilderFromImage(ctx, img)
	if err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(work, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var progs []link.Program
	var objs []string
	var keysAfter []int
	for i, fusedBytes := range images {
		name := strings.ReplaceAll(strings.TrimPrefix(closureEntries[i], "/"), "/", "_")
		fusedPath := filepath.Join(work, name+".fused")
		if err := os.WriteFile(fusedPath, fusedBytes, 0o755); err != nil {
			t.Fatal(err)
		}
		sha, err := translate.FileSHA256(fusedPath)
		if err != nil {
			t.Fatal(err)
		}
		moduleID := translate.ModuleID(fusedPath, sha)
		prog := link.Program{Name: moduleID, Index: i}
		progs = append(progs, prog)

		frag := filepath.Join(work, fmt.Sprintf("frag_%d.c", i))
		if err := os.WriteFile(frag, []byte(link.FragmentC(prog)), 0o644); err != nil {
			t.Fatal(err)
		}
		// b.Run, not translateOne: the object cache would serve the whole object
		// and no partition would be compiled at all, which is the one thing this
		// test is measuring.
		if err := b.Run(ctx, translate.Request{
			ELF: fusedPath, OutDir: outDir, ModuleID: moduleID, Fragment: frag,
			Keep: prog.Symbol(), Options: translate.Options{Runtime: "ecvisor"},
		}); err != nil {
			t.Fatalf("translating %s: %v", closureEntries[i], err)
		}
		objs = append(objs, filepath.Join(outDir, moduleID+".o"))
		keysAfter = append(keysAfter, len(cachedPartitions(t, partCache)))
		t.Logf("%s translated; partition cache holds %d objects", closureEntries[i], keysAfter[i])
	}

	// REUSE. The second program compiles the partitions it does not share with
	// the first and no others, so the cache grows by far less than it did for the
	// first program. Asserted on the SERVED artifacts -- the objects in the cache
	// -- rather than on a hit flag.
	first, second := keysAfter[0], keysAfter[1]-keysAfter[0]
	if first == 0 {
		t.Fatal("the first program cached no partitions at all; the partition cache is not wired up")
	}
	// A fraction, not just "fewer". The old name-hashed scheme reused exactly 0 of
	// 80 here, and a scheme that reused a handful by luck would still be broken;
	// library-scoped partitioning reuses every library-scoped and common partition,
	// measured at 38 of 122. A tenth is well clear of both.
	reused := first - second
	if reused*10 < first {
		t.Errorf("cross-program reuse is %d of %d partitions, under a tenth — "+
			"program 1 cached %d, program 2 added %d. Identical library code must hit; "+
			"check that the partitioner received ECV_LIB_RANGES and scoped its "+
			"partitions to one library each", reused, first, first, second)
	}
	t.Logf("reuse: program 1 cached %d partitions, program 2 added %d (%d of %d served)",
		first, second, first-second, first)

	// COMPLETENESS. A partition that compiled to nothing is the failure that
	// looks like success in the count above.
	//
	// This was a SIZE floor of 1 KB, calibrated when a discarded partition was
	// exactly 314 bytes and the smallest real one was 11 KB. Library-scoped
	// partitioning closed that gap -- a legitimate partition can hold a single
	// `bb_addr_vmas` array and compile to 540 bytes -- and the floor began firing
	// on healthy objects. Size was always a proxy. "Defines nothing" is the
	// property itself, it is exact, and it does not drift as partitions get finer.
	var empty []string
	for _, p := range cachedPartitions(t, partCache) {
		if len(definedSymbols(t, ctx, p)) == 0 {
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			empty = append(empty, fmt.Sprintf("%s (%d bytes)", filepath.Base(p), fi.Size()))
		}
	}
	if len(empty) > 0 {
		shown := empty
		if len(shown) > 5 {
			shown = shown[:5]
		}
		t.Errorf("%d of %d cached partitions define no symbol at all, e.g. %v — "+
			"a shared definition must be emitted even where nothing local uses it "+
			"(weak_odr, not linkonce_odr)", len(empty), len(cachedPartitions(t, partCache)), shown)
	}

	// And it has to run. Two programs now DEFINE the same library symbols, which
	// only links because they are ODR; that the link succeeds and the result
	// executes is the claim a byte count cannot make.
	registry, err := link.RegistryC(progs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "registry.c"), []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}
	linked := make([]string, len(objs))
	for i, o := range objs {
		linked[i] = "/out/" + filepath.Base(o)
	}
	wasm := linkAll(t, ctx, work, outDir, "closure.wasm", linked)
	const marker = "SHARED-CLOSURE-OK"
	if got := runWasmIn(t, ctx, wasm, nil, nil, "", marker); !strings.Contains(got, marker) {
		t.Errorf("the shared-name module did not run program 0:\n%s", got)
	}
}

// cachedPartitions lists the compiled objects in a partition cache, whose layout
// is <dir>/<key[:2]>/<key>.o.
func cachedPartitions(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*", "*.o"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// removeAsRoot deletes a directory the builder created inside parent. Docker runs
// as root here, so the entries under it are not the invoking user's to unlink.
func removeAsRoot(parent, name string) {
	abs, err := filepath.Abs(parent)
	if err != nil {
		return
	}
	_, _ = dockerRun(context.Background(), []string{"-v", abs + ":/parent"},
		"rm -rf /parent/"+name)
}

// definedSymbols lists what a partition object actually defines. Separate from
// externalSymbols in e2e_test.go, which filters to extern-only: a shared library
// body is weak_odr and hidden, so extern-only would report an intact partition as
// empty and invert the check this exists for.
func definedSymbols(t *testing.T, ctx context.Context, objPath string) []string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Dir(objPath))
	if err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx, []string{"-v", dir + ":/out"},
		"llvm-nm-16 --defined-only /out/"+filepath.Base(objPath)+" 2>/dev/null | awk '{print $NF}' || true")
	if err != nil {
		t.Fatalf("listing symbols in %s: %v\n%s", objPath, err, out)
	}
	var syms []string
	for _, line := range strings.Split(out, "\n") {
		if sym := strings.TrimSpace(line); sym != "" && !strings.Contains(sym, " ") {
			syms = append(syms, sym)
		}
	}
	return syms
}
