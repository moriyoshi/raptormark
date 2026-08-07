package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"raptormark/internal/pipeline"
)

// TestBuildCommandDrivesTheWholePipeline is the guard for the ONLY production
// path through discovery and fusing.
//
// Every other test in this suite assembles the pipeline itself -- its own
// closure, its own fuse, its own registry and maps. That is deliberate (each
// isolates one stage), but it means the stages had no single caller, and for
// three sessions `image.Plugins` was carried in TODO.md as an unwired helper
// when the truth was that `cmd/raptormark` had no discovery or fuse command at
// all. `internal/pipeline` is that caller; this is what keeps it working.
//
// # What a PASS would look like if the claim were false
//
//   - A driver that built a module but never ran discovery would still produce
//     app.wasm. Guarded: the fixture's plugins are reachable ONLY by dlopen --
//     nothing DT_NEEDEDs them -- so if discovery did not find them, the guest's
//     dlopen fails and PGDL-OK never prints.
//   - A driver that discovered them but fused them into the main image would
//     also print PGDL-OK. Guarded by asserting Units >= 2 in the Result.
//   - A driver that produced a module nobody can run would pass any check that
//     only looked at the filesystem. Guarded by actually running it under
//     wasmedge with its sidecar.
func TestBuildCommandDrivesTheWholePipeline(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	// The same fixture the dlopen differential uses: a dynamically linked host
	// plus two extensions in postgres's real $libdir that nothing links against.
	buildPgDlopenFixture(t, ctx, dir)

	out := filepath.Join(dir, "build")
	c := &pipeline.Build{
		Image:       pgExtFixture,
		Out:         out,
		Builder:     img,
		ObjectCache: os.Getenv("RAPTORMARK_OBJECT_CACHE"),
		Plugins:     "auto",
		Profile:     "wasmedge",
		MaxClosure:  10000,
	}
	res, err := c.BuildForTest(ctx)
	if err != nil {
		t.Fatalf("raptormark build: %v", err)
	}
	t.Logf("built %s: %d program(s), %d unit(s), shared layout=%v",
		res.Module, len(res.Programs), len(res.Units), res.SharedLayout)

	// Discovery ran and produced UNITS, not just a bigger main image. The
	// fixture installs 2 extensions; debian:trixie-slim adds its own OpenSSL
	// modules, so this is a floor rather than an equality -- an exact count
	// would break on a base-image change for a reason unrelated to the driver.
	if len(res.Units) < 2 {
		t.Errorf("the build produced %d unit(s), want at least the fixture's 2 "+
			"extensions. Either discovery did not run, or the plugins were fused "+
			"into the main image instead of as their own units.", len(res.Units))
	}
	if len(res.Programs) < 1 {
		t.Errorf("the build produced no programs")
	}
	// ⚠️ The shared layout is a property worth NOTICING, not asserting: an
	// overflow is a legitimate degradation (see fuse.FuseClosure) and failing on
	// it would make a large image unbuildable in exchange for an optimization.
	// But it was also silently false once, when guest paths were passed to
	// PlanLayoutFor and the fallback reported the wrong cause -- so it is logged
	// above and remarked on here.
	if !res.SharedLayout {
		t.Logf("note: no shared layout. Legitimate on a large closure; on this " +
			"one-program fixture it suggests the planner was given bad input.")
	}

	for _, f := range []string{res.Module, res.Sidecar} {
		st, err := os.Stat(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if st.Size() == 0 {
			t.Fatalf("%s is empty", f)
		}
	}

	// THE ASSERTION THAT MATTERS: the artifact runs, and the guest's own dlopen
	// checks pass. `runWasmIn` mounts the module's directory, so the sidecar
	// beside it is /rootfs.img to the guest.
	got := runWasmIn(t, ctx, res.Module, nil,
		[]string{"RAPTORMARK_ROOTFS=/rootfs.img"}, "/:/out")
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(got, "PGDL-OK") {
		t.Errorf("the module the driver built did not reach PGDL-OK.\n"+
			"The plugins are reachable only by dlopen, so this is what proves "+
			"discovery found them AND the dlopen map names them.\nOutput:\n%s", got)
	}
}
