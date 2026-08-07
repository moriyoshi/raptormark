package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"raptormark/internal/pipeline"
)

// TestBuildHostedProfileServesBothTriggers is the last combination in the
// dynamic side-module chain that had never been run: `--profile hosted` on a
// DYNAMIC, MULTI-PROGRAM image, built by the CLI rather than assembled by hand.
//
// # Why each of those words is load-bearing
//
//   - `--profile hosted`: the mid-run loader. Both earlier hosted tests build
//     their closure by hand; this one goes through `raptormark build`, so it
//     also proves the driver's `--side-out --profile` plumbing produces
//     something an embedder can actually drive.
//   - DYNAMIC: a fused dynamic program carries `.ecv.tls`. `sys_execve` used to
//     inherit `dlopen`'s TLS refusal and returned ENOEXEC for exactly this
//     shape, which every `gcc -static` test in this suite missed.
//   - MULTI-PROGRAM: the entry carries the plugin units and the other programs
//     must not. A one-program closure cannot express that.
//
// So this covers, in one run, the three defects the preceding three tests each
// found separately -- which is the point of running the combination rather than
// the parts.
//
// # What a PASS would look like if the claim were false
//
//   - If nothing were loaded on demand, the guest would still run: the units
//     could have been placed before boot. Guarded by `loads served: 2` -- one
//     for the dlopen, one for the execve.
//   - If the host served the dlopen but the execve resolved without a load, the
//     count would be 1. Asserted exactly, not as a floor.
//   - If the guest never reached program B, TWOPROG-OK would be absent.
func TestBuildHostedProfileServesBothTriggers(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	buildTwoProgFixture(t, ctx, dir)

	out := filepath.Join(dir, "build")
	side := filepath.Join(dir, "side")
	c := &pipeline.Build{
		Image:       twoProgFixture,
		Out:         out,
		Builder:     img,
		ObjectCache: os.Getenv("RAPTORMARK_OBJECT_CACHE"),
		Plugins:     "auto",
		Profile:     "hosted",
		SideOut:     side,
		MaxClosure:  10000,
	}
	res, err := c.BuildForTest(ctx)
	if err != nil {
		t.Fatalf("raptormark build --profile hosted --side-out: %v", err)
	}
	t.Logf("built: %d program(s), %d unit(s)", len(res.Programs), len(res.Units))
	if len(res.Programs) < 2 {
		t.Fatalf("want a multi-program closure, got %d", len(res.Programs))
	}

	// The entry is what boots; every other program is reachable only by execve,
	// and only the ONE the guest execs needs to be offered. Offering all of them
	// would still work but would not distinguish "the host served the execve"
	// from "the host placed everything up front".
	entry := artifactFor(t, res.Programs, "/usr/bin/twoa")
	second := artifactFor(t, res.Programs, "/usr/bin/twob")
	plugin := artifactFor(t, res.Units, "/usr/lib/postgresql/17/lib/ext_a.so")
	for _, a := range []pipeline.Artifact{entry, second, plugin} {
		if a.Side == "" {
			t.Fatalf("%s has no side module; --side-out did not reach link-all", a.Guest)
		}
		if _, err := os.Stat(a.Side); err != nil {
			t.Fatalf("side module for %s is missing: %v", a.Guest, err)
		}
	}

	// The harness reads paths inside the container, so everything it touches has
	// to be under the one mount. `dir` is the mount; `out` and `side` are both
	// inside it.
	rel := func(p string) string {
		r, err := filepath.Rel(dir, p)
		if err != nil {
			t.Fatalf("%s is not under %s: %v", p, dir, err)
		}
		return "/w/" + filepath.ToSlash(r)
	}
	harness, err := os.ReadFile("testdata/hostedembedder.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hostedembedder.mjs"), harness, 0o644); err != nil {
		t.Fatal(err)
	}

	args := []string{
		"node", "/w/hostedembedder.mjs",
		"--supervisor", rel(filepath.Join(side, "supervisor.wasm")),
		"--program-size", fmt.Sprint(ecvProgramSizeFrom(t, out)),
		"--dir", rel(out),
		"--env", "RAPTORMARK_ROOTFS=/rootfs.img",
		"--main", rel(entry.Side),
		// OFFERED, not placed: the guest's own dlopen and execve are the only
		// things that can cause either of these to be instantiated.
		"--unit", fmt.Sprintf("%s:%d:%s", plugin.Hash, plugin.Index, rel(plugin.Side)),
		"--unit", fmt.Sprintf("%s:%d:%s", second.Hash, second.Index, rel(second.Side)),
	}

	got, runErr := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, strings.Join(args, " "))
	t.Logf("harness output:\n%s", got)
	if runErr != nil {
		t.Fatalf("the mid-run embedder failed: %v", runErr)
	}

	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(got, "TWOPROG-A dlopen ok") {
		t.Errorf("program A's dlopen did not succeed under the hosted loader")
	}
	if !strings.Contains(got, "TWOPROG-OK") {
		t.Errorf("program B never ran, so the execve trigger did not serve a load.\n" +
			"⚠️ A fused DYNAMIC program carries .ecv.tls, and sys_execve used to " +
			"inherit dlopen's TLS refusal and return ENOEXEC for exactly this shape.")
	}
	// EXACTLY two: the plugin (dlopen) and program B (execve). A floor would
	// pass if only one trigger worked.
	if !strings.Contains(got, "loads served: 2") {
		t.Errorf("expected exactly 2 served loads -- one per trigger. Fewer means a " +
			"unit was reachable without the host, so this exercised the preloaded " +
			"path under a hosted label.")
	}
	if n := strings.Count(got, "woke=1"); n != 2 {
		t.Errorf("expected 2 acknowledged loads with a real waiter, saw %d", n)
	}
	if !strings.Contains(got, "HOSTEDEMBEDDER-COMPLETE exit=0") {
		t.Errorf("the run did not complete cleanly")
	}
}

// artifactFor finds the artifact for a guest path, failing loudly rather than
// returning a zero value -- an empty Side would otherwise reach the harness as
// an empty --unit argument and fail there, far from the cause.
func artifactFor(t *testing.T, arts []pipeline.Artifact, guest string) pipeline.Artifact {
	t.Helper()
	var have []string
	for _, a := range arts {
		if a.Guest == guest {
			return a
		}
		have = append(have, a.Guest)
	}
	t.Fatalf("no artifact for %s; the build produced %v", guest, have)
	return pipeline.Artifact{}
}

// ecvProgramSizeFrom reads sizeof(EcvProgram) out of a fragment the build left
// behind, rather than from a second copy of the struct.
func ecvProgramSizeFrom(t *testing.T, outDir string) int {
	t.Helper()
	frags, _ := filepath.Glob(filepath.Join(outDir, "work", "frag_*.c"))
	if len(frags) == 0 {
		t.Fatalf("no generated fragment under %s/work; cannot determine sizeof(EcvProgram)", outDir)
	}
	b, err := os.ReadFile(frags[0])
	if err != nil {
		t.Fatal(err)
	}
	return ecvProgramSize(t, string(b))
}
