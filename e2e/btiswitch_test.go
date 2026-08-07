package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// btiSwitchGuestSrc is a computed-goto interpreter, the exact shape
// `patches/0013`'s BTI landing-pad scan exists to serve: the dispatch labels
// are reached only through an indirect `br`, so direct-branch discovery never
// decodes them. Compiled with `-mbranch-protection=standard` every one of them
// carries a `bti j` pad, which is what the scan seeds.
//
// This is the fixture the rest of the suite does not have. Measured over the
// saved fixtures, `bti j` counts are: busybox-musl 0, bash-glibc 0,
// aptget-glibc 0, postgres-glibc 21,470. Every cheap fixture is
// non-branch-protected, so on all of them the seeding block seeds nothing and
// any change to it -- including deleting it -- leaves their output identical.
//
// The entry of `interp` is itself a `bti c` under branch protection, which
// matters: `patches/0025`'s orphan-block fallback is gated on the entry NOT
// being a landing pad, so it is switched off here. Nothing but the BTI scan can
// seed these labels, which is what makes this a guard for the scan alone.
//
// If seeding fails, the guest does not merely print the wrong number. The
// indirect `br` misses the block-address map, falls through to the function's
// far-jump catch-all and re-dispatches the same pc, nesting a wasm frame each
// time until the shadow stack dies.
const btiSwitchGuestSrc = `#include <stdio.h>

long interp(const unsigned char *prog, long n, long x) {
	static const void *tab[] = {&&L0, &&L1, &&L2, &&L3, &&L4, &&L5, &&L6, &&L7};
	long pc = 0, acc = x;
	while (pc < n) {
		goto *tab[prog[pc] & 7];
	L0: acc += 3;             pc++; continue;
	L1: acc -= 1;             pc++; continue;
	L2: acc *= 2;             pc++; continue;
	L3: acc ^= 0x5a;          pc++; continue;
	L4: acc |= 1;             pc++; continue;
	L5: acc &= 0xffff;        pc++; continue;
	L6: acc = acc << 1 | 1;   pc++; continue;
	L7: acc = acc >> 1;       pc++; continue;
	}
	return acc;
}

int main(void) {
	unsigned char p[64];
	for (int i = 0; i < 64; i++) p[i] = (unsigned char)(i * 5 + 1);
	printf("acc=%ld\n", interp(p, 64, 7));
	return 0;
}
`

// btiSwitchExpected re-implements the guest independently, so agreement between
// the module and the native binary is not the only evidence. Two runs of the
// same C can agree while both are wrong about what the program means; this is a
// third opinion that shares no code with either.
func btiSwitchExpected() int64 {
	acc := int64(7)
	for pc := 0; pc < 64; pc++ {
		switch byte(pc*5+1) & 7 {
		case 0:
			acc += 3
		case 1:
			acc--
		case 2:
			acc *= 2
		case 3:
			acc ^= 0x5a
		case 4:
			acc |= 1
		case 5:
			acc &= 0xffff
		case 6:
			acc = acc<<1 | 1
		case 7:
			acc >>= 1
		}
	}
	return acc
}

// TestBTIJumpTableUnderEcvisor guards the BTI landing-pad seeding, and since
// `patches/0050` the index that answers it.
//
// Neutralized 2026-08-13 against `raptormark-builder:btinoseed`, a builder whose
// seeding block discards every pad it is given. The module built and ran, then
// died with "out of bounds memory access" and a calling stack of `1061, 1418`
// repeating -- the indirect `br` missing the block-address map, falling to the
// function catch-all and re-dispatching the same pc, one frame per hop.
// TestBTIJumpTableNativeBaseline passed on that same image, so the failure is
// the module and not the fixture. The assertion below observes seeding rather
// than naming it.
func TestBTIJumpTableUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuestBranchProtected(t, ctx, dir, "btiswitch", btiSwitchGuestSrc)
	// The fixture has to actually contain what the test claims, or a toolchain
	// that stopped emitting pads would quietly turn this into a test of nothing.
	requireBTIJumpPads(t, ctx, elf, 8)

	wasm := liftOne(t, ctx, img, dir, elf, "btiswitch")
	got := parseBTISwitchAcc(t, runWasm(t, ctx, wasm))
	if want := btiSwitchExpected(); got != want {
		t.Errorf("module computed acc=%d, independent reference says %d", got, want)
	}
}

// TestBTIJumpTableNativeBaseline runs the same guest on the host CPU, so a
// failure under ecvisor cannot be blamed on the fixture or on the reference.
func TestBTIJumpTableNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuestBranchProtected(t, ctx, dir, "btiswitch", btiSwitchGuestSrc)
	requireBTIJumpPads(t, ctx, elf, 8)

	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/btiswitch")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	if got, want := parseBTISwitchAcc(t, out), btiSwitchExpected(); got != want {
		t.Errorf("native run computed acc=%d, independent reference says %d", got, want)
	}
}

// compileGuestBranchProtected is compileGuest with branch protection turned on.
// It is separate rather than a parameter on compileGuest because every other
// fixture in the suite wants the plain flags, and because the flag is the whole
// point here: without it gcc emits the same jump table with no landing pads.
func compileGuestBranchProtected(t *testing.T, ctx context.Context, dir, name, src string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".c"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"},
		fmt.Sprintf("gcc -static -O2 -mbranch-protection=standard -o /w/%s /w/%s.c", name, name))
	if err != nil {
		t.Fatalf("compiling guest %s: %v\n%s", name, err, out)
	}
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("guest %s was not produced: %v", name, err)
	}
	return p
}

// requireBTIJumpPads fails unless the binary carries at least `min` `bti j` (or
// `bti jc`) landing pads. `bti c` does not count: it marks a CALL target, which
// direct discovery finds on its own, and counting it would let a binary with no
// jump pads at all satisfy the check.
func requireBTIJumpPads(t *testing.T, ctx context.Context, elf string, min int) {
	t.Helper()
	dir := mustAbs(t, filepath.Dir(elf))
	out, err := dockerRun(ctx, []string{"-v", dir + ":/w"},
		"objdump -d /w/"+filepath.Base(elf)+` | grep -cE 'bti[[:space:]]+jc?$' || true`)
	if err != nil {
		t.Fatalf("objdump on %s: %v\n%s", elf, err, out)
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if v, err := strconv.Atoi(strings.TrimSpace(line)); err == nil {
			n = v
		}
	}
	if n < min {
		t.Fatalf("fixture %s has %d bti j/jc landing pads, want >= %d; "+
			"the toolchain is not emitting them and this test would prove nothing",
			filepath.Base(elf), n, min)
	}
	t.Logf("fixture carries %d bti j/jc landing pads", n)
}

var btiAccRe = regexp.MustCompile(`acc=(-?\d+)`)

func parseBTISwitchAcc(t *testing.T, out string) int64 {
	t.Helper()
	m := btiAccRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no acc= line in output:\n%s", out)
	}
	v, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("parsing %q: %v", m[1], err)
	}
	return v
}
