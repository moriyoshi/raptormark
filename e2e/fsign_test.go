package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// fsignGuestSrc exercises the VECTOR FNEG and FABS forms (`FNEG_ASIMDMISC_R`,
// `FABS_ASIMDMISC_R`), which patch 0053 implements.
//
// Both were `return false` decoder stubs, so a guest that negated a float vector
// met `__ecv_warning` and took SIGILL. python:3-slim stops on exactly that:
// `fneg v29.2d, v29.2d` inside `_PyCode_ConstantKey`, which is CPython's
// `copysign` on a double — build a sign mask with FNEG, blend it with BIF.
//
// The inputs are chosen so that a plausible-but-wrong implementation cannot
// pass, because the obvious implementation IS wrong:
//
//   - `0.0 - x` for FNEG. It agrees with FNEG everywhere except +0.0, where
//     `0.0 - 0.0` is +0.0 and FNEG(+0.0) must be -0.0. `copysign` exists to move
//     the sign of a zero, so that single value is the whole point. Checked by
//     comparing BIT PATTERNS, not by `==`: +0.0 == -0.0 in C, so an equality
//     test on the doubles passes for the broken version.
//   - `fabs()` via a compare-and-branch. Agrees except on -0.0 and on NaN.
//   - anything that routes through FP arithmetic quiets a signalling NaN and may
//     replace the payload. FPNeg and FPAbs are defined on the ENCODING, so a NaN
//     must come back with its payload intact and only the sign bit touched.
//
// Lane coverage matters separately: 2S, 4S and 2D are three distinct DEF_ISELs,
// and each lane of a vector carries a different value so a wrong-lane or
// splat-the-first-lane implementation fails rather than passing on symmetry.
const fsignGuestSrc = `#define _GNU_SOURCE
#include <stdint.h>
#include <stdio.h>
#include <string.h>

static int failures = 0;

static void check_u64(const char *what, uint64_t got, uint64_t want)
{
	if (got != want) {
		printf("FAIL %s: got 0x%016llx want 0x%016llx\n", what,
		       (unsigned long long)got, (unsigned long long)want);
		failures++;
	}
}

static void check_u32(const char *what, uint32_t got, uint32_t want)
{
	if (got != want) {
		printf("FAIL %s: got 0x%08x want 0x%08x\n", what, got, want);
		failures++;
	}
}

/* volatile so the values cannot be constant-folded into the expected answer:
   the point is that the INSTRUCTION ran, not that the compiler knows arithmetic. */
static volatile uint64_t in64[2];
static volatile uint32_t in32[4];

static void fneg_2d(uint64_t *out)
{
	uint64_t a = in64[0], b = in64[1];
	__asm__ volatile(
		"ins v0.d[0], %1\n\t"
		"ins v0.d[1], %2\n\t"
		"fneg v0.2d, v0.2d\n\t"
		"st1 {v0.2d}, [%0]\n\t"
		: : "r"(out), "r"(a), "r"(b) : "v0", "memory");
}

static void fabs_2d(uint64_t *out)
{
	uint64_t a = in64[0], b = in64[1];
	__asm__ volatile(
		"ins v0.d[0], %1\n\t"
		"ins v0.d[1], %2\n\t"
		"fabs v0.2d, v0.2d\n\t"
		"st1 {v0.2d}, [%0]\n\t"
		: : "r"(out), "r"(a), "r"(b) : "v0", "memory");
}

static void fneg_4s(uint32_t *out)
{
	uint32_t a = in32[0], b = in32[1], c = in32[2], d = in32[3];
	__asm__ volatile(
		"ins v0.s[0], %w1\n\t"
		"ins v0.s[1], %w2\n\t"
		"ins v0.s[2], %w3\n\t"
		"ins v0.s[3], %w4\n\t"
		"fneg v0.4s, v0.4s\n\t"
		"st1 {v0.4s}, [%0]\n\t"
		: : "r"(out), "r"(a), "r"(b), "r"(c), "r"(d) : "v0", "memory");
}

static void fabs_4s(uint32_t *out)
{
	uint32_t a = in32[0], b = in32[1], c = in32[2], d = in32[3];
	__asm__ volatile(
		"ins v0.s[0], %w1\n\t"
		"ins v0.s[1], %w2\n\t"
		"ins v0.s[2], %w3\n\t"
		"ins v0.s[3], %w4\n\t"
		"fabs v0.4s, v0.4s\n\t"
		"st1 {v0.4s}, [%0]\n\t"
		: : "r"(out), "r"(a), "r"(b), "r"(c), "r"(d) : "v0", "memory");
}

/* The 64-bit (2S) forms are a separate DEF_ISEL from the 128-bit (4S) ones and
   must write only the low half; the upper 64 bits of the register are zeroed. */
static void fneg_2s(uint32_t *out)
{
	uint32_t a = in32[0], b = in32[1];
	__asm__ volatile(
		"movi v0.4s, #0\n\t"
		"ins v0.s[0], %w1\n\t"
		"ins v0.s[1], %w2\n\t"
		"ins v0.s[2], %w1\n\t"
		"ins v0.s[3], %w2\n\t"
		"fneg v0.2s, v0.2s\n\t"
		"st1 {v0.4s}, [%0]\n\t"
		: : "r"(out), "r"(a), "r"(b) : "v0", "memory");
}

#define SIGN64 0x8000000000000000ULL
#define SIGN32 0x80000000U

int main(void)
{
	/* Distinct per lane, and every one of them a case the wrong
	   implementations get wrong: +0.0, -0.0, a signalling NaN with a payload,
	   an ordinary negative, +inf. */
	const uint64_t v64[2] = {
		0x0000000000000000ULL, /* +0.0  -> FNEG must give -0.0 */
		0xfff0000000000001ULL, /* -sNaN with payload 1 */
	};
	const uint32_t v32[4] = {
		0x00000000U, /* +0.0 */
		0x80000000U, /* -0.0 */
		0xff800001U, /* -sNaN, payload 1 */
		0xc1200000U, /* -10.0 */
	};

	printf("check fneg-2d\n");
	fflush(stdout);
	in64[0] = v64[0];
	in64[1] = v64[1];
	uint64_t o64[2] = {0, 0};
	fneg_2d(o64);
	check_u64("fneg 2d lane0 (+0.0 must become -0.0)", o64[0], v64[0] ^ SIGN64);
	check_u64("fneg 2d lane1 (sNaN payload must survive)", o64[1], v64[1] ^ SIGN64);

	printf("check fabs-2d\n");
	fflush(stdout);
	o64[0] = o64[1] = 0;
	fabs_2d(o64);
	check_u64("fabs 2d lane0", o64[0], v64[0] & ~SIGN64);
	check_u64("fabs 2d lane1 (sNaN payload must survive)", o64[1], v64[1] & ~SIGN64);

	printf("check fneg-4s\n");
	fflush(stdout);
	for (int i = 0; i < 4; i++) {
		in32[i] = v32[i];
	}
	uint32_t o32[4] = {0, 0, 0, 0};
	fneg_4s(o32);
	for (int i = 0; i < 4; i++) {
		char what[48];
		snprintf(what, sizeof what, "fneg 4s lane%d", i);
		check_u32(what, o32[i], v32[i] ^ SIGN32);
	}

	printf("check fabs-4s\n");
	fflush(stdout);
	memset(o32, 0, sizeof o32);
	fabs_4s(o32);
	for (int i = 0; i < 4; i++) {
		char what[48];
		snprintf(what, sizeof what, "fabs 4s lane%d", i);
		check_u32(what, o32[i], v32[i] & ~SIGN32);
	}

	printf("check fneg-2s\n");
	fflush(stdout);
	memset(o32, 0, sizeof o32);
	fneg_2s(o32);
	check_u32("fneg 2s lane0", o32[0], v32[0] ^ SIGN32);
	check_u32("fneg 2s lane1", o32[1], v32[1] ^ SIGN32);
	/* A 64-bit form must ZERO the upper half rather than leave the old
	   contents, which is what a 4S implementation reused for 2S would do. */
	check_u32("fneg 2s zeroes lane2", o32[2], 0);
	check_u32("fneg 2s zeroes lane3", o32[3], 0);

	/* And the thing this was actually found by: copysign on a double, which
	   the compiler lowers to fneg + bif. */
	printf("check copysign\n");
	fflush(stdout);
	static volatile double mag = 3.5;
	static volatile double sgn = -0.0;
	double cs = __builtin_copysign(mag, sgn);
	uint64_t csb;
	memcpy(&csb, (void *)&cs, 8);
	check_u64("copysign(3.5, -0.0)", csb, 0xc00c000000000000ULL);
	sgn = 0.0;
	cs = __builtin_copysign(mag, sgn);
	memcpy(&csb, (void *)&cs, 8);
	check_u64("copysign(3.5, +0.0)", csb, 0x400c000000000000ULL);

	if (failures == 0) {
		printf("FSIGN-OK\n");
	}
	return failures == 0 ? 0 : 1;
}
`

// TestVectorSignUnderEcvisor is the regression guard for patch 0053. Pre-patch
// the guest dies on the first `fneg v0.2d` with SIGILL, so a regression is loud
// rather than a wrong number.
func TestVectorSignUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "fsign", fsignGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "fsign")

	out := runWasm(t, ctx, wasm)
	assertFsignGuestPassed(t, out)
}

// TestVectorSignNativeBaseline runs the same guest on the hardware, so the
// expected bit patterns above are pinned to what aarch64 actually does rather
// than to what the lifter was written to do.
func TestVectorSignNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "fsign", fsignGuestSrc)

	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx, []string{"-v", abs + ":/w"}, "/w/fsign")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertFsignGuestPassed(t, out)
}

func assertFsignGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "FSIGN-OK") {
		t.Errorf("guest did not reach FSIGN-OK; full output:\n%s", out)
	}
}
