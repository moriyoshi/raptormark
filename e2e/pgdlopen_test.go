package e2e

import (
	"context"
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

// The postgres dlopen differential: two plugins defining THE SAME symbols, and
// a guest that must get the right one from each handle.
//
// # The defect this witnesses
//
// `.agents/docs/TODO.md`: postgres:17 ships 79 extensions and "every extension
// defines `Pg_magic_func`, `_PG_init` and `pg_finfo_*`, so a second module
// collides in the flat namespace and first-wins binds the wrong one silently.
// Our `dlsym` compounds it by ignoring the handle and resolving globally."
//
// Before the unit work, `dlopen` returned a sentinel `1` for every path and
// `dlsym` searched one closure-wide `.ecv.dlsyms`. Both handles therefore
// resolved `Pg_magic_func` to whichever definition the fuser saw first, and the
// second extension silently ran the first one's code.
//
// # ⚠️ WHAT A PASS WOULD LOOK LIKE IF THE FIX WERE ABSENT
//
// This is what decides whether the test is worth anything. Under the old
// behaviour:
//
//   - `dlopen` SUCCEEDS for both paths -- it succeeded for every path, always
//   - `dlsym` returns a NON-NULL address for both
//   - calling it returns a perfectly plausible magic number
//
// So "both dlopens worked and both dlsyms were non-null" PASSES under the bug.
// The discriminators are that each magic must match ITS OWN plugin, and that
// the two resolved addresses must DIFFER -- the defect stated directly rather
// than through its effect.
//
// # Why a real glibc guest rather than a freestanding one
//
// The first version of this test used a `-nostdlib` guest calling the
// pseudo-syscalls (`0xF00`/`0xF01`) directly, on the reasoning that the syscall
// is the real entry point of the mechanism. It could not be lifted, and the
// reason is worth keeping: `lifter/TraceManager.cpp` sets the entry function
// inside `for (i = 0; i < func_symbols.size() - 1; i++)`, so an image whose
// entry is the LAST func symbol never gets one -- and a `-O2 -nostdlib` guest
// inlines its statics down to a single `_start`. It died with
// "[ERROR] entry_function is not found."
//
// A real dynamically-linked guest has no such problem, and it tests strictly
// MORE: it goes through libc's own `dlopen`, which `internal/fuse.patchDLStubs`
// rewrites into `movz x8,#nr; svc #0; ret`. The freestanding version skipped
// that rewrite entirely.

const pgExtFixture = "raptormark-tmp-pgdlopen:latest"

// Each plugin defines the SAME symbol names a postgres extension does, with a
// distinct magic. `_PG_init` is here because a real extension has one, and its
// constructor-like role is what a unit's own `.ecv.init` exists to serve.
const pgExtSrcFmt = `int Pg_magic_func(void) { return %d; }
int _PG_init(void) { return %d; }
`

// The loader: postgres's sequence in miniature. dlopen the extension, dlsym its
// magic function, call it, and refuse the answer if it is not the one THAT
// extension defines.
const pgHostSrc = `#include <dlfcn.h>
#include <stdio.h>

typedef int (*magic_fn)(void);

int main(void) {
	static const char *paths[2] = {
		"/usr/lib/postgresql/17/lib/ext_a.so",
		"/usr/lib/postgresql/17/lib/ext_b.so" };
	const int want[2] = { 0xA1, 0xB2 };
	void *h[2] = {0, 0};
	void *sym[2] = {0, 0};
	int fails = 0;

	for (int i = 0; i < 2; i++) {
		h[i] = dlopen(paths[i], RTLD_NOW);
		if (!h[i]) { printf("FAIL dlopen(%s) -> NULL: %s\n", paths[i], dlerror()); fails++; continue; }
		sym[i] = dlsym(h[i], "Pg_magic_func");
		if (!sym[i]) { printf("FAIL dlsym(%s, Pg_magic_func): %s\n", paths[i], dlerror()); fails++; continue; }
		int got = ((magic_fn)sym[i])();
		printf("%s handle=%p magic=0x%X want=0x%X\n", paths[i], h[i], got, want[i]);
		if (got != want[i]) { printf("FAIL wrong magic for %s\n", paths[i]); fails++; }
	}

	/* THE defect, stated directly. Under the old flat table both handles
	   resolve to the SAME address and both calls return the FIRST plugin's
	   magic -- which every check above would still pass if the two plugins
	   happened to agree on a value. */
	if (sym[0] && sym[1] && sym[0] == sym[1]) {
		printf("FAIL both handles resolved Pg_magic_func to one address %p\n", sym[0]); fails++;
	}
	if (h[0] && h[1] && h[0] == h[1]) {
		printf("FAIL both dlopens returned the same handle %p (sentinel)\n", h[0]); fails++;
	}
	/* A handle must not answer for a symbol nothing defines. Nothing in the old
	   behaviour could fail this either, because there was one table. */
	if (h[0] && dlsym(h[0], "definitely_not_here_xyz")) {
		printf("FAIL dlsym invented a symbol\n"); fails++;
	}
	/* And _PG_init must be scoped the same way -- one symbol could be right by
	   luck, two agreeing is the mechanism. */
	if (h[0] && h[1]) {
		void *a = dlsym(h[0], "_PG_init"), *b = dlsym(h[1], "_PG_init");
		if (a && b && a == b) { printf("FAIL _PG_init also collapsed to one address\n"); fails++; }
	}

	printf(fails ? "PGDL-FAIL\n" : "PGDL-OK\n");
	fflush(stdout);
	return fails != 0;
}
`

// guestPathOf turns a unit's HOST path under the exported rootfs back into the
// path the guest names it by -- which is what the dlopen map keys on.
func guestPathOf(t *testing.T, root, hostPath string) string {
	t.Helper()
	rel, err := filepath.Rel(root, hostPath)
	if err != nil {
		t.Fatalf("%s is not under %s: %v", hostPath, root, err)
	}
	return "/" + filepath.ToSlash(rel)
}

// buildPgDlopenFixture builds a container image holding a dynamically linked
// loader and two plugins that nothing DT_NEEDEDs.
func buildPgDlopenFixture(t *testing.T, ctx context.Context, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "host.c"), []byte(pgHostSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	for i, magic := range []int{0xA1, 0xB2} {
		name := fmt.Sprintf("ext_%c.c", 'a'+i)
		if err := os.WriteFile(filepath.Join(dir, name),
			[]byte(fmt.Sprintf(pgExtSrcFmt, magic, magic)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The plugins are NOT linked into the loader -- that is what makes them
	// dlopen-only, and what `fuse.Options.Extra` exists to admit.
	df := "FROM " + builderImage() + ` AS build
COPY host.c ext_a.c ext_b.c /tmp/
RUN gcc -O2 -o /tmp/host /tmp/host.c -ldl \
 && gcc -shared -fPIC -O2 -o /tmp/ext_a.so /tmp/ext_a.c \
 && gcc -shared -fPIC -O2 -o /tmp/ext_b.so /tmp/ext_b.c
FROM debian:trixie-slim
COPY --from=build /tmp/host /usr/bin/pgdlhost
COPY --from=build /tmp/ext_a.so /usr/lib/postgresql/17/lib/ext_a.so
COPY --from=build /tmp/ext_b.so /usr/lib/postgresql/17/lib/ext_b.so
CMD ["/usr/bin/pgdlhost"]
`
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := dockerBuild(ctx, abs, pgExtFixture); err != nil {
		t.Skipf("cannot build the pgdlopen fixture (needs debian:trixie-slim locally): %v\n%s", err, out)
	}
}

// The ORACLE. It runs the same guest natively, so the expected magics are
// observed rather than asserted from the source -- and a guest that cannot even
// dlopen its own plugins on Linux would otherwise look like a raptormark bug.
func TestPostgresStyleDlopenNativeBaseline(t *testing.T) {
	// Gated like every other native baseline in this suite. Without it a plain
	// `go test ./...` attempts a docker build, which AGENTS.md forbids on the
	// default path -- and `builderImage()` would resolve to :latest, which is
	// not what any of this was built against.
	requireE2E(t)
	ctx := ctxFor(t)
	buildPgDlopenFixture(t, ctx, t.TempDir())
	out, err := dockerRunImage(ctx, pgExtFixture)
	if err != nil {
		t.Fatalf("native run: %v\n%s", err, out)
	}
	t.Logf("native:\n%s", out)
	if !strings.Contains(out, "PGDL-OK") {
		t.Fatalf("the guest fails NATIVELY, so it cannot witness anything:\n%s", out)
	}
	for _, want := range []string{"0xA1", "0xB2"} {
		if !strings.Contains(out, want) {
			t.Errorf("native run never reported magic %s:\n%s", want, out)
		}
	}
}

// TestPostgresStyleDlopenResolvesPerUnit is the end-to-end witness for the
// per-unit `dlsym`: two plugins fused as their OWN units, linked into one
// module with a dlopen map, and a guest that must tell them apart.
func TestPostgresStyleDlopenResolvesPerUnit(t *testing.T) {
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
	// DISCOVERED, not named. This is `image.Plugins`' only consumer, and it is
	// why the fixture puts the plugins in postgres's real `$libdir` rather than
	// somewhere convenient: `image.PluginDirs` recognises
	// `usr/lib/postgresql/*/lib`, so a test that hardcoded its own directory
	// would exercise the fuser and leave discovery unexercised.
	found, excluded, err := image.Plugins(root)
	if err != nil {
		t.Fatalf("plugin discovery: %v", err)
	}
	for _, e := range excluded {
		t.Logf("discovery excluded %s: %s", e.Guest, e.Reason)
	}
	var guestPaths, extras []string
	for _, p := range found {
		guestPaths = append(guestPaths, p.Guest)
		// ⚠️ NARROWED to this fixture's own extensions, deliberately.
		//
		// Discovery legitimately finds MORE than the test planted:
		// debian:trixie-slim ships OpenSSL engines and ossl-modules, and
		// `image.PluginDirs` recognises `usr/lib/*/engines-3` and
		// `usr/lib/*/ossl-modules` as well as postgres's `$libdir`. Measured on
		// this fixture: 5 plugins, of which 3 are the base image's.
		//
		// Fusing all five would make this a ~20-minute test whose extra work
		// says nothing about per-unit `dlsym`, which is its subject. So it
		// narrows -- but the assertion below still requires discovery to have
		// FOUND both extensions, so a discovery regression cannot hide here as
		// a test that quietly fuses nothing.
		if strings.Contains(p.Guest, "/postgresql/") {
			extras = append(extras, p.Host)
		}
	}
	t.Logf("discovered %d plugin(s): %v", len(found), guestPaths)
	t.Logf("fusing the %d under postgresql/: %v", len(extras), extras)
	if len(extras) != 2 {
		t.Fatalf("discovery found %d plugin(s) under postgresql/, want the 2 the "+
			"fixture installs. All discovered: %v\n"+
			"If this is 0, image.PluginDirs no longer recognises "+
			"usr/lib/postgresql/*/lib and the test would fuse nothing at all.",
			len(extras), guestPaths)
	}
	opts := fuse.Options{LibraryPaths: libs, Extra: extras}

	// A closure-wide layout, so each unit sits at a base every image agrees on.
	// Without one the plugins are packed per image and a unit's addresses are
	// nobody else's.
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
		t.Fatalf("expected 2 units, got %d -- Options.Extra named two plugins", len(units))
	}
	t.Logf("main %d bytes; units %s (%d B @ %#x), %s (%d B @ %#x)",
		len(mainImg), units[0].Name, len(units[0].Image), units[0].Base,
		units[1].Name, len(units[1].Image), units[1].Base)

	b, err := translate.BuilderFromImage(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	topts := translate.Options{Runtime: "ecvisor"}
	outDir := filepath.Join(dir, "out")

	images := []struct {
		file  string
		bytes []byte
		guest string // "" for the main program
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
		objs = append(objs, "/out/"+id+".o")
		guestOf[i] = im.guest

		frag := filepath.Join(dir, fmt.Sprintf("frag_%d.c", i))
		if err := os.WriteFile(frag, []byte(link.FragmentC(prog)), 0o644); err != nil {
			t.Fatal(err)
		}
		// Not translateOne: a UNIT image is a shape elflift has never been asked
		// to lift -- one plugin's sections, no program entry -- so if that is
		// what fails, the diagnosis belongs here and not in a shared helper.
		if _, err := b.RunCached(ctx, objectCache, translate.Request{
			ELF: p, OutDir: outDir, ModuleID: id,
			Fragment: frag, Keep: prog.Symbol(), Options: topts,
		}); err != nil {
			t.Fatalf("translating %s: %v\n\n"+
				"For a UNIT image the shape is new: FuseWithUnits emits one plugin's "+
				"sections alone. Note lifter/TraceManager.cpp only assigns the entry "+
				"function inside `i < func_symbols.size() - 1`, so an image whose entry "+
				"is the LAST func symbol reports \"entry_function is not found\".",
				im.file, err)
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
	// THE new table: each plugin's guest path names the unit that serves it,
	// and they name DIFFERENT units, which is the whole point.
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
	// The real rootfs, so `dlopen` resolves its path through the VFS exactly as
	// execve does. The `.so` files are present because that resolution happens
	// before the map is consulted; their contents are never read -- the lifted
	// units are in the module.
	// NOT named `image`: that is the package this test discovers plugins with,
	// and a local of the same name shadows it. It compiles today only because
	// `image.Plugins` is called before this declaration; moving either would
	// break it in a way the compiler explains badly.
	sidecarImg, _, err := rootfs.Build(root, rootfs.Options{
		ExecMap: execMap,
		DlMap:   dlMap,
		Boot:    &rootfs.Boot{Argv: []string{"/usr/bin/pgdlhost"}, Cwd: "/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Into outDir: runWasmIn mounts the module's OWN directory, so a sidecar
	// written elsewhere lands outside the mount, the runtime reports it "set but
	// unreadable", and every dlopen then fails for a reason that has nothing to
	// do with the code under test.
	sidecar := filepath.Join(outDir, "rootfs.img")
	if err := os.WriteFile(sidecar, sidecarImg, 0o644); err != nil {
		t.Fatal(err)
	}

	wasm := linkAll(t, ctx, dir, outDir, "pgdl.wasm", objs)
	// ⚠️ `--dir /:/out` IS REQUIRED, and its absence is not obvious: without a
	// WASI preopen the runtime cannot open the sidecar at all, reports
	// "RAPTORMARK_ROOTFS set but unreadable", and then runs with NO dlopen map
	// -- so every dlopen fails with "cannot open shared object file" and looks
	// exactly like the feature being broken. Measured: that is precisely how
	// this test first failed, after the comment beside the sidecar write warned
	// about the same class of mistake.
	//
	// The path is what the GUEST sees through the preopen: the sidecar is
	// /out/rootfs.img on the host and /rootfs.img here.
	//
	// `wasmEdgeEnv()` is NOT passed -- `runWasmIn` prepends it already.
	env := []string{"RAPTORMARK_ROOTFS=/" + filepath.Base(sidecar)}
	got := runWasmIn(t, ctx, wasm, nil, env, "/:/out")

	t.Logf("guest output:\n%s", got)
	if strings.Contains(got, "PGDL-FAIL") || !strings.Contains(got, "PGDL-OK") {
		t.Errorf("per-unit dlsym did not hold:\n%s\n\n"+
			"Read the FAIL lines. \"both handles resolved Pg_magic_func to one address\" "+
			"IS the original defect: a flat closure-wide .ecv.dlsyms with a "+
			"handle-ignoring dlsym, where the second extension silently runs the "+
			"first one's code.", got)
	}
	// Both magics must appear, or the guest exited early and PGDL-OK would be
	// the answer to a question it never asked.
	for _, want := range []string{"0xA1", "0xB2"} {
		if !strings.Contains(got, want) {
			t.Errorf("guest never reported magic %s; it did not reach both plugins:\n%s", want, got)
		}
	}
}
