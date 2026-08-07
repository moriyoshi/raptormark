package e2e

import (
	"strings"
	"testing"
)

// fcvtGuestSrc exercises the directed-rounding float-to-integer conversions
// added by patches/0028-fcvt-directed-rounding.patch.
//
// remill implemented only the round-toward-zero forms (FCVTZS/FCVTZU), which is
// what an ordinary C cast compiles to, so a program runs a very long way before
// meeting one of these: they appear only where the source rounds deliberately.
// PostgreSQL does, in XLOGfileslop's `ceil((double) distance / wal_segment_size)`,
// and died there partway through initdb.
//
// The expectations are written out per input rather than compared against a
// second implementation, and the same guest is run natively so the host CPU --
// which executes the real instructions -- is the oracle. The interesting cases
// are the ties, because that is the only place the four modes disagree:
//
//	value   N (even)   A (away)   P (ceil)   M (floor)
//	 2.5       2          3          3          2
//	 3.5       4          4          4          3
//	-2.5      -2         -3         -2         -3
const fcvtGuestSrc = `#define _GNU_SOURCE
#include <stdint.h>
#include <stdio.h>

static int failures = 0;
#define CHECK(got, want, what) do { \
	if ((got) != (want)) { \
		printf("FAIL %s: got %lld want %lld\n", what, (long long)(got), (long long)(want)); \
		failures++; \
	} \
} while (0)

#define DEF_S(name, insn, ctype, dreg) \
	static inline int64_t name(ctype v) { int64_t r; \
		__asm__(insn " %0, %" dreg "1" : "=r"(r) : "w"(v)); return r; }
#define DEF_U(name, insn, ctype, dreg) \
	static inline uint64_t name(ctype v) { uint64_t r; \
		__asm__(insn " %0, %" dreg "1" : "=r"(r) : "w"(v)); return r; }

DEF_S(fcvtns_d, "fcvtns", double, "d")
DEF_S(fcvtps_d, "fcvtps", double, "d")
DEF_S(fcvtms_d, "fcvtms", double, "d")
DEF_S(fcvtas_d, "fcvtas", double, "d")
DEF_U(fcvtnu_d, "fcvtnu", double, "d")
DEF_U(fcvtpu_d, "fcvtpu", double, "d")
DEF_U(fcvtmu_d, "fcvtmu", double, "d")
DEF_U(fcvtau_d, "fcvtau", double, "d")

/* 32-bit destination out of a float source, the other operand shape. */
static inline int32_t fcvtps_s32(float v) { int32_t r;
	__asm__("fcvtps %w0, %s1" : "=r"(r) : "w"(v)); return r; }
static inline int32_t fcvtms_s32(float v) { int32_t r;
	__asm__("fcvtms %w0, %s1" : "=r"(r) : "w"(v)); return r; }

int main(void) {
	/* Ties: the only inputs where the four modes differ. */
	CHECK(fcvtns_d(2.5), 2, "fcvtns(2.5) ties-to-even");
	CHECK(fcvtns_d(3.5), 4, "fcvtns(3.5) ties-to-even");
	CHECK(fcvtns_d(-2.5), -2, "fcvtns(-2.5) ties-to-even");
	CHECK(fcvtas_d(2.5), 3, "fcvtas(2.5) ties-away");
	CHECK(fcvtas_d(-2.5), -3, "fcvtas(-2.5) ties-away");
	CHECK(fcvtps_d(2.5), 3, "fcvtps(2.5) ceil");
	CHECK(fcvtps_d(-2.5), -2, "fcvtps(-2.5) ceil");
	CHECK(fcvtms_d(2.5), 2, "fcvtms(2.5) floor");
	CHECK(fcvtms_d(-2.5), -3, "fcvtms(-2.5) floor");

	/* Non-ties: ceil and floor must still differ from truncation. */
	CHECK(fcvtps_d(2.1), 3, "fcvtps(2.1)");
	CHECK(fcvtms_d(2.9), 2, "fcvtms(2.9)");
	CHECK(fcvtps_d(-2.9), -2, "fcvtps(-2.9)");
	CHECK(fcvtms_d(-2.1), -3, "fcvtms(-2.1)");

	/* Exact values must not move. */
	CHECK(fcvtps_d(7.0), 7, "fcvtps(7.0) exact");
	CHECK(fcvtms_d(7.0), 7, "fcvtms(7.0) exact");
	CHECK(fcvtns_d(7.0), 7, "fcvtns(7.0) exact");

	/* Unsigned: a negative input saturates to 0, it does not wrap. */
	CHECK(fcvtpu_d(-0.5), 0, "fcvtpu(-0.5) saturates to 0");
	CHECK(fcvtmu_d(-2.5), 0, "fcvtmu(-2.5) saturates to 0");
	CHECK(fcvtnu_d(2.5), 2, "fcvtnu(2.5) ties-to-even");
	CHECK(fcvtau_d(2.5), 3, "fcvtau(2.5) ties-away");
	CHECK(fcvtpu_d(0.25), 1, "fcvtpu(0.25) ceil");

	/* The 32-bit/float operand shape. */
	CHECK(fcvtps_s32(2.5f), 3, "fcvtps 32-bit ceil");
	CHECK(fcvtms_s32(2.5f), 2, "fcvtms 32-bit floor");
	CHECK(fcvtms_s32(-2.5f), -3, "fcvtms 32-bit floor negative");

	/* The exact shape PostgreSQL uses: ceil(distance / segsize). */
	double distance = 100.0 * 1024 * 1024, segsize = 16.0 * 1024 * 1024;
	CHECK(fcvtpu_d(distance / segsize), 7, "XLOGfileslop-shaped ceil");

	if (failures == 0) printf("FCVT-OK\n");
	return failures == 0 ? 0 : 1;
}
`

// TestFCVTDirectedRoundingUnderEcvisor guards the FCVT{N,P,M,A}{S,U} family.
func TestFCVTDirectedRoundingUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "fcvt", fcvtGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "fcvt")

	out := runWasm(t, ctx, wasm)
	assertFCVTGuestPassed(t, out)
}

// TestFCVTDirectedRoundingNativeBaseline runs the same guest on the host CPU,
// which executes the real instructions and is therefore the oracle for every
// expectation above.
func TestFCVTDirectedRoundingNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "fcvt", fcvtGuestSrc)

	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/fcvt")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertFCVTGuestPassed(t, out)
}

func assertFCVTGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "FCVT-OK") {
		t.Errorf("guest did not reach FCVT-OK; full output:\n%s", out)
	}
}
