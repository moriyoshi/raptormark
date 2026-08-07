package e2e

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"raptormark/internal/builder"
	"raptormark/internal/oci"
)

// The re-entrant surface: boot once, then ask for slices, instead of calling
// `_start` and never getting control back.
//
// ⚠️ AN EXPORT HERE IS NOT COSMETIC. `--export=` also implies `--undefined=`,
// which is what pulls the symbol's member out of libecvisor.a. Nothing inside
// the module references these functions, and wasm-ld never pulls an
// unreferenced archive member -- so a missing flag does not yield a module with
// a hidden function, it yields a module without the code. That mechanism is why
// a neutralization silently failed to fire during the loopback work, and it is
// the reason this test reads the EXPORT SECTION of a real module rather than
// trusting the link command.

func exportsOf(t *testing.T, wasm string) []string {
	t.Helper()
	b, err := os.ReadFile(wasm)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := oci.ModuleExports(b)
	if err != nil {
		t.Fatalf("reading exports: %v", err)
	}
	if len(exp) == 0 {
		// A module always exports at least `memory`; an empty list means the
		// parser found nothing, and "the surface is absent" would then be
		// trivially true.
		t.Fatal("no exports found; every module exports at least `memory`")
	}
	names := make([]string, 0, len(exp))
	for _, e := range exp {
		names = append(names, e.Name)
	}
	slices.Sort(names)
	return names
}

// TestReentrantSurfaceIsPresentOnLoopbackAndAbsentByDefault is a differential,
// because either half alone can pass for the wrong reason: a build that
// exported everything would satisfy "present", and a build that linked nothing
// would satisfy "absent".
func TestReentrantSurfaceIsPresentOnLoopbackAndAbsentByDefault(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)

	dirL := t.TempDir()
	elfL := compileGuest(t, ctx, dirL, "reentl", importProbeGuestSrc)
	loop := exportsOf(t, liftOne(t, ctx, img, dirL, elfL, "reentl", "--profile", "loopback"))

	dirW := t.TempDir()
	elfW := compileGuest(t, ctx, dirW, "reentw", importProbeGuestSrc)
	edge := exportsOf(t, liftOne(t, ctx, img, dirW, elfW, "reentw"))

	t.Logf("loopback exports: %s", strings.Join(loop, " "))
	t.Logf("wasmedge exports: %s", strings.Join(edge, " "))

	for _, sym := range builder.ReentrantSurface {
		if !slices.Contains(loop, sym) {
			t.Errorf("the loopback module does not export %q. Since `--export=` is also "+
				"what links the symbol in, the function is absent from the module, not "+
				"merely hidden.", sym)
		}
		if slices.Contains(edge, sym) {
			t.Errorf("the default profile exports %q. Nothing drives the re-entrant "+
				"surface there -- a blocking backend never goes idle -- so this is "+
				"artifact churn on the shipping path.", sym)
		}
	}

	// The command entry must survive on BOTH. Losing it would make the module
	// un-runnable by every existing host while every assertion above still passed.
	for _, prof := range []struct {
		name string
		exp  []string
	}{{"loopback", loop}, {"wasmedge", edge}} {
		if !slices.Contains(prof.exp, "_start") {
			t.Errorf("%s: lost `_start`; no existing host can run this module", prof.name)
		}
		if !slices.Contains(prof.exp, "memory") {
			t.Errorf("%s: lost `memory`; no host can read or write the guest", prof.name)
		}
	}
}

// sleepGuestSrc sleeps in the middle, so the scheduler must go idle.
//
// The sleep is what makes this a test of re-entrancy rather than of a fast
// path: without it the guest never blocks, the run queue never drains, and
// `ecv_run_slice` would return EXITED on its first call having proved nothing
// about idling.
const sleepGuestSrc = `#include <stdio.h>
#include <time.h>
int main(void) {
	printf("BEFORE\n");
	fflush(stdout);
	struct timespec ts = { 0, 400 * 1000 * 1000 }; /* 400ms */
	nanosleep(&ts, NULL);
	printf("AFTER\n");
	return 0;
}
`

// TestReentrantDriverIdlesInsteadOfBlocking is the point of the whole
// re-entrancy change.
//
// ⚠️ It needs BOTH halves, and either alone proves nothing. A module with the
// exports but a blocking backend simply sleeps inside a slice and reports
// EXITED once, looking identical to `_start`. A non-blocking backend without the
// exports cannot be driven at all. So the assertion is not "it ran" but "it
// went IDLE with a future deadline, and the host -- not the guest -- did the
// waiting".
//
// `RAPTORMARK_ECV_NONBLOCK=1` is what makes the in-process backend decline to
// wait, which is the browser backend's permanent behaviour.
func TestReentrantDriverIdlesInsteadOfBlocking(t *testing.T) {
	img := requireE2E(t)
	node := requireNode(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "sleepy", sleepGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "sleepy", "--profile", "loopback")

	out := runNodeHostArgs(t, ctx, node, wasm, "",
		"--reentrant", "--env", "RAPTORMARK_ECV_NONBLOCK=1")

	for _, want := range []string{"BEFORE", "AFTER", "HOST-EXIT: 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q; full output:\n%s", want, out)
		}
	}
	// The guest must have gone idle at least once -- that IS the re-entrancy.
	idle := reSliceIdle.FindStringSubmatch(out)
	if idle == nil {
		t.Fatalf("no HOST-SLICES line; the driver did not run slices:\n%s", out)
	}
	if idle[1] == "0" {
		t.Errorf("the guest never went idle, so the host never got control back "+
			"during the sleep -- the scheduler blocked inside a slice instead. "+
			"Full output:\n%s", out)
	}
	t.Logf("idle waits: %s", idle[1])
}

var reSliceIdle = regexp.MustCompile(`HOST-SLICES: \d+ idle=(\d+)`)

// TestReentrantAndStartAgreeOnTheSameModule is the differential: the two ways
// of driving one module must produce the same guest behaviour.
func TestReentrantAndStartAgreeOnTheSameModule(t *testing.T) {
	img := requireE2E(t)
	node := requireNode(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "bothways", sleepGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "bothways", "--profile", "loopback")

	viaStart := runNodeHost(t, ctx, node, wasm, "")
	viaSlices := runNodeHostArgs(t, ctx, node, wasm, "",
		"--reentrant", "--env", "RAPTORMARK_ECV_NONBLOCK=1")

	norm := func(s string) string {
		var keep []string
		for _, l := range strings.Split(s, "\n") {
			if l == "BEFORE" || l == "AFTER" {
				keep = append(keep, l)
			}
		}
		return strings.Join(keep, "\n")
	}
	if norm(viaStart) != norm(viaSlices) {
		t.Errorf("the two drivers disagree.\n_start:\n%s\nslices:\n%s", viaStart, viaSlices)
	}
}
