package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"raptormark/internal/builder"
	"raptormark/internal/link"
	"raptormark/internal/oci"
	"raptormark/internal/translate"
)

// supervisorExportNames reads the names the supervisor module actually exports.
// Shared with the embedder harness test, which needs the same set to build its
// import object.
func supervisorExportNames(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	exports, err := oci.ModuleExports(b)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(exports))
	for _, e := range exports {
		names = append(names, e.Name)
	}
	return names, nil
}

// Placement: what the EMBEDDER supplies when it instantiates the module. These
// are not a supervisor concern -- deciding where a side module's memory and
// table slots go is what placing one means.
var placementSurface = []string{
	"memory", "__indirect_function_table",
	"__memory_base", "__table_base", "__stack_pointer",
}

// TestSideModulesAreBuiltAndCarryTheContract exercises the path that would
// otherwise rot.
//
// Both artifacts come out of ONE link over ONE set of objects: the flat module a
// stock shim runs, and the PIC side module an embedder instantiates. Nothing on
// the shipping path runs the second today, so without a test that BUILDS it,
// "we support both" degrades silently to "we support one and there is a flag".
//
// It asserts what can be checked without an embedder, which is the whole
// contract except its runtime behaviour: that the side module is genuinely a
// side module (`dylink.0` present, and it declares what it needs), that it
// exports the descriptor and the two initialisers, and that it imports exactly
// the supervisor surface plus placement -- nothing else, because anything else
// is a symbol nobody has agreed to supply.
func TestSideModulesAreBuiltAndCarryTheContract(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "sideprobe", sideProbeGuestSrc)
	b, err := translate.BuilderFromImage(ctx, img)
	if err != nil {
		t.Fatal(err)
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

	// Through translate.Link, NOT the open-coded docker command the other tests
	// use: the plumbing under test is `LinkRequest.SideOut`.
	sideDir := filepath.Join(dir, "side")
	flat := filepath.Join(outDir, "sideprobe.wasm")
	if err := b.Link(ctx, translate.LinkRequest{
		Registry: filepath.Join(dir, "registry.c"),
		Objects:  []string{filepath.Join(outDir, moduleID+".o")},
		Out:      flat,
		SideOut:  sideDir,
	}); err != nil {
		t.Fatalf("link with --side-out: %v", err)
	}

	side := filepath.Join(sideDir, moduleID+".side.wasm")
	sb, err := os.ReadFile(side)
	if err != nil {
		t.Fatalf("no side module was produced: %v", err)
	}
	fb, err := os.ReadFile(flat)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("flat %d bytes, side %d bytes", len(fb), len(sb))

	// --- it is genuinely a side module, and the flat one is genuinely flat ---
	needs, ok, err := oci.SideModuleNeeds(sb)
	if err != nil {
		t.Fatalf("reading dylink.0: %v", err)
	}
	if !ok {
		t.Fatal("the side module has no dylink.0: it was linked flat")
	}
	if needs.MemSize == 0 || needs.TableSize == 0 {
		t.Errorf("a side module carrying lifted code needs memory and table slots, got %+v", needs)
	}
	t.Logf("dylink.0: %d bytes of memory (align 2^%d), %d table entries",
		needs.MemSize, needs.MemAlignLog2, needs.TableSize)
	if _, ok, _ := oci.SideModuleNeeds(fb); ok {
		t.Error("the FLAT module has a dylink.0; a stock shim cannot place it")
	}

	// --- exports: the descriptor, and the two initialisers an embedder calls ---
	exports, err := oci.ModuleExports(sb)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, e := range exports {
		byName[e.Name] = e.Kind
	}
	// A GLOBAL, holding the descriptor's address. If this ever became a func an
	// embedder would look for something to call, so the kind is asserted.
	if k := byName["ecv_program_0"]; k != "global" {
		t.Errorf("ecv_program_0 must be an exported global, got %q", k)
	}
	for _, want := range []string{"__wasm_apply_data_relocs", "__wasm_call_ctors"} {
		if byName[want] != "func" {
			t.Errorf("%s must be an exported func; §8 has the embedder call it", want)
		}
	}

	// --- imports: exactly the contract, and nothing else ---
	//
	// ⚠️ The allow-list is READ OFF THE SUPERVISOR MODULE, not written down here.
	// A hand-maintained copy cannot catch a name being dropped from
	// `builder.SupervisorSurface` -- a mutation proved that on the unit-test
	// side, where the guard iterates the list under test and a shorter list is
	// just a shorter loop. Comparing two artifacts has no such blind spot: drop a
	// name and the side module still imports what the supervisor stopped
	// exporting, which is precisely an instantiation failure and precisely what
	// fails here.
	imports, err := oci.ModuleImports(sb)
	if err != nil {
		t.Fatal(err)
	}
	if len(imports) == 0 {
		t.Fatal("no imports: a side module that needs nothing is not lifted code")
	}
	supExports, err := supervisorExportNames(filepath.Join(sideDir, "supervisor.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	// The Go list is what the LINK roots; the module is what it produced. If they
	// disagree the export flag was dropped or the symbol no longer exists.
	for _, want := range builder.SupervisorSurface {
		if !slices.Contains(supExports, want) {
			t.Errorf("%s is in builder.SupervisorSurface but the supervisor module does not export it", want)
		}
	}
	allowed := append(slices.Clone(supExports), placementSurface...)
	for _, im := range imports {
		if im.Module != "env" {
			t.Errorf("%s: a side module imports from `env` only", im)
			continue
		}
		if !slices.Contains(allowed, im.Field) {
			t.Errorf("%s: the supervisor module does not export it and it is not a "+
				"placement value, so nobody has agreed to supply this. Either the supervisor must "+
				"export it (widening the boundary, which MULTIMODULE.md §6 argues against "+
				"for anything cheap) or the side link must resolve it locally, as it does "+
				"for __multi3 and fabs.", im)
		}
	}
	// The two that were resolved locally on purpose: importing `fabs` would make
	// a cross-module call out of one wasm instruction.
	for _, gone := range []string{"fabs", "__multi3"} {
		if slices.ContainsFunc(imports, func(i oci.Import) bool { return i.Field == gone }) {
			t.Errorf("%s is imported again; it should be linked into the side module", gone)
		}
	}

	// --- and the flat module is untouched by all of this ---
	flatImports, err := oci.ImportSet(fb)
	if err != nil {
		t.Fatal(err)
	}
	for _, im := range flatImports {
		if !strings.HasPrefix(im, "wasi_snapshot_preview1.") {
			t.Errorf("%s: asking for side modules changed the artifact a stock shim runs", im)
		}
	}
}

// Small on purpose: this test is about the LINK, and a bigger guest would only
// make it slower. It still carries lifted code, which is what gives the side
// module a non-empty dylink.0.
const sideProbeGuestSrc = `int main(void) { return 0; }
`
