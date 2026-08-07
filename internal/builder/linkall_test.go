package builder

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// As in translateone_test.go, these pin the command lines builder/link-all.sh
// produced — except where suspend-by-early-return deliberately diverged from it:
// the SjLj shim and -lsetjmp are gone from the link, and --translate-to-exnref is
// gone from wasm-opt. See TestWasmOptEnablesNoProposal for why that matters.

func TestLinkArgsMatchTheScript(t *testing.T) {
	want := []string{
		"-O3", "--sysroot=/wasi/share/wasi-sysroot",
		"-Wl,--allow-undefined", "-Wl,-z,stack-size=16777216",
		"-o", "/out/app.wasm.pre",
		"/objs/p0.o", "/objs/p1.o",
		"/reg/registry.o",
		"/opt/ecvisor/ecv_sp.o", "/opt/ecvisor/ecv_globals.o", "/opt/ecvisor/libecvisor.a",
	}
	got := linkArgs("--sysroot=/wasi/share/wasi-sysroot",
		[]string{"/objs/p0.o", "/objs/p1.o"}, "/reg/registry.o", "/out/app.wasm.pre",
		ecvisorArchive("wasmedge"))
	if !slices.Equal(got, want) {
		t.Errorf("linkArgs:\n got %q\nwant %q", got, want)
	}
}

// Every object must precede libecvisor.a: wasm-ld resolves an archive against
// the undefined symbols it has already seen. That covers the two C shims as well
// as the lifted objects — libecvisor.a calls into ecv_globals.o for the
// `__ecv_unwinding` accessors, so an archive placed first would leave them
// unresolved and `--allow-undefined` would turn them into `env` imports that no
// host provides.
func TestLinkArgsPutObjectsBeforeTheArchive(t *testing.T) {
	got := linkArgs("--sysroot=/s", []string{"/objs/p0.o"}, "/reg/registry.o", "/out/o.wasm",
		ecvisorArchive("wasmedge"))
	ar := slices.Index(got, "/opt/ecvisor/libecvisor.a")
	if ar < 0 {
		t.Fatalf("libecvisor.a missing from the link, got %q", got)
	}
	for _, obj := range []string{
		"/objs/p0.o", "/reg/registry.o",
		"/opt/ecvisor/ecv_sp.o", "/opt/ecvisor/ecv_globals.o",
	} {
		switch i := slices.Index(got, obj); {
		case i < 0:
			t.Errorf("%s missing from the link, got %q", obj, got)
		case i > ar:
			t.Errorf("%s must come before libecvisor.a, got %q", obj, got)
		}
	}
}

func TestWasmOptDefaults(t *testing.T) {
	t.Setenv("ECV_WASM_OPT_LEVEL", "")
	t.Setenv("ECV_WASM_NAMES", "")
	c := &LinkAll{Out: "/out/app.wasm"}
	want := []string{"-g", "-O0", "/out/app.wasm.pre", "-o", "/out/app.wasm"}
	if got := c.wasmOptArgs("/out/app.wasm.pre"); !slices.Equal(got, want) {
		t.Errorf("wasmOptArgs:\n got %q\nwant %q", got, want)
	}
}

// The emitted module must need no wasm proposal beyond Wasm 2.0. Measured: a
// module carrying exnref EH is rejected by every released runwasi shim, and
// toggling that one proposal is the difference between WasmEdge 0.17.1 running
// a raptormark module and failing it with "malformed section id, Code: 0x105".
// An --enable-* here would put the proposal back in the module.
func TestWasmOptEnablesNoProposal(t *testing.T) {
	c := &LinkAll{Out: "/out/app.wasm"}
	for _, a := range c.wasmOptArgs("/pre") {
		if strings.HasPrefix(a, "--enable-") || a == "--translate-to-exnref" {
			t.Errorf("wasm-opt must not re-enable a proposal, got %q", a)
		}
	}
}

// ECV_WASM_NAMES=0 drops -g, which drops the wasm name section — the difference
// between a readable trap and a list of bare function indices.
func TestWasmOptNamesCanBeStripped(t *testing.T) {
	t.Setenv("ECV_WASM_NAMES", "0")
	c := &LinkAll{Out: "/out/app.wasm"}
	if slices.Contains(c.wasmOptArgs("/pre"), "-g") {
		t.Error("ECV_WASM_NAMES=0 should drop -g")
	}
}

// ECV_WASM_OPT=0 skips the finaliser entirely: the module wasm-ld emitted
// becomes the output unchanged, and the pre-module does not survive.
//
// This is a behaviour test rather than an argument-list one, because the thing
// that can break is the file handling — Run used to remove `pre` itself, so a
// finalise that renames it has to own that removal too, and a leftover or
// missing file is exactly what an args check cannot see.
func TestWasmOptCanBeSkipped(t *testing.T) {
	t.Setenv("ECV_WASM_OPT", "0")
	dir := t.TempDir()
	c := &LinkAll{Out: filepath.Join(dir, "app.wasm")}
	pre := c.Out + ".pre"
	want := []byte("\x00asm\x01\x00\x00\x00 pretend this is 120 MB")
	if err := os.WriteFile(pre, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.finalise(pre); err != nil {
		t.Fatalf("finalise with ECV_WASM_OPT=0: %v", err)
	}
	got, err := os.ReadFile(c.Out)
	if err != nil {
		t.Fatalf("output missing: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("skipped finalise altered the module:\n got %q\nwant %q", got, want)
	}
	if _, err := os.Stat(pre); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the pre-module must not survive finalise, got err=%v", err)
	}
}

// The control for the test above: with the default settings finalise must
// actually try to run wasm-opt. Without this, a finalise that skipped
// unconditionally would pass TestWasmOptCanBeSkipped and silently drop 5.5% of
// the size win on every build.
//
// There is no wasm-opt on the unit-test host — that IS the observation. The
// error names the binary it failed to exec, which is the evidence that the
// default path is the wasm-opt path.
func TestWasmOptRunsByDefault(t *testing.T) {
	t.Setenv("ECV_WASM_OPT", "")
	dir := t.TempDir()
	c := &LinkAll{Out: filepath.Join(dir, "app.wasm")}
	pre := c.Out + ".pre"
	if err := os.WriteFile(pre, []byte("\x00asm\x01\x00\x00\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := c.finalise(pre)
	if err == nil {
		t.Fatal("finalise must invoke wasm-opt by default; it succeeded without one")
	}
	if !strings.Contains(err.Error(), "wasm-opt") {
		t.Errorf("expected the failure to name wasm-opt, got %v", err)
	}
	if _, err := os.Stat(c.Out); err == nil {
		t.Error("a failed finalise must not leave an output module")
	}
}

func TestWasmOptLevelOverride(t *testing.T) {
	t.Setenv("ECV_WASM_OPT_LEVEL", "-O2")
	c := &LinkAll{Out: "/out/app.wasm"}
	if !slices.Contains(c.wasmOptArgs("/pre"), "-O2") {
		t.Error("ECV_WASM_OPT_LEVEL should reach wasm-opt")
	}
}

// --objs is one space-separated string because that is what
// internal/translate.Link builds; the scripts relied on the shell to split it.
func TestObjsIsSpaceSeparated(t *testing.T) {
	if got := strings.Fields(" a.o  b.o\tc.o "); !slices.Equal(got, []string{"a.o", "b.o", "c.o"}) {
		t.Errorf("unexpected split: %q", got)
	}
	c := &LinkAll{Registry: "/reg/registry.c", Out: "/out/o.wasm", Objs: "   "}
	if _, ok := c.Run().(*UsageError); !ok {
		t.Error("an all-whitespace --objs should be a usage error")
	}
}

// ⚠️ `wasm-ld --experimental-pic -shared --export-all` CRASHES on a real lifted
// object (wasi-sdk 24). The side link must root the program descriptor
// explicitly instead, which is the export we want anyway -- so this guards a
// workaround that is also the correct design, and would otherwise look like an
// arbitrary choice to a later reader who "simplified" it.
func TestSideLinkNeverExportsAll(t *testing.T) {
	args := sideLinkArgs("--sysroot=/x", "/out/prog_ab.o", "/side/prog_ab.side.wasm", 3, "/rt.a", "/libc.a")
	for _, a := range args {
		if strings.Contains(a, "--export-all") {
			t.Errorf("--export-all crashes wasm-ld under -shared; got %q", args)
		}
	}
	if !slices.Contains(args, "-Wl,--export=ecv_program_3") {
		t.Errorf("the descriptor must be rooted by index; got %q", args)
	}
}

// A side module is PIC and shared, or it is not a side module. Without both
// flags the link silently produces an ordinary module, which would be
// indistinguishable from the flat one until an embedder tried to place it.
func TestSideLinkIsPICAndShared(t *testing.T) {
	args := sideLinkArgs("--sysroot=/x", "/out/p.o", "/side/p.wasm", 0, "/rt.a", "/libc.a")
	for _, want := range []string{"-fPIC", "-Wl,--experimental-pic", "-Wl,-shared"} {
		if !slices.Contains(args, want) {
			t.Errorf("%s missing from the side link: %q", want, args)
		}
	}
}

// The flat link must not acquire side-module flags: they would put
// `__memory_base` into the artifact a stock shim runs, and no shim supplies it.
func TestFlatLinkStaysFlat(t *testing.T) {
	args := linkArgs("--sysroot=/x", []string{"/out/p.o"}, "/out/registry.o", "/out/app.wasm",
		ecvisorArchive("wasmedge"))
	for _, forbidden := range []string{"-Wl,-shared", "-Wl,--experimental-pic"} {
		if slices.Contains(args, forbidden) {
			t.Errorf("%s leaked into the flat link: %q", forbidden, args)
		}
	}
}

// A side module must resolve `__multi3` and `fabs` LOCALLY. Widening the
// supervisor's export list for them would make a cross-module call out of
// `f64.abs` -- one wasm opcode -- which is the inlining loss MULTIMODULE.md §6
// identifies, at the worst granularity available. Measured cost of linking them
// in instead: +5,371 bytes per side module.
func TestSideLinkResolvesCompilerRtLocally(t *testing.T) {
	args := sideLinkArgs("--sysroot=/sr", "/o/p.o", "/side/p.wasm", 0,
		"/sdk/lib/clang/18/lib/wasi/libclang_rt.builtins-wasm32.a",
		"/sdk/share/wasi-sysroot/lib/wasm32-wasip1/libc.a")
	for _, want := range []string{
		"/sdk/lib/clang/18/lib/wasi/libclang_rt.builtins-wasm32.a",
		"/sdk/share/wasi-sysroot/lib/wasm32-wasip1/libc.a",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("%s missing from the side link: %q", want, args)
		}
	}
	// ⚠️ `-lc` resolves to a libc variant with NO fabs, and the link then
	// succeeds while still importing it -- a silent miss. The archive is named
	// by path for that reason, so this guards against "simplifying" it back.
	if slices.Contains(args, "-lc") {
		t.Errorf("-lc picks a libc without fabs; name the wasip1 archive: %q", args)
	}
	// The archives must come after the object: wasm-ld resolves an archive
	// against undefined symbols it has already seen.
	iObj, iLibc := slices.Index(args, "/o/p.o"), slices.Index(args, "/sdk/share/wasi-sysroot/lib/wasm32-wasip1/libc.a")
	if iObj < 0 || iLibc < iObj {
		t.Errorf("archives must follow the object: %q", args)
	}
}

// The supervisor must export every name a side module imports, or the pair
// cannot be instantiated.
//
// ⚠️ WHAT THIS CANNOT CATCH, stated because a mutation proved it. Deleting a
// name from `SupervisorSurface` leaves this test GREEN: it iterates the list
// under test, so a shorter list is simply a shorter loop. It therefore guards
// the export LOOP -- four mutations to `supervisorLinkArgs` were caught -- and
// not the CONTENTS of the contract.
//
// Nothing on this host can guard the contents: the contract is what a real side
// module imports, and that is an artifact, not a literal. The load-bearing check
// is `TestSideModulesAreBuiltAndCarryTheContract` in e2e/, which reads the
// supervisor's actual exports and the side module's actual imports and asserts
// containment. Delete a name here and THAT test fails, because the side module
// still imports what the supervisor stopped exporting.
func TestSupervisorExportsTheWholeSurface(t *testing.T) {
	args := supervisorLinkArgs("--sysroot=/x", "/side/supervisor.wasm")
	for _, sym := range SupervisorSurface {
		if !slices.Contains(args, "-Wl,--export="+sym) {
			t.Errorf("%s is in the contract but the supervisor does not export it: %q", sym, args)
		}
	}
	// Placement is the embedder's to supply; the supervisor exporting one would
	// mean every side module lands at the same address.
	for _, sym := range []string{"__memory_base", "__table_base"} {
		if slices.Contains(args, "-Wl,--export="+sym) {
			t.Errorf("%s is a PLACEMENT value, not a supervisor export: %q", sym, args)
		}
	}
}

// Without --growable-table wasm-ld caps the exported table at its initial size,
// `Table.prototype.grow` throws, and the embedder can place exactly zero side
// modules -- a failure that appears only once an embedder exists, which is why
// it is pinned here.
func TestSupervisorTableIsExportedAndGrowable(t *testing.T) {
	args := supervisorLinkArgs("--sysroot=/x", "/side/supervisor.wasm")
	for _, want := range []string{"-Wl,--export-table", "-Wl,--growable-table", "-Wl,--export=__stack_pointer"} {
		if !slices.Contains(args, want) {
			t.Errorf("%s missing from the supervisor link: %q", want, args)
		}
	}
	for _, want := range SupervisorControl {
		if !slices.Contains(args, "-Wl,--export="+want) {
			t.Errorf("the embedder calls %s; it must be exported: %q", want, args)
		}
	}
}

// ⚠️ No registry.o, and that is the shape rather than an omission: with the
// registry absent `ecv_program_count` resolves to zero under --allow-undefined,
// which is what makes the supervisor ask to be TOLD about programs. Linking one
// in would freeze a static program list into a module whose whole purpose is a
// dynamic one.
func TestSupervisorLinksNoRegistryAndNoPrograms(t *testing.T) {
	args := supervisorLinkArgs("--sysroot=/x", "/side/supervisor.wasm")
	for _, a := range args {
		if strings.HasSuffix(a, "registry.o") || strings.HasSuffix(a, ".side.wasm") {
			t.Errorf("the supervisor links neither a registry nor a program: %q", args)
		}
	}
	if !slices.Contains(args, "-Wl,--allow-undefined") {
		t.Errorf("--allow-undefined is what resolves ecv_program_count to zero: %q", args)
	}
	// The two C shims carry `__ecv_unwinding` and the shadow-stack probe, and
	// libecvisor.a references them -- so they precede the archive here for the
	// same reason the lifted objects do in the flat link.
	iShim := slices.Index(args, "/opt/ecvisor/ecv_globals.o")
	iLib := slices.Index(args, "/opt/ecvisor/libecvisor.a")
	if iShim < 0 || iLib < 0 || iLib < iShim {
		t.Errorf("ecv_globals.o must precede libecvisor.a: %q", args)
	}
}

// TestProfileSelectsTheArchive pins the one thing the loopback profile IS.
//
// ⚠️ The default must keep resolving to the bare `/opt/ecvisor/libecvisor.a`.
// Every existing artifact linked against that path, and
// `TestLinkArgsMatchTheScript` asserts the default argv verbatim -- so a change
// here that looked harmless would silently relink every shipping module against
// a different runtime.
//
// The two paths must also DIFFER. A mapping that returned the same archive for
// both profiles would leave every other check in place and still be testing one
// backend twice, which is exactly the shape of a guarantee that isn't one.
func TestProfileSelectsTheArchive(t *testing.T) {
	const shipping = "/opt/ecvisor/libecvisor.a"
	if got := ecvisorArchive("wasmedge"); got != shipping {
		t.Errorf("the default profile must link %s, got %s", shipping, got)
	}
	if got := ecvisorArchive(""); got != shipping {
		t.Errorf("an unset profile must behave as the default, got %s", got)
	}
	loop := ecvisorArchive("loopback")
	if loop == shipping {
		t.Fatalf("loopback resolves to the shipping archive (%s), so linking it "+
			"would prove nothing about the backend seam", loop)
	}
	if !strings.Contains(loop, "loopback") {
		t.Errorf("the loopback archive path should name the profile, got %s", loop)
	}
}

// TestLinkArgsHonourTheProfile checks the archive actually reaches the argv, in
// the LAST position -- ordering is load-bearing (see
// TestLinkArgsPutObjectsBeforeTheArchive), and a profile that placed its archive
// anywhere else would link but resolve nothing.
func TestLinkArgsHonourTheProfile(t *testing.T) {
	args := linkArgs("--sysroot=/s", []string{"/o/p.o"}, "/o/registry.o", "/o/app.wasm",
		ecvisorArchive("loopback"))
	last := args[len(args)-1]
	if last != ecvisorArchive("loopback") {
		t.Errorf("the profile's archive must be the last argument, got %q", last)
	}
	for _, a := range args {
		if a == ecvisorArchive("wasmedge") {
			t.Errorf("the loopback link still names the wasmedge archive: %q", a)
		}
	}
}

// TestReentrantSurfaceIsExportedOnlyOffTheDefaultProfile pins both halves.
//
// ⚠️ An export here is not cosmetic: `--export=` also implies `--undefined=`,
// which is what PULLS the symbol's archive member into the link. Without it
// these functions are not merely un-exported, they are absent -- wasm-ld never
// pulls an unreferenced member out of a static archive. So dropping a flag does
// not produce a module missing an export; it produces a module missing the code.
func TestReentrantSurfaceIsExportedOnlyOffTheDefaultProfile(t *testing.T) {
	flags := exportFlags(ReentrantSurface)
	if len(flags) != len(ReentrantSurface) || len(flags) == 0 {
		t.Fatalf("expected one flag per symbol, got %q", flags)
	}
	for i, sym := range ReentrantSurface {
		if want := "-Wl,--export=" + sym; flags[i] != want {
			t.Errorf("flag %d: got %q, want %q", i, flags[i], want)
		}
	}
	// The shipping profile must be untouched: its argv is asserted verbatim by
	// TestLinkArgsMatchTheScript, and every existing artifact came from it.
	base := linkArgs("--sysroot=/s", []string{"/o/p.o"}, "/o/registry.o", "/o/a.wasm",
		ecvisorArchive("wasmedge"))
	for _, a := range base {
		if strings.HasPrefix(a, "-Wl,--export=") {
			t.Errorf("the default profile gained an export flag (%q); nothing drives "+
				"the re-entrant surface there, because a blocking backend never goes idle", a)
		}
	}
	if len(exportFlags(nil)) != 0 {
		t.Error("no symbols must render no flags, not an empty flag")
	}
}
