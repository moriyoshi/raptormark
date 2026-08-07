// Package e2e attests the recovered pipeline end to end: a real aarch64 binary
// goes through internal/image, internal/translate and internal/link, into the
// builder image's translate-one and link-all, and out as a WASI module that runs
// under wasmedge and prints what the native binary would.
//
// These tests are opt-in because they need Docker, a built raptormark-builder
// image, and minutes of lifting:
//
//	RAPTORMARK_E2E=1 go test ./e2e/ -v -timeout 60m
//	RAPTORMARK_E2E=1 RAPTORMARK_BUILDER=raptormark-builder:mytag go test ./e2e/ -v -timeout 60m
//
// They are gated by an environment variable rather than a build tag so they
// still compile and vet as part of the normal build and cannot rot silently.
//
// Guest programs are compiled inside the builder image rather than taken from
// the surviving raptormark-test-* fixture images: those fixtures are themselves
// unreproducible artifacts of the lost tree, so depending on them would make
// this suite unrunnable the moment they are pruned.
package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"raptormark/internal/fuse"
	"raptormark/internal/image"
	"raptormark/internal/link"
	"raptormark/internal/rootfs"
	"raptormark/internal/translate"
)

const defaultBuilder = "raptormark-builder:latest"

func builderImage() string {
	if v := os.Getenv("RAPTORMARK_BUILDER"); v != "" {
		return v
	}
	return defaultBuilder
}

// requireE2E skips unless the suite is explicitly enabled and its prerequisites
// are actually present, so an unconfigured machine reports "skipped", not
// "failed".
func requireE2E(t *testing.T) string {
	t.Helper()
	if os.Getenv("RAPTORMARK_E2E") != "1" {
		t.Skip("set RAPTORMARK_E2E=1 to run end-to-end tests (needs Docker and a builder image)")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	img := builderImage()
	out, err := exec.Command("docker", "image", "inspect",
		"--format", "{{.Created}} {{index .Config.Labels \"raptormark.base_id\"}}", img).Output()
	if err != nil {
		t.Skipf("builder image %s not present; build it with `raptormark build-image`", img)
	}
	// Which toolchain a failure came from is otherwise invisible, and
	// `raptormark build-image` tags side builds (p29, p30, ...) without moving `latest`.
	// A stale default image fails deep inside elflift with an error that looks
	// like a defect in the input -- see .agents/docs/JOURNAL.md on
	// `__wrap_main`.
	t.Logf("builder image %s (created %s); override with RAPTORMARK_BUILDER",
		img, strings.TrimSpace(string(out)))
	return img
}

// objectCache is the store RAPTORMARK_OBJECT_CACHE names, or nil when it is
// unset. Translating a fused guest is ~30 minutes of codegen, and the key is
// content-addressed on the base image, the translate-one sources and the ELF — so a
// change confined to runtime/, which only link-all consumes, leaves every
// object valid and reruns cost the link alone. It is opt-in so a run that must
// exercise the real lifter can simply not set it.
var objectCache = translate.CacheFromEnv()

// inlineCallHistory reports whether this run exercises elfconv patch 0060's
// inlined call history end to end: modules are BUILT with it and ecvisor is
// told to USE it. `RAPTORMARK_E2E_INLINE_CH=1`.
//
// It has to drive both halves or it proves nothing. The feature needs the build
// flag for the fast path to exist AND the runtime variable for it to be taken,
// and either one missing leaves every guest BL on the ordinary call path — which
// is the configuration the suite already covers. A run that set only one of them
// would pass while testing nothing new.
//
// Off by default: the DEFAULT path is what ships, so that is what an unqualified
// `go test ./e2e/` must exercise.
func inlineCallHistory() bool {
	return os.Getenv("RAPTORMARK_E2E_INLINE_CH") == "1"
}

// wasmEdgeEnv is the `--env` list every module run needs, which today is the
// call-history gate or nothing. wasmedge does NOT inherit the host environment,
// so a variable that is not passed here simply does not reach the guest — a
// gate-on measurement once showed no speedup at all for exactly this reason.
// linkMarkerFlag makes the linked module record that its objects were lifted
// with the inlined call history. It tracks `inlineCallHistory()` for the same
// reason `translateOne` does: the build flag and this marker have to agree, and
// deriving both from one switch is what keeps them agreeing.
func linkMarkerFlag() string {
	if inlineCallHistory() {
		return " --inline-call-history"
	}
	return ""
}

func wasmEdgeEnv() []string {
	if inlineCallHistory() {
		return []string{"RAPTORMARK_ECV_INLINE_CH=1"}
	}
	return nil
}

// translateOne runs one binary through translate-one, serving it from the
// object cache when one is configured. It logs which happened, because a
// 30-minute step completing instantly is otherwise indistinguishable from a
// test that silently skipped it.
func translateOne(t *testing.T, ctx context.Context, b translate.Builder, r translate.Request) {
	t.Helper()
	// Set here rather than at every call site so no test can be left behind:
	// a guest translated without the flag has no fast path to take, and would
	// pass the gate-on run while proving nothing.
	if inlineCallHistory() {
		r.Options.InlineCallHistory = true
	}
	start := time.Now()
	hit, err := b.RunCached(ctx, objectCache, r)
	if err != nil {
		t.Fatalf("translating %s: %v", r.ModuleID, err)
	}
	if hit {
		t.Logf("%s: served from the object cache (%s)", r.ModuleID, time.Since(start).Round(time.Millisecond))
		return
	}
	t.Logf("%s: translated in %s", r.ModuleID, time.Since(start).Round(time.Second))
}

// defaultTestBudget is how long one test may take, and it is a real ceiling:
// a translation that overruns it is KILLED mid-codegen, which surfaces as
// `translate: <id>: signal: killed` rather than as anything resembling a
// timeout. 45 minutes is chosen so the slow arm stays a usable signal.
const defaultTestBudget = 45 * time.Minute

// ctxFor bounds one test. `RAPTORMARK_E2E_BUDGET` raises it, as a Go duration
// ("90m", "2h").
//
// It exists because a configuration can be legitimately slower rather than
// broken. `--inline-call-history` emits four basic blocks per call site, and on
// the fused OpenSSL closure that took translation from 30m51s to past 45
// minutes -- the test failed on the BUDGET while the feature was working. A
// fixed ceiling cannot tell those apart, so raising it is how you find out
// which one you have.
//
// Raise it deliberately, not reflexively: the ceiling is what keeps a
// pathological translation from turning the suite into an overnight job, and
// `go test -timeout` has to be raised alongside it or the whole binary is killed
// first.
func testBudget(t *testing.T) time.Duration {
	t.Helper()
	v := os.Getenv("RAPTORMARK_E2E_BUDGET")
	if v == "" {
		return defaultTestBudget
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		// Not a warning: a mistyped budget would silently reinstate the default
		// and the run would look like it had honoured the request.
		t.Fatalf("RAPTORMARK_E2E_BUDGET=%q is not a positive Go duration: %v", v, err)
	}
	return d
}

func ctxFor(t *testing.T) context.Context {
	t.Helper()
	budget := testBudget(t)
	if budget != defaultTestBudget {
		t.Logf("per-test budget raised to %s via RAPTORMARK_E2E_BUDGET", budget)
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	t.Cleanup(cancel)
	return ctx
}

// dockerRun runs a bash command in the builder image with the login shell, which
// is where WASI_SDK_PATH and the wasmedge/llvm tooling come from.
func dockerRun(ctx context.Context, mounts []string, script string) (string, error) {
	args := []string{"run", "--rm"}
	args = append(args, mounts...)
	args = append(args, "--entrypoint", "bash", builderImage(), "--login", "-c", script)
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return string(out), err
}

// compileGuest builds a static aarch64 guest inside the builder image. The image
// runs natively on aarch64, so its own gcc is the cross-compiler.
func compileGuest(t *testing.T, ctx context.Context, dir, name, src string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".c"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx, []string{"-v", abs + ":/w"},
		fmt.Sprintf("gcc -static -O2 -o /w/%s /w/%s.c", name, name))
	if err != nil {
		t.Fatalf("compiling guest %s: %v\n%s", name, err, out)
	}
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("guest %s was not produced: %v", name, err)
	}
	return p
}

// runWasm executes a module under wasmedge and returns its combined output.
func runWasm(t *testing.T, ctx context.Context, wasmPath string) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Dir(wasmPath))
	if err != nil {
		t.Fatal(err)
	}
	cmd := "wasmedge --enable-all"
	// Before the module path: wasmedge stops reading its own flags there.
	for _, e := range wasmEdgeEnv() {
		cmd += " --env " + e
	}
	out, err := dockerRun(ctx, []string{"-v", dir + ":/out"},
		cmd+" /out/"+filepath.Base(wasmPath))
	if err != nil {
		t.Fatalf("running %s: %v\n%s", wasmPath, err, out)
	}
	return out
}

// externalSymbols lists the defined external symbols in an object. What the
// ecvisor link actually requires is that these sets be DISJOINT across programs
// — not that they be small; see TestEcvisorTwoProgramsLinkWithoutCollision.
func externalSymbols(t *testing.T, ctx context.Context, objPath string) []string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Dir(objPath))
	if err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx, []string{"-v", dir + ":/out"},
		"llvm-nm-16 --defined-only --extern-only /out/"+filepath.Base(objPath)+" 2>/dev/null | awk '{print $NF}' || true")
	if err != nil {
		t.Fatalf("listing symbols in %s: %v\n%s", objPath, err, out)
	}
	var syms []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" && !strings.Contains(s, " ") {
			syms = append(syms, s)
		}
	}
	return syms
}

const guestSrc = `#include <stdio.h>
int main(void) { puts("%s"); return 0; }
`

// TestUpstreamRuntime is the baseline: elflift plus elfconv's own C++ runtime
// must produce a module whose output matches the native binary.
func TestUpstreamRuntime(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "guest", fmt.Sprintf(guestSrc, "UPSTREAM-OK"))

	b, err := translate.BuilderFromImage(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	sha, err := translate.FileSHA256(elf)
	if err != nil {
		t.Fatal(err)
	}
	opts := translate.Options{Runtime: "upstream"}
	moduleID := translate.ModuleID(elf, sha)

	outDir := filepath.Join(dir, "out")
	translateOne(t, ctx, b, translate.Request{
		ELF: elf, OutDir: outDir, ModuleID: moduleID, Options: opts,
	})
	if got := runWasm(t, ctx, filepath.Join(outDir, moduleID+".wasm")); !strings.Contains(got, "UPSTREAM-OK") {
		t.Errorf("module output missing guest stdout:\n%s", got)
	}
}

// TestEcvisorSingleProgram exercises the whole reconstructed path: the fragment
// and registry from internal/link, translate-one's ecvisor mode, link-all, and
// the Rust supervisor actually running the guest.
func TestEcvisorSingleProgram(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "guest", fmt.Sprintf(guestSrc, "ECVISOR-OK"))

	b, err := translate.BuilderFromImage(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	// The labels `raptormark build-image` writes are the cache identity.
	if b.BaseID == "" || b.TranslateSH == "" {
		t.Fatal("builder image is missing its identity labels")
	}

	opts := translate.Options{Runtime: "ecvisor"}
	sha, err := translate.FileSHA256(elf)
	if err != nil {
		t.Fatal(err)
	}
	moduleID := translate.ModuleID(elf, sha)
	prog := link.Program{Name: moduleID, Index: 0}

	frag := filepath.Join(dir, "frag_0.c")
	if err := os.WriteFile(frag, []byte(link.FragmentC(prog)), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	translateOne(t, ctx, b, translate.Request{
		ELF: elf, OutDir: outDir, ModuleID: moduleID,
		Fragment: frag, Keep: prog.Symbol(), Options: opts,
	})

	registry, err := link.RegistryC([]link.Program{prog})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "registry.c"), []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}

	wasm := linkAll(t, ctx, dir, outDir, "single.wasm", []string{"/out/" + moduleID + ".o"})
	if got := runWasm(t, ctx, wasm); !strings.Contains(got, "ECVISOR-OK") {
		t.Errorf("ecvisor module output missing guest stdout:\n%s", got)
	}
}

// TestEcvisorTwoProgramsLinkWithoutCollision is the regression guard for the
// defect found during recovery: translate-one's split codegen path externalises
// every cross-part local, leaving thousands of hidden-but-global symbols in each
// object. Two programs then failed to link on the shared remill helpers, so
// multi-binary modules — the whole point of the ecvisor runtime — were broken.
//
// builder/namespace-object tags every local with the module id before the split,
// so those promotions stay unique per program. The invariant is therefore NOT
// that each object exports few symbols — it exports thousands — but that the two
// objects' exported sets are DISJOINT, and that they link and run.
//
// (Suppressing the promotion instead, with llvm-split --preserve-locals, also
// makes the link succeed but defeats splitting at scale: on a fused glibc binary
// it produced one 35.7 MB partition plus 80 small ones. Hence namespacing.)
func TestEcvisorTwoProgramsLinkWithoutCollision(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	b, err := translate.BuilderFromImage(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	opts := translate.Options{Runtime: "ecvisor"}
	outDir := filepath.Join(dir, "out")

	var progs []link.Program
	var entries []link.ExecEntry
	var objs []string
	var exported [][]string
	for i, name := range []string{"alpha", "beta"} {
		elf := compileGuest(t, ctx, dir, name, fmt.Sprintf(guestSrc, strings.ToUpper(name)+"-OK"))
		sha, err := translate.FileSHA256(elf)
		if err != nil {
			t.Fatal(err)
		}
		moduleID := translate.ModuleID(elf, sha)
		prog := link.Program{Name: moduleID, Index: i}
		progs = append(progs, prog)
		entries = append(entries, link.ExecEntry{Path: "/bin/" + name, Hash: moduleID})

		frag := filepath.Join(dir, fmt.Sprintf("frag_%d.c", i))
		if err := os.WriteFile(frag, []byte(link.FragmentC(prog)), 0o644); err != nil {
			t.Fatal(err)
		}
		translateOne(t, ctx, b, translate.Request{
			ELF: elf, OutDir: outDir, ModuleID: moduleID,
			Fragment: frag, Keep: prog.Symbol(), Options: opts,
		})

		obj := filepath.Join(outDir, moduleID+".o")
		exported = append(exported, externalSymbols(t, ctx, obj))
		objs = append(objs, "/out/"+moduleID+".o")
	}

	// The property that actually matters: no symbol is defined by both objects.
	// Without namespacing the overlap is thousands of remill helpers and the
	// link below fails outright.
	first := make(map[string]bool, len(exported[0]))
	for _, s := range exported[0] {
		first[s] = true
	}
	var overlap []string
	for _, s := range exported[1] {
		if first[s] {
			overlap = append(overlap, s)
		}
	}
	if len(overlap) > 0 {
		shown := overlap
		if len(shown) > 5 {
			shown = shown[:5]
		}
		t.Errorf("the two objects define %d symbol(s) in common, e.g. %v — "+
			"builder/namespace-object did not tag them per program", len(overlap), shown)
	}
	// Each program must still export its descriptor, or the registry cannot bind it.
	for i, p := range progs {
		if !slices.Contains(exported[i], p.Symbol()) {
			t.Errorf("program %d does not export %s", i, p.Symbol())
		}
	}

	registry, err := link.RegistryC(progs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "registry.c"), []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}
	// The exec map is what lets an execve of /bin/beta reach program 1.
	execMap, err := link.ExecMap(progs, entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "exec.bin"), execMap, 0o644); err != nil {
		t.Fatal(err)
	}

	wasm := linkAll(t, ctx, dir, outDir, "two.wasm", objs)
	// Program 0 is what runs without an exec; the point here is that the link
	// succeeded at all with two programs present.
	if got := runWasm(t, ctx, wasm); !strings.Contains(got, "ALPHA-OK") {
		t.Errorf("two-program module did not run program 0:\n%s", got)
	}
}

// linkAll runs link-all over the generated registry and objects, and returns the
// host path of the module. It goes through dockerRun's login shell rather than
// translate.Link because it predates that wrapper; both reach the same
// subcommand of the builder tools binary.
// `extra` appends raw link-all flags, e.g. `--profile loopback`. Variadic so
// every existing caller is unchanged.
func linkAll(t *testing.T, ctx context.Context, workDir, outDir, name string, objs []string, extra ...string) string {
	t.Helper()
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		t.Fatal(err)
	}
	absOut, err := filepath.Abs(outDir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx,
		[]string{"-v", absOut + ":/out", "-v", absWork + ":/work"},
		fmt.Sprintf("raptormark-tools link-all --registry /work/registry.c --out /out/%s --objs %q%s%s",
			name, strings.Join(objs, " "), linkMarkerFlag(), extraFlags(extra)))
	if err != nil {
		t.Fatalf("link-all: %v\n%s", err, out)
	}
	return filepath.Join(outDir, name)
}

// extraFlags renders variadic link-all flags as a leading-space-separated
// suffix, or "" when there are none -- so the default command line is
// byte-identical to what it was before the parameter existed.
func extraFlags(extra []string) string {
	if len(extra) == 0 {
		return ""
	}
	return " " + strings.Join(extra, " ")
}

// TestImageDiscoveryClosure builds a small image whose entrypoint is a script
// and checks internal/image resolves the closure the way the postgres image
// needs it to: through the shebang to the interpreter, and through a bare
// command word to a program on PATH — without dragging in unreferenced binaries.
func TestImageDiscoveryClosure(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	build := filepath.Join(dir, "ctx")
	if err := os.MkdirAll(build, 0o755); err != nil {
		t.Fatal(err)
	}
	compileGuest(t, ctx, build, "sh", fmt.Sprintf(guestSrc, "SH"))
	compileGuest(t, ctx, build, "helper", fmt.Sprintf(guestSrc, "HELPER"))
	compileGuest(t, ctx, build, "unused", fmt.Sprintf(guestSrc, "UNUSED"))
	// The interpreter comes from the shebang; helper is named as a bare word.
	if err := os.WriteFile(filepath.Join(build, "entry.sh"), []byte("#!/bin/sh\nhelper\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dockerfile := `FROM scratch
COPY sh /bin/sh
COPY helper /bin/helper
COPY unused /bin/unused
COPY entry.sh /entry.sh
ENV PATH=/bin
ENTRYPOINT ["/entry.sh"]
`
	if err := os.WriteFile(filepath.Join(build, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	tag := "raptormark-e2e-discovery:" + strconv.Itoa(os.Getpid())
	if out, err := exec.CommandContext(ctx, "docker", "build", "-t", tag, build).CombinedOutput(); err != nil {
		t.Fatalf("building test image: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rmi", "-f", tag).Run() })

	cfg, err := image.Inspect(ctx, tag)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Entrypoint) != 1 || cfg.Entrypoint[0] != "/entry.sh" {
		t.Fatalf("entrypoint = %v, want [/entry.sh]", cfg.Entrypoint)
	}

	root := filepath.Join(dir, "root")
	if err := image.ExportRootfs(ctx, tag, root); err != nil {
		t.Fatal(err)
	}
	inv, err := image.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := inv.Scripts["/entry.sh"]; !ok {
		t.Errorf("entry.sh not inventoried as a script; scripts=%v", keys(inv.Scripts))
	}
	if len(inv.Programs) != 3 {
		t.Errorf("found %d programs, want 3: %v", len(inv.Programs), keys(inv.Programs))
	}

	seeds := image.EntrypointSeeds(cfg, inv)
	if !slices.Contains(seeds, "/entry.sh") {
		t.Fatalf("seeds = %v, want to contain /entry.sh", seeds)
	}
	got, err := image.Closure(inv, image.ClosureOptions{Seeds: seeds, PathDirs: image.PathDirs(cfg.Env)})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/bin/sh", "/bin/helper"} {
		if !slices.Contains(got, want) {
			t.Errorf("closure %v missing %s", got, want)
		}
	}
	if slices.Contains(got, "/bin/unused") {
		t.Errorf("closure %v pulled in an unreferenced program", got)
	}
	// Scripts are exec targets but not registry programs — the interpreter runs
	// them, with the file supplied by the rfs sidecar.
	if slices.Contains(got, "/entry.sh") {
		t.Errorf("closure %v lists a script as a program", got)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// fusedGuestSrc is a freestanding program that calls into a shared library, so
// the only way it can run is if the fuser resolved the cross-object call.
const fusedLibSrc = `int foo(void) { return 42; }
`

const fusedMainSrc = `int foo(void);
static void sys_write(const char *s, long n) {
  register long x8 __asm__("x8") = 64; register long x0 __asm__("x0") = 1;
  register const char *x1 __asm__("x1") = s; register long x2 __asm__("x2") = n;
  __asm__ volatile("svc #0" : : "r"(x8), "r"(x0), "r"(x1), "r"(x2) : "memory");
}
static void sys_exit(long c) {
  register long x8 __asm__("x8") = 93; register long x0 __asm__("x0") = c;
  __asm__ volatile("svc #0" : : "r"(x8), "r"(x0) : "memory");
}
void _start(void) { sys_write("FUSED-OK\n", 9); sys_exit(foo() == 42 ? 0 : 1); }
`

// TestFusedDynamicProgram attests the dynamic path: internal/fuse links an
// executable with its shared library into one image, and elflift — with
// patches/0016 — lifts it into a module that runs.
//
// Before the fuser this was impossible: elflift aborts on a bare dynamic
// executable with "__wrap_main code block is not found".
func TestFusedDynamicProgram(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "lib.c"), []byte(fusedLibSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.c"), []byte(fusedMainSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Freestanding on purpose: this exercises fusing itself, without glibc's
	// ifunc and startup machinery.
	out, err := dockerRun(ctx, []string{"-v", abs + ":/w"},
		"cd /w && gcc -shared -nostdlib -fPIC -O2 -o libfoo.so lib.c && "+
			"gcc -nostdlib -O2 -no-pie -o prog main.c -L. -lfoo")
	if err != nil {
		t.Fatalf("building fused guest: %v\n%s", err, out)
	}

	fused, err := fuse.Fuse(filepath.Join(dir, "prog"), fuse.Options{
		LibraryPaths:    []string{dir},
		SkipInterpreter: true,
	})
	if err != nil {
		t.Fatalf("fusing: %v", err)
	}
	fusedPath := filepath.Join(dir, "fused")
	if err := os.WriteFile(fusedPath, fused, 0o755); err != nil {
		t.Fatal(err)
	}

	b, err := translate.BuilderFromImage(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	sha, err := translate.FileSHA256(fusedPath)
	if err != nil {
		t.Fatal(err)
	}
	opts := translate.Options{Runtime: "upstream"}
	moduleID := translate.ModuleID(fusedPath, sha)
	outDir := filepath.Join(dir, "out")
	// Not translateOne: this is the one site whose failure has a specific
	// diagnosis worth naming, and it is what the test exists to report.
	if _, err := b.RunCached(ctx, objectCache, translate.Request{
		ELF: fusedPath, OutDir: outDir, ModuleID: moduleID, Options: opts,
	}); err != nil {
		t.Fatalf("lifting the fused image failed: %v\n\n"+
			"If this reports \"__wrap_main code block is not found\", the builder "+
			"image lacks patches/0016-dynamic-entry-no-wrap-main-thunk.patch; "+
			"rebuild it with `raptormark build-image`.", err)
	}

	got := runWasm(t, ctx, filepath.Join(outDir, moduleID+".wasm"))
	if !strings.Contains(got, "FUSED-OK") {
		t.Errorf("fused module did not run the cross-object call:\n%s", got)
	}
}

// osslFixture is a surviving pre-wipe fixture: a Debian image whose entrypoint
// hashes a file with the distro openssl, over a stripped libcrypto. It is the
// closest thing the project has to a record of what already worked, which is
// exactly why it belongs in the suite -- a freshly pulled image can be harder
// than anything the pipeline ever targeted, and diagnosing that as a lifter bug
// costs days (see .agents/docs/JOURNAL.md, "nginx was a bad baseline").
const osslFixture = "raptormark-tmp-ossldgst:latest"

// runWasmIn runs a module under wasmedge with extra mounts and guest argv.
func runWasmIn(t *testing.T, ctx context.Context, wasmPath string, mounts, env []string, dirMap string, args ...string) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Dir(wasmPath))
	if err != nil {
		t.Fatal(err)
	}
	all := append([]string{"-v", dir + ":/out"}, mounts...)
	cmd := "wasmedge --enable-all"
	if dirMap != "" {
		cmd += " --dir " + dirMap
	}
	// wasmedge only accepts --env before the module path; everything after it
	// is the guest's own argv.
	for _, e := range append(wasmEdgeEnv(), env...) {
		cmd += " --env " + e
	}
	cmd += " /out/" + filepath.Base(wasmPath)
	for _, a := range args {
		cmd += " " + a
	}
	out, err := dockerRun(ctx, all, cmd)
	if err != nil {
		t.Fatalf("running %s: %v\n%s", wasmPath, err, out)
	}
	return out
}

// TestOpenSSLFixtureDiscoverAndFuse is the fast guard: a real image through
// discovery and fusing, which is pure Go and takes seconds. It is where the
// regressions of 2026-08-06 would have been caught -- a symlinked entrypoint
// resolving to nothing, and an ifunc resolver starting with `bti c` making the
// image unfuseable.
func TestOpenSSLFixtureDiscoverAndFuse(t *testing.T) {
	requireFixture(t)
	ctx := ctxFor(t)
	root, entry := discoverOSSL(t, ctx)
	fused := fuseOSSL(t, root, entry)
	if len(fused) < 1<<20 {
		t.Errorf("fused image is implausibly small: %d bytes", len(fused))
	}
	assertNoUnrelocatedPointers(t, fused)
}

// assertNoUnrelocatedPointers is the guard for the RELR defect: Debian's glibc
// packs nearly all of its R_AARCH64_RELATIVE relocations into `.relr.dyn`, and
// `internal/fuse` used to walk only SHT_RELA sections. Nothing about the
// resulting image looks wrong -- it fuses, it lifts, and readelf is happy --
// but every relocated data pointer keeps its link-time value. It surfaced only
// at runtime, as a vtable dispatch to 0x7e1c0 on the first fclose.
//
// A pointer-shaped word in an allocatable, writable data section that is
// non-zero and below the executable base cannot be a fused address, so it is
// either an unrelocated pointer or a small integer. Small integers are common,
// so this checks the one place that is all pointers by construction:
// glibc's `_IO_file_jumps` vtable.
func assertNoUnrelocatedPointers(t *testing.T, fused []byte) {
	t.Helper()
	f, err := elf.NewFile(bytes.NewReader(fused))
	if err != nil {
		t.Fatalf("reading the fused image: %v", err)
	}
	syms, err := f.Symbols()
	if err != nil {
		t.Fatalf("reading symbols: %v", err)
	}
	var vt *elf.Symbol
	for i := range syms {
		if syms[i].Name == "_IO_file_jumps" {
			vt = &syms[i]
			break
		}
	}
	if vt == nil {
		t.Skip("no _IO_file_jumps in this closure (not glibc)")
	}
	data := sectionBytesAt(t, f, vt.Value, vt.Size)
	var bad int
	for off := uint64(0); off+8 <= vt.Size; off += 8 {
		v := binary.LittleEndian.Uint64(data[off:])
		// Slots 0 and 1 of an _IO_jump_t are the reserved dummy words.
		if v != 0 && v < 0x400000 {
			bad++
			if bad == 1 {
				t.Errorf("_IO_file_jumps[%d] = %#x -- a link-time offset, not a fused "+
					"address. .relr.dyn relocations were not applied.", off/8, v)
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d of %d vtable slots are unrelocated", bad, vt.Size/8)
	}
}

func sectionBytesAt(t *testing.T, f *elf.File, vaddr, size uint64) []byte {
	t.Helper()
	for _, sec := range f.Sections {
		if sec.Type == elf.SHT_NOBITS || sec.Addr == 0 {
			continue
		}
		if vaddr >= sec.Addr && vaddr+size <= sec.Addr+sec.Size {
			d, err := sec.Data()
			if err != nil {
				t.Fatalf("reading %s: %v", sec.Name, err)
			}
			return d[vaddr-sec.Addr:]
		}
	}
	t.Fatalf("no section covers %#x..%#x", vaddr, vaddr+size)
	return nil
}

// TestOpenSSLFixtureEndToEnd drives the same image all the way to a running
// module. Gated separately because the final codegen is the known bottleneck:
// translate-one compiles the lifted module in one -O3 clang invocation, and for
// a fused openssl that is ~162 MB of bitcode. See .agents/docs/JOURNAL.md
// on `llvm-split --preserve-locals` and the missing `namespace-object` step.
// TestOpenSSLFixtureEndToEnd exercises elfconv's OWN single-binary runtime on
// the fused openssl fixture. It needs its own opt-in beyond RAPTORMARK_E2E_SLOW,
// and the reason is that it CANNOT PASS at any budget this suite can offer.
//
// `runOSSL` passes Runtime "upstream", and `codegenUpstream` never calls
// `splitAndCompile` -- it hands ~162 MB of bitcode to ONE serial `clang++ -O3`.
// Measured 2026-08-10: it died at exactly 2700.20 s with no assertion having
// run, which is `ctxFor`'s 45-minute budget to the second. Raising the budget
// does not fix it either; the codegen is hours.
//
// Left in the suite and skipped rather than deleted, because the SLOW ARM IS AN
// INSTRUMENT. While this ran there, `RAPTORMARK_E2E_SLOW=1` could never be
// green, so a genuine regression in the slow fixtures was indistinguishable from
// the standing failure -- the arm reported nothing either way. The shipping path
// is covered by TestOpenSSLFixtureEcvisorEndToEnd, which uses the split codegen
// and passes in ~1945 s.
//
// Set RAPTORMARK_E2E_UPSTREAM=1 to run it anyway, on a machine and a deadline
// that can take it.
func TestOpenSSLFixtureEndToEnd(t *testing.T) {
	if os.Getenv("RAPTORMARK_E2E_SLOW") != "1" {
		t.Skip("set RAPTORMARK_E2E_SLOW=1: final codegen of a fused openssl is hours, see .agents/docs/JOURNAL.md")
	}
	if os.Getenv("RAPTORMARK_E2E_UPSTREAM") != "1" {
		t.Skip("set RAPTORMARK_E2E_UPSTREAM=1: this test cannot pass inside ctxFor's " +
			"45-minute budget (~162 MB through one serial clang++ -O3, measured to die at " +
			"2700.20 s), and leaving it in the slow arm made that arm useless as a signal. " +
			"The shipping path is covered by TestOpenSSLFixtureEcvisorEndToEnd.")
	}
	img := requireE2E(t)
	ctx := ctxFor(t)
	requireFixture(t)
	root, entry := discoverOSSL(t, ctx)
	fused := fuseOSSL(t, root, entry)
	fusedPath := filepath.Join(t.TempDir(), "openssl.fused")
	if err := os.WriteFile(fusedPath, fused, 0o755); err != nil {
		t.Fatal(err)
	}
	runOSSL(t, ctx, img, root, fusedPath)
}

func requireFixture(t *testing.T) {
	t.Helper()
	if os.Getenv("RAPTORMARK_E2E") != "1" {
		t.Skip("set RAPTORMARK_E2E=1 to run end-to-end tests")
	}
	if err := exec.Command("docker", "image", "inspect", osslFixture).Run(); err != nil {
		t.Skipf("fixture %s not present (it is unreproducible; see .agents/docs/JOURNAL.md)", osslFixture)
	}
}

// discoverOSSL exports the fixture and resolves its entrypoint program.
func discoverOSSL(t *testing.T, ctx context.Context) (string, image.Executable) {
	t.Helper()
	cfg, err := image.Inspect(ctx, osslFixture)
	if err != nil {
		t.Fatalf("inspecting %s: %v", osslFixture, err)
	}
	root := t.TempDir()
	if err := image.ExportRootfs(ctx, osslFixture, root); err != nil {
		t.Fatalf("exporting rootfs: %v", err)
	}
	inv, err := image.Scan(root)
	if err != nil {
		t.Fatalf("scanning rootfs: %v", err)
	}
	seeds := image.EntrypointSeeds(cfg, inv)
	closure, err := image.Closure(inv, image.ClosureOptions{
		Seeds: seeds, PathDirs: image.PathDirs(cfg.Env), Max: 10000,
	})
	if err != nil {
		t.Fatalf("closure: %v", err)
	}
	const entryPath = "/usr/bin/openssl"
	if !slices.Contains(closure, entryPath) {
		t.Fatalf("closure does not contain %s: %v", entryPath, closure)
	}
	entry, ok := inv.Programs[entryPath]
	if !ok {
		t.Fatalf("%s is not an inventoried program", entryPath)
	}
	t.Logf("discovery: %d programs, %d scripts, entry %s", len(closure), len(inv.Scripts), entryPath)
	return root, entry
}

// fuseOSSL links the entry program with its libraries into one image.
func fuseOSSL(t *testing.T, root string, entry image.Executable) []byte {
	t.Helper()
	fused, err := fuse.Fuse(entry.HostPath, fuse.Options{
		LibraryPaths: []string{
			filepath.Join(root, "lib"),
			filepath.Join(root, "usr/lib"),
			filepath.Join(root, "lib/aarch64-linux-gnu"),
			filepath.Join(root, "usr/lib/aarch64-linux-gnu"),
		},
	})
	if err != nil {
		t.Fatalf("fusing openssl: %v\n\n"+
			"If this reports an unsupported instruction 0xd503245f, the ifunc "+
			"evaluator is not skipping the HINT space (bti c).", err)
	}
	t.Logf("fused: %d bytes", len(fused))
	return fused
}

// runOSSL translates the fused image and checks the module reproduces the
// digest the input file actually has.
func runOSSL(t *testing.T, ctx context.Context, img, root, fusedPath string) {
	t.Helper()
	b, err := translate.BuilderFromImage(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	sha, err := translate.FileSHA256(fusedPath)
	if err != nil {
		t.Fatal(err)
	}
	opts := translate.Options{Runtime: "upstream"}
	moduleID := translate.ModuleID(fusedPath, sha)
	outDir := filepath.Join(filepath.Dir(fusedPath), "out")
	translateOne(t, ctx, b, translate.Request{
		ELF: fusedPath, OutDir: outDir, ModuleID: moduleID, Options: opts,
	})

	// --- run and check against the file's real digest --------------------
	want, err := sha256OfFile(filepath.Join(root, "etc/os-release"))
	if err != nil {
		t.Fatal(err)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	got := runWasmIn(t, ctx, filepath.Join(outDir, moduleID+".wasm"),
		[]string{"-v", absRoot + ":/guest:ro"}, nil, "/:/guest",
		"dgst", "-sha256", "/etc/os-release")
	if !strings.Contains(got, want) {
		t.Errorf("module did not produce the file's digest\nwant substring: %s\ngot:\n%s", want, got)
	}
}

func sha256OfFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// TestOpenSSLFixtureEcvisorEndToEnd is the real target: the same image through
// discovery, fusing, the ecvisor translation path, the rfs sidecar, and a run
// that must reproduce the digest the file actually has.
//
// This is the path TestOpenSSLFixtureEndToEnd cannot cover. That one uses
// elfconv's C++ runtime, which talks to WASI directly, so a `--dir` mount is
// enough. ecvisor mounts an rfs sidecar instead and takes its argv/env from the
// boot record inside it, so a run needs internal/rootfs as well.
func TestOpenSSLFixtureEcvisorEndToEnd(t *testing.T) {
	if os.Getenv("RAPTORMARK_E2E_SLOW") != "1" {
		t.Skip("set RAPTORMARK_E2E_SLOW=1: lifting and compiling a fused openssl takes ~30 min")
	}
	img := requireE2E(t)
	ctx := ctxFor(t)
	requireFixture(t)
	root, entry := discoverOSSL(t, ctx)

	work := t.TempDir()
	fusedPath := filepath.Join(work, "openssl.fused")
	if err := os.WriteFile(fusedPath, fuseOSSL(t, root, entry), 0o755); err != nil {
		t.Fatal(err)
	}

	// The sidecar carries both the filesystem and the command line.
	want, err := sha256OfFile(filepath.Join(root, "etc/os-release"))
	if err != nil {
		t.Fatal(err)
	}
	image, stats, err := rootfs.Build(root, rootfs.Options{Boot: &rootfs.Boot{
		Argv: []string{"openssl", "dgst", "-sha256", "/etc/os-release"},
		Cwd:  "/",
	}})
	if err != nil {
		t.Fatalf("building the rfs sidecar: %v", err)
	}
	t.Logf("sidecar: %d bytes (%d dirs, %d files, %d symlinks, %d skipped)",
		len(image), stats.Dirs, stats.Files, stats.Symlinks, stats.Skipped)
	if err := os.WriteFile(filepath.Join(work, "rootfs.img"), image, 0o644); err != nil {
		t.Fatal(err)
	}

	// Translate, then link. The program name must match the module id: the
	// registry binds `_ecv<tag>_*` symbols that namespace-object renames by tag.
	const name = "openssl_ecv"
	prog := link.Program{Name: name, Index: 0}
	fragment := filepath.Join(work, "frag.c")
	if err := os.WriteFile(fragment, []byte(link.FragmentC(prog)), 0o644); err != nil {
		t.Fatal(err)
	}
	registry, err := link.RegistryC([]link.Program{prog})
	if err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(work, "registry.c")
	if err := os.WriteFile(registryPath, []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}

	b, err := translate.BuilderFromImage(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(work, "out")
	opts := translate.Options{Runtime: "ecvisor"}
	translateOne(t, ctx, b, translate.Request{
		ELF: fusedPath, OutDir: outDir, ModuleID: name,
		Fragment: fragment, Keep: prog.Symbol(), Options: opts,
	})
	wasm := filepath.Join(work, name+".wasm")
	if err := b.Link(ctx, translate.LinkRequest{
		Registry: registryPath,
		Objects:  []string{filepath.Join(outDir, name+".o")},
		Out:      wasm,
	}); err != nil {
		t.Fatalf("linking: %v", err)
	}

	// runWasmIn mounts the module's directory at /out, and rootfs.img sits
	// beside it; the preopen is a path inside the container, not on the host.
	// argv comes from the boot record, so the module takes no arguments.
	got := runWasmIn(t, ctx, wasm, nil,
		[]string{"RAPTORMARK_ROOTFS=/rootfs.img"}, "/:/out")
	if !strings.Contains(got, want) {
		t.Errorf("module did not produce the file's digest\nwant substring: %s\ngot:\n%s", want, got)
	}
}

// nginxAlpineFixture is a public image, unlike the openssl one, so this test is
// reproducible on any machine with Docker.
const nginxAlpineFixture = "nginx:alpine"

// TestNginxAlpineFuseHandlesMusl is the fast guard for the musl path. Every
// other fuse guard here is glibc, and the two libcs fail differently:
//
//   - musl ships 868 bytes of .eh_frame for the whole library (Debian's glibc
//     ships full unwind tables), so boundary recovery has to fall back on
//     computed code pointers, or `libc_start_main_stage2` is never lifted.
//   - Alpine's libc.musl-aarch64.so.1 is a SYMLINK to ld-musl-aarch64.so.1:
//     libc and the interpreter are one file. De-duplicating DT_NEEDED by name
//     fused it twice, giving the guest two heaps and two errno.
func TestNginxAlpineFuseHandlesMusl(t *testing.T) {
	if os.Getenv("RAPTORMARK_E2E") != "1" {
		t.Skip("set RAPTORMARK_E2E=1 to run end-to-end tests")
	}
	ctx := ctxFor(t)
	if err := exec.Command("docker", "image", "inspect", nginxAlpineFixture).Run(); err != nil {
		t.Skipf("%s not present locally; docker pull it to run this", nginxAlpineFixture)
	}
	root := t.TempDir()
	if err := image.ExportRootfs(ctx, nginxAlpineFixture, root); err != nil {
		t.Fatalf("exporting rootfs: %v", err)
	}
	inv, err := image.Scan(root)
	if err != nil {
		t.Fatalf("scanning rootfs: %v", err)
	}
	const entryPath = "/usr/sbin/nginx"
	entry, ok := inv.Programs[entryPath]
	if !ok {
		t.Fatalf("%s is not an inventoried program", entryPath)
	}
	fused, err := fuse.Fuse(entry.HostPath, fuse.Options{
		LibraryPaths: []string{filepath.Join(root, "lib"), filepath.Join(root, "usr/lib")},
	})
	if err != nil {
		t.Fatalf("fusing nginx: %v", err)
	}
	t.Logf("fused: %d bytes", len(fused))

	f, err := elf.NewFile(bytes.NewReader(fused))
	if err != nil {
		t.Fatal(err)
	}
	syms, err := f.Symbols()
	if err != nil {
		t.Fatal(err)
	}

	// One definition per name. musl has no symbol versioning, so a name defined
	// at two addresses means a library was fused twice -- which is exactly what
	// the ld-musl symlink caused. (This check would not be valid on glibc,
	// where versioned symbols legitimately repeat a name.)
	addrs := map[string]map[uint64]bool{}
	for _, s := range syms {
		if elf.ST_TYPE(s.Info) != elf.STT_FUNC || s.Value == 0 || s.Size == 0 {
			continue
		}
		if strings.HasPrefix(s.Name, "_ecv_") {
			continue // synthesized boundaries, not real definitions
		}
		// Every shared object has its own crti.o `_init`/`_fini`, so these
		// legitimately appear once per library in the closure.
		if s.Name == "_init" || s.Name == "_fini" {
			continue
		}
		if addrs[s.Name] == nil {
			addrs[s.Name] = map[uint64]bool{}
		}
		addrs[s.Name][s.Value] = true
	}
	var dups []string
	for name, at := range addrs {
		if len(at) > 1 {
			dups = append(dups, name)
		}
	}
	if len(dups) > 0 {
		sort.Strings(dups)
		if len(dups) > 5 {
			dups = dups[:5]
		}
		t.Errorf("%d symbols defined at more than one address (e.g. %v) -- a library "+
			"was fused twice; check DT_NEEDED de-duplication by path and soname",
			len(dups), dups)
	}

	// musl's libc_start_main_stage2 is reachable only through a laundered
	// function pointer. Without code-pointer recovery it is never lifted and
	// the guest dies at _ecv_unreached before main.
	start, ok := addrs["__libc_start_main"]
	if !ok {
		t.Fatal("__libc_start_main not found in the fused image")
	}
	var lsm uint64
	for a := range start {
		lsm = a
	}
	stage2 := lsm - 0x38
	if !coveredByAnyFunc(syms, stage2) {
		t.Errorf("no function covers %#x (libc_start_main_stage2, just below "+
			"__libc_start_main at %#x) -- computed code pointers were not recovered",
			stage2, lsm)
	}
}

func coveredByAnyFunc(syms []elf.Symbol, addr uint64) bool {
	for _, s := range syms {
		if elf.ST_TYPE(s.Info) != elf.STT_FUNC || s.Size == 0 {
			continue
		}
		if addr >= s.Value && addr < s.Value+s.Size {
			return true
		}
	}
	return false
}

// The gate-on run is worth nothing if the environment variable never reaches
// the guest, and a passing suite cannot tell you that it did: with the gate off
// every test still passes, because the fast path is an optimisation and the slow
// arm is the same behaviour. So the plumbing is asserted directly.
//
// This is not hypothetical. A gate-on MEASUREMENT once showed no speedup at all
// because `wasmedge` does not inherit the host environment and the variable was
// never passed with `--env` -- the run looked fine and proved nothing.
//
// Host-side and Docker-free, so it runs in the ordinary `go test ./...`.
func TestInlineCallHistoryGateReachesTheGuest(t *testing.T) {
	t.Setenv("RAPTORMARK_E2E_INLINE_CH", "")
	if got := wasmEdgeEnv(); len(got) != 0 {
		t.Errorf("no gate requested, but the run would pass %q", got)
	}
	if got := linkMarkerFlag(); got != "" {
		t.Errorf("no gate requested, but the link would carry %q", got)
	}
	if inlineCallHistory() {
		t.Error("the gate must be off unless explicitly requested")
	}

	t.Setenv("RAPTORMARK_E2E_INLINE_CH", "1")
	if !inlineCallHistory() {
		t.Fatal("RAPTORMARK_E2E_INLINE_CH=1 did not enable the gate")
	}
	if got := wasmEdgeEnv(); !slices.Contains(got, "RAPTORMARK_ECV_INLINE_CH=1") {
		t.Errorf("the gate is on but the guest would not be told: %q", got)
	}
	// And the LINK has to record the marker, or ecvisor refuses the gate and the
	// run silently exercises the default path -- passing while proving nothing.
	// This is the same failure shape as the missing `--env`, one layer down.
	if got := linkMarkerFlag(); !strings.Contains(got, "--inline-call-history") {
		t.Errorf("the gate is on but the module would carry no build marker: %q", got)
	}
}

// The budget is a ceiling that KILLS a translation mid-codegen, so a mistyped
// value silently reinstating the default would make a run look like it had
// honoured a request it ignored. Host-side and Docker-free.
func TestTestBudgetIsConfigurable(t *testing.T) {
	t.Setenv("RAPTORMARK_E2E_BUDGET", "")
	if got := testBudget(t); got != defaultTestBudget {
		t.Errorf("unset budget = %s, want the %s default", got, defaultTestBudget)
	}
	t.Setenv("RAPTORMARK_E2E_BUDGET", "90m")
	if got := testBudget(t); got != 90*time.Minute {
		t.Errorf("budget = %s, want 90m", got)
	}
}
