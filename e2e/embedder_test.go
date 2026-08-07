package e2e

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"raptormark/internal/link"
	"raptormark/internal/oci"
	"raptormark/internal/translate"
)

// The development embedder. Kept as a real file rather than a Go string so it
// can be run by hand against `.agents-workspace/fixtures/embedder`, which is how
// it was written.
//
//go:embed testdata/embedder.mjs
var embedderHarness string

// TestEmbedderRunsTheSideModule is the answer to the question MULTIMODULE.md §5
// left open for three days: not "can the artifacts be BUILT" -- §8 and
// TestSideModulesAreBuiltAndCarryTheContract already say yes -- but "does the
// placement protocol WORK".
//
// One set of translated objects produces both artifacts. This test runs both:
// the flat module under wasmedge, and the supervisor + PIC side module through
// the nine-step sequence under node. Same guest, same objects, and the
// assertion is that they print the same thing.
//
// ⚠️ Node is a DEVELOPMENT host and this test does not make it anything else.
// It cannot supply WasmEdge's 11 socket imports (the harness stubs them to
// ENOTSUP), and node:wasi needs a private-symbol workaround to survive the
// guest growing memory at all. What the test establishes is that the PROTOCOL
// is right -- reserve, place, relocate, register, start -- which is the part no
// amount of inspection could settle.
func TestEmbedderRunsTheSideModule(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	// TWO programs, not one. A single placement cannot show that two do not
	// overlap, and overlap is the failure `ecv_reserve_side` exists to prevent --
	// silent, and arbitrarily far from where it was caused. Program 0 is the
	// entry and the only one that runs; program 1 is here to be placed.
	b, err := translate.BuilderFromImage(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	var progs []link.Program
	var objs []string
	var fragSrc string
	for i, g := range []struct{ name, src string }{
		{"embedguest", embedderGuestSrc},
		{"embedguest2", embedderGuest2Src},
	} {
		elf := compileGuest(t, ctx, dir, g.name, g.src)
		sha, err := translate.FileSHA256(elf)
		if err != nil {
			t.Fatal(err)
		}
		prog := link.Program{Name: translate.ModuleID(elf, sha), Index: i}
		progs = append(progs, prog)
		objs = append(objs, filepath.Join(outDir, prog.Name+".o"))

		frag := filepath.Join(dir, fmt.Sprintf("frag_%d.c", i))
		src := link.FragmentC(prog)
		if i == 0 {
			fragSrc = src
		}
		if err := os.WriteFile(frag, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		translateOne(t, ctx, b, translate.Request{
			ELF: elf, OutDir: outDir, ModuleID: prog.Name,
			Fragment: frag, Keep: prog.Symbol(), Options: translate.Options{Runtime: "ecvisor"},
		})
	}
	registry, err := link.RegistryC(progs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "registry.c"), []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}

	sideDir := filepath.Join(dir, "side")
	flat := filepath.Join(outDir, "embedguest.wasm")
	if err := b.Link(ctx, translate.LinkRequest{
		Registry: filepath.Join(dir, "registry.c"),
		Objects:  objs,
		Out:      flat,
		SideOut:  sideDir,
	}); err != nil {
		t.Fatalf("link with --side-out: %v", err)
	}

	// --- the flat module, the way a stock shim runs it ---
	flatOut := runWasm(t, ctx, flat)
	if !strings.Contains(flatOut, embedderGuestMarker) {
		t.Fatalf("the FLAT module did not run the guest; without that the "+
			"multi-module comparison below has no baseline:\n%s", flatOut)
	}

	// --- the same objects, placed by an embedder ---
	if err := os.WriteFile(filepath.Join(dir, "embedder.mjs"), []byte(embedderHarness), 0o644); err != nil {
		t.Fatal(err)
	}
	var sides []string
	for _, p := range progs {
		sides = append(sides, "/w/side/"+p.Name+".side.wasm")
	}
	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"},
		fmt.Sprintf("node /w/embedder.mjs --supervisor /w/side/supervisor.wasm --program-size %d --dir /w %s",
			ecvProgramSize(t, fragSrc), strings.Join(sides, " ")))
	t.Logf("harness output:\n%s", out)
	if err != nil {
		t.Fatalf("the embedder harness failed: %v", err)
	}

	// Every step, in order. A harness that fell out early and still printed the
	// guest's output would otherwise pass -- and the interesting failures
	// (reservation, relocation, registration) are all before step 9.
	for i := 1; i <= 9; i++ {
		if !strings.Contains(out, fmt.Sprintf("step%d", i)) {
			t.Errorf("step %d of the MULTIMODULE.md §8 sequence did not run", i)
		}
	}
	if !strings.Contains(out, embedderGuestMarker) {
		t.Errorf("the guest did not run under the embedder, though it ran flat")
	}
	if !strings.Contains(out, "EMBEDDER-SEQUENCE-COMPLETE exit=0") {
		t.Errorf("the sequence did not complete cleanly")
	}

	// --- the two dylink.0 parsers must agree ---
	//
	// The harness parses `dylink.0` in JS so it can be run by hand; `internal/oci`
	// parses it in Go. Either could be wrong in a way that still produces plausible
	// numbers -- a mis-read alignment is a power of two either way -- and a single
	// parser has nothing to be wrong against.
	type placement struct{ base, end int64 }
	var placed []placement
	for i, p := range progs {
		sb, err := os.ReadFile(filepath.Join(sideDir, p.Name+".side.wasm"))
		if err != nil {
			t.Fatal(err)
		}
		needs, ok, err := oci.SideModuleNeeds(sb)
		if err != nil || !ok {
			t.Fatalf("reading dylink.0 in Go for program %d: ok=%v err=%v", i, ok, err)
		}
		want := fmt.Sprintf("needs mem=%d align=2^%d table=%d", needs.MemSize, needs.MemAlignLog2, needs.TableSize)
		if !strings.Contains(out, want) {
			t.Errorf("program %d: the two dylink.0 parsers disagree; Go read %q "+
				"and the harness printed no such line", i, want)
		}

		// --- the reservation landed aligned, and holds the descriptor ---
		//
		// This is the rule with no loud failure mode. A side module placed over
		// the supervisor's heap runs perfectly until dlmalloc hands the same
		// bytes out again, which can be any amount of time later and looks like
		// data corruption rather than like a placement bug.
		base := stepValue(t, out, fmt.Sprintf(`step3\[%d\]: reserved at (\d+)`, i))
		if align := int64(1) << needs.MemAlignLog2; base%align != 0 {
			t.Errorf("program %d was placed at %d, which is not %d-aligned", i, base, align)
		}
		descAddr := stepValue(t, out, fmt.Sprintf(`step7\[%d\]: ecv_program_%d at offset \d+ -> (\d+)`, i, i))
		if descAddr < base || descAddr >= base+int64(needs.MemSize) {
			t.Errorf("program %d: the descriptor at %d is outside its own region [%d, %d); "+
				"the supervisor was handed a pointer into somebody else's memory",
				i, descAddr, base, base+int64(needs.MemSize))
		}
		placed = append(placed, placement{base, base + int64(needs.MemSize)})
	}

	// --- and the two regions are disjoint ---
	//
	// The whole reason placement goes through the supervisor rather than through
	// `memory.grow` or `__heap_base`. Two side modules sharing a byte is not a
	// crash; it is one program's data under another's, discovered later and
	// somewhere else.
	if len(placed) == 2 {
		a, b := placed[0], placed[1]
		if a.base < b.end && b.base < a.end {
			t.Errorf("the two side modules overlap: [%d, %d) and [%d, %d)", a.base, a.end, b.base, b.end)
		}
		t.Logf("placements [%d, %d) and [%d, %d), gap %d bytes",
			a.base, a.end, b.base, b.end, max(a.base, b.base)-min(a.end, b.end))
	}

	// Both must have been registered, and the witness is the SUPERVISOR's own
	// line rather than the harness's.
	//
	// ⚠️ This asserted `step8[i]: registered` first, and a mutation that skipped
	// the call for every program after the first SURVIVED it -- the harness
	// prints that line whether or not the call did anything. `[ecvisor]
	// registered program N` comes from `abi::dyn_register` and cannot be printed
	// by a registration that did not happen.
	for i := range progs {
		if !strings.Contains(out, fmt.Sprintf("[ecvisor] registered program %d", i)) {
			t.Errorf("the supervisor never reported registering program %d; "+
				"it would run program 0 regardless, so nothing else here would notice", i)
		}
	}
}

func stepValue(t *testing.T, out, pattern string) int64 {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no line matching %q in the harness output", pattern)
	}
	v, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// ecvProgramSize is sizeof(EcvProgram) as the C SIDE spells it, counted off the
// struct `internal/link` emits. Every field is a pointer, and wasm32 pointers
// are 4 bytes.
//
// The harness has to pass this to `ecv_register_program`, which compares it with
// the Rust side's `size_of::<EcvProgram>()`. That comparison is the ABI tripwire,
// and it only means something if the number comes from somewhere other than the
// runtime -- which is why the harness refuses to default it, and why this counts
// the C declaration instead of writing 72 down.
func ecvProgramSize(t *testing.T, fragmentC string) int {
	t.Helper()
	const open = "typedef struct {"
	i := strings.Index(fragmentC, open)
	if i < 0 {
		t.Fatal("internal/link no longer emits `typedef struct {`; this can no longer count the fields")
	}
	j := strings.Index(fragmentC[i:], "} EcvProgram;")
	if j < 0 {
		t.Fatal("the EcvProgram typedef is not closed where expected")
	}
	body := fragmentC[i+len(open) : i+j]
	n := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), ";") {
			n++
		}
	}
	// A struct that lost its fields would otherwise yield 0 and the ABI check
	// would fire on a number this function invented.
	if n < 10 {
		t.Fatalf("counted only %d fields in EcvProgram; that is not the struct", n)
	}
	return n * 4
}

const embedderGuestMarker = "EMBEDDER-GUEST-OK"

// Prints, rather than just returning 0: the point is that lifted code RAN, and
// an exit status of zero is also what a supervisor that quietly did nothing
// would produce.
const embedderGuestSrc = `#include <stdio.h>
int main(void) { printf("` + embedderGuestMarker + `\n"); return 0; }
`

// Program 1 exists to be PLACED, not run: two placements are what make overlap
// observable. Its source differs from program 0's so that it lifts to a
// different object, and therefore to a side module with its own dylink.0.
const embedderGuest2Src = `#include <stdio.h>
int main(void) { printf("EMBEDDER-GUEST-2 (never reached in this test)\n"); return 0; }
`
