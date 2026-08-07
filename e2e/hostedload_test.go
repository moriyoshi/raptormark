package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"raptormark/internal/fuse"
	"raptormark/internal/image"
	"raptormark/internal/link"
	"raptormark/internal/rootfs"
	"raptormark/internal/translate"
)

// TestHostedLoaderServesADlopenMidRun is the end of the dynamic side-module
// chain: a guest calls `dlopen`, and the module that serves it is compiled and
// instantiated IN RESPONSE, while the guest is parked.
//
// # What distinguishes it from every test above it
//
// `TestPostgresStyleDlopenResolvesPerUnit` proves handle-scoped `dlsym` on the
// FLAT module, where every unit's code is linked in and only the merge is
// deferred. `TestEmbedderRunsTheSideModule` proves the nine-step placement
// sequence, but places everything BEFORE `_start` -- the set is fixed at link
// time and the guest never influences it.
//
// Here the units are NOT in the supervisor and are NOT placed at startup. The
// only thing that causes them to exist is the guest asking. That is the shape a
// browser needs, because Chromium will not synchronously compile a module over
// 8 MB on the main thread and this tree's CPython unit is 36.4 MB.
//
// # What a PASS would look like if the claim were false
//
// The trap is that the guest could succeed for reasons that have nothing to do
// with a load being served:
//
//   - if the units were linked into the supervisor, `dlopen` would resolve
//     immediately and no load would happen. Guarded by asserting the harness
//     reports `loads served: 2` AND that the supervisor imports the loader.
//   - if the host placed them before boot, likewise. Guarded because the harness
//     places only `--main` early, and the load lines are printed with the token.
//   - if the wake never reached the guest, it would hang rather than pass -- and
//     the harness fails loudly on `woke=0` rather than waiting.
//   - if `dlsym` ignored the handle, both magics would agree; the guest itself
//     checks they differ, which is what `pgHostSrc` already asserts.
func TestHostedLoaderServesADlopenMidRun(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	buildPgDlopenFixture(t, ctx, dir)

	root, entry := discoverImage(t, ctx, pgExtFixture, "/usr/bin/pgdlhost")
	libs := []string{
		filepath.Join(root, "lib"),
		filepath.Join(root, "usr/lib"),
		filepath.Join(root, "lib/aarch64-linux-gnu"),
		filepath.Join(root, "usr/lib/aarch64-linux-gnu"),
	}
	found, _, err := image.Plugins(root)
	if err != nil {
		t.Fatalf("plugin discovery: %v", err)
	}
	var extras []string
	for _, p := range found {
		if strings.Contains(p.Guest, "/postgresql/") {
			extras = append(extras, p.Host)
		}
	}
	if len(extras) != 2 {
		t.Fatalf("expected the fixture's 2 extensions under postgresql/, got %d", len(extras))
	}
	opts := fuse.Options{LibraryPaths: libs, Extra: extras}
	layout, err := fuse.PlanLayoutFor([]string{entry.HostPath}, opts)
	if err != nil {
		t.Fatalf("planning the layout: %v", err)
	}
	opts.Layout = layout

	mainImg, units, err := fuse.FuseWithUnits(entry.HostPath, opts)
	if err != nil {
		t.Fatalf("fusing with units: %v", err)
	}
	if len(units) != 2 {
		t.Fatalf("expected 2 units, got %d", len(units))
	}

	b, err := translate.BuilderFromImage(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	topts := translate.Options{Runtime: "ecvisor"}
	outDir := filepath.Join(dir, "out")

	images := []struct {
		file  string
		bytes []byte
		guest string
	}{
		{"main.fused", mainImg, ""},
		{"unit_a.fused", units[0].Image, guestPathOf(t, root, units[0].Path)},
		{"unit_b.fused", units[1].Image, guestPathOf(t, root, units[1].Path)},
	}

	var progs []link.Program
	var objs []string
	guestOf := map[int]string{}
	for i, im := range images {
		p := filepath.Join(dir, im.file)
		if err := os.WriteFile(p, im.bytes, 0o755); err != nil {
			t.Fatal(err)
		}
		sha, err := translate.FileSHA256(p)
		if err != nil {
			t.Fatal(err)
		}
		id := translate.ModuleID(p, sha)
		prog := link.Program{Name: id, Index: i}
		progs = append(progs, prog)
		// ⚠️ HOST paths, not guest ones. `translate.Builder.Link` mounts the
		// object directory at /objs and rewrites each entry itself, unlike the
		// `linkAll` helper the flat tests use, which mounts outDir at /out and
		// wants /out/... spellings. Passing the /out form here produces
		// "clang: no such file or directory: /objs/<name>.o", which names the
		// path it derived rather than the one that was wrong.
		objs = append(objs, filepath.Join(outDir, id+".o"))
		guestOf[i] = im.guest

		frag := filepath.Join(dir, fmt.Sprintf("frag_%d.c", i))
		if err := os.WriteFile(frag, []byte(link.FragmentC(prog)), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := b.RunCached(ctx, objectCache, translate.Request{
			ELF: p, OutDir: outDir, ModuleID: id,
			Fragment: frag, Keep: prog.Symbol(), Options: topts,
		}); err != nil {
			t.Fatalf("translating %s: %v", im.file, err)
		}
	}

	if _, err := link.WriteLinkInputs(dir, "registry.c", progs); err != nil {
		t.Fatal(err)
	}
	linked, err := link.ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	execMap, err := link.ExecMap(linked, []link.ExecEntry{
		{Path: "/usr/bin/pgdlhost", Hash: linked[0].Name},
	})
	if err != nil {
		t.Fatal(err)
	}
	dlMap, err := link.DlMap(linked, []link.DlEntry{
		{Path: guestOf[1], Hash: linked[1].Name},
		{Path: guestOf[2], Hash: linked[2].Name},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sidecarImg, _, err := rootfs.Build(root, rootfs.Options{
		ExecMap: execMap,
		DlMap:   dlMap,
		Boot:    &rootfs.Boot{Argv: []string{"/usr/bin/pgdlhost"}, Cwd: "/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "rootfs.img"), sidecarImg, 0o644); err != nil {
		t.Fatal(err)
	}

	// ❗ --profile hosted, and it must reach the SUPERVISOR, not just the flat
	// module. Before 2026-08-24 `supervisorLinkArgs` hardcoded the default
	// archive, so this produced a supervisor with no loader backend at all --
	// which the harness now refuses to run rather than hanging on.
	sideDir := filepath.Join(dir, "side")
	flat := filepath.Join(outDir, "hostedload.wasm")
	if err := b.Link(ctx, translate.LinkRequest{
		Registry: filepath.Join(dir, "registry.c"),
		Objects:  objs,
		Out:      flat,
		SideOut:  sideDir,
		Profile:  "hosted",
	}); err != nil {
		t.Fatalf("link with --side-out --profile hosted: %v", err)
	}

	// The CONTENTS, not a path: ecvProgramSize counts fields in the generated
	// `typedef struct { ... } EcvProgram;`, so it parses the C rather than
	// reading a file. Passing the path made it report that internal/link no
	// longer emits the typedef, which is a true statement about the string it
	// was given and says nothing about internal/link.
	fragSrc := link.FragmentC(progs[0])
	args := []string{
		"node", "/w/hostedembedder.mjs",
		"--supervisor", "/w/side/supervisor.wasm",
		"--program-size", fmt.Sprint(ecvProgramSize(t, fragSrc)),
		"--dir", "/w/out",
		"--env", "RAPTORMARK_ROOTFS=/rootfs.img",
		"--main", "/w/side/" + linked[0].Name + ".side.wasm",
	}
	// The two UNITS are OFFERED, not placed. The harness instantiates one only
	// when the guest's dlopen asks for it by name.
	for i := 1; i <= 2; i++ {
		args = append(args, "--unit",
			fmt.Sprintf("%s:%d:%s", linked[i].Name, i, "/w/side/"+linked[i].Name+".side.wasm"))
	}
	harness, err := os.ReadFile("testdata/hostedembedder.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hostedembedder.mjs"), harness, 0o644); err != nil {
		t.Fatal(err)
	}

	mounts := []string{"-v", mustAbs(t, dir) + ":/w"}
	// THE NEUTRALIZATION, kept rather than done once and thrown away, because a
	// guard is only worth what it can still be shown to catch.
	//
	//   NEUTRALIZE_REFUSE_ALL=1 RAPTORMARK_E2E=1 RAPTORMARK_BUILDER=... \
	//     go test ./e2e/ -run TestHostedLoaderServesADlopenMidRun
	//
	// makes the host refuse every load. Measured 2026-08-24: both dlopens return
	// NULL, the guest prints PGDL-FAIL, and all three assertions below fire.
	// It also shows `dlerror` carrying a real diagnosis -- "the host refused to
	// load this unit" -- which is the defect this whole chain started from: a
	// dlopen that succeeded, a dlsym that returned NULL, and a dlerror that
	// returned NULL too, leaving the guest nothing to report.
	if os.Getenv("NEUTRALIZE_REFUSE_ALL") != "" {
		mounts = append(mounts, "-e", "NEUTRALIZE_REFUSE_ALL=1")
	}
	out, runErr := dockerRun(ctx, mounts, strings.Join(args, " "))
	t.Logf("harness output:\n%s", out)
	if runErr != nil {
		t.Fatalf("the mid-run embedder failed: %v", runErr)
	}

	// 1. The guest's own assertions. It checks that each handle resolves its OWN
	//    Pg_magic_func and that the two addresses differ, so a dlsym that
	//    ignored the handle fails here rather than passing quietly.
	if !strings.Contains(out, "PGDL-OK") {
		t.Errorf("the guest did not report PGDL-OK, so its dlopen/dlsym assertions failed")
	}
	// 2. A load was actually SERVED, twice. Without this the test would pass on
	//    a build where the units were linked in and nothing was ever loaded --
	//    which is exactly what the flat profile does, correctly, and is not what
	//    this test is about.
	if !strings.Contains(out, "loads served: 2") {
		t.Errorf("the host served no side-module load. The guest's dlopen must have " +
			"resolved without one, which means the units were reachable already -- " +
			"so this exercised the preloaded path under a hosted label.")
	}
	// 3. Each load went through the section-8 sequence and was acknowledged to a
	//    guest that was really waiting. The harness fails on woke=0 itself; this
	//    catches a harness that stopped printing it.
	if n := strings.Count(out, "woke=1"); n != 2 {
		t.Errorf("expected 2 acknowledged loads with a waiter, saw %d occurrences of woke=1", n)
	}
	if !strings.Contains(out, "HOSTEDEMBEDDER-COMPLETE exit=0") {
		t.Errorf("the run did not complete cleanly")
	}
}
