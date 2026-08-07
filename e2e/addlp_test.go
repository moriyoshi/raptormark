package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// addlpGuestSrc exercises the pairwise widening adds `{S,U}ADDLP` and
// `{S,U}ADALP` (`*_ASIMDMISC_P`), which patch 0054 implements.
//
// All four were `return false` decoder stubs. cryptography's Rust extension
// stops on `uaddlp v0.8h, v0.16b` — the second half of the vector POPCOUNT
// idiom, `cnt` followed by two pairwise widening adds folding byte counts into
// halfwords and then words.
//
// The inputs are chosen against the two ways this goes wrong, and neither is
// reachable from the idiom that found it:
//
//   - WIDENING AFTER THE ADD instead of before. Byte popcounts max out at 8, so
//     the sum of a pair never leaves the narrow type and the bug is invisible on
//     the very code that motivated the patch. The guard therefore feeds 0xff
//     lanes: 0xff + 0xff is 0x1fe, which truncates to 0xfe if the add happens at
//     source width. Same shape as the defect patch 0051 documents for the
//     widening multiplies.
//   - SIGNEDNESS. SADDLP sign-extends and UADDLP zero-extends, and they differ
//     only on lanes with the top bit set — so every signed case here uses
//     negative inputs, and the unsigned expectation for the same bytes is
//     checked alongside it.
//
// ADALP accumulates and ADDLP overwrites; the accumulator is seeded non-zero so
// an ADALP implemented as an ADDLP fails rather than coinciding.
//
// Lanes carry distinct values throughout, so a splat or a wrong-pair pairing
// fails instead of passing on symmetry.
const addlpGuestSrc = `#define _GNU_SOURCE
#include <stdint.h>
#include <stdio.h>
#include <string.h>

static int failures = 0;

static void check_u32(const char *what, uint32_t got, uint32_t want)
{
	if (got != want) {
		printf("FAIL %s: got 0x%08x want 0x%08x\n", what, got, want);
		failures++;
	}
}

static void check_u64(const char *what, uint64_t got, uint64_t want)
{
	if (got != want) {
		printf("FAIL %s: got 0x%016llx want 0x%016llx\n", what,
		       (unsigned long long)got, (unsigned long long)want);
		failures++;
	}
}

/* volatile: the expected values must come from the INSTRUCTION, not from the
   compiler folding the arithmetic at build time. */
static volatile uint8_t in8[16];
static volatile uint16_t in16[8];
static volatile uint16_t acc16[8];

static void uaddlp_8h_16b(uint16_t *out)
{
	__asm__ volatile(
		"ld1 {v0.16b}, [%1]\n\t"
		"uaddlp v0.8h, v0.16b\n\t"
		"st1 {v0.8h}, [%0]\n\t"
		: : "r"(out), "r"(in8) : "v0", "memory");
}

static void saddlp_8h_16b(uint16_t *out)
{
	__asm__ volatile(
		"ld1 {v0.16b}, [%1]\n\t"
		"saddlp v0.8h, v0.16b\n\t"
		"st1 {v0.8h}, [%0]\n\t"
		: : "r"(out), "r"(in8) : "v0", "memory");
}

static void uadalp_8h_16b(uint16_t *out)
{
	__asm__ volatile(
		"ld1 {v1.8h}, [%2]\n\t"
		"ld1 {v0.16b}, [%1]\n\t"
		"uadalp v1.8h, v0.16b\n\t"
		"st1 {v1.8h}, [%0]\n\t"
		: : "r"(out), "r"(in8), "r"(acc16) : "v0", "v1", "memory");
}

static void uaddlp_4s_8h(uint32_t *out)
{
	__asm__ volatile(
		"ld1 {v0.8h}, [%1]\n\t"
		"uaddlp v0.4s, v0.8h\n\t"
		"st1 {v0.4s}, [%0]\n\t"
		: : "r"(out), "r"(in16) : "v0", "memory");
}

/* The 64-bit (8B -> 4H) form is a separate DEF_ISEL from the 128-bit one and
   must leave the upper half of the register zeroed. */
static void uaddlp_4h_8b(uint16_t *out)
{
	__asm__ volatile(
		"movi v0.4s, #0\n\t"
		"ld1 {v0.8b}, [%1]\n\t"
		"uaddlp v0.4h, v0.8b\n\t"
		"st1 {v0.8h}, [%0]\n\t"
		: : "r"(out), "r"(in8) : "v0", "memory");
}

int main(void)
{
	/* Distinct per lane, and every PAIR sums past 0xff so a narrow add
	   truncates visibly. Lanes 12..15 are negative as signed. */
	const uint8_t v8[16] = {
		0xff, 0xff, 0x80, 0x81, 0x01, 0x02, 0x7f, 0x7f,
		0x00, 0xff, 0x10, 0x20, 0xfe, 0xfd, 0xc0, 0xb0,
	};
	for (int i = 0; i < 16; i++) {
		in8[i] = v8[i];
	}

	printf("check uaddlp-8h-16b\n");
	fflush(stdout);
	uint16_t o16[8];
	memset(o16, 0, sizeof o16);
	uaddlp_8h_16b(o16);
	for (int i = 0; i < 8; i++) {
		char what[64];
		snprintf(what, sizeof what, "uaddlp 8h lane%d", i);
		/* Widened FIRST, so the sum is a full 16-bit value. */
		check_u32(what, o16[i], (uint32_t)v8[2 * i] + (uint32_t)v8[2 * i + 1]);
	}

	printf("check saddlp-8h-16b\n");
	fflush(stdout);
	memset(o16, 0, sizeof o16);
	saddlp_8h_16b(o16);
	for (int i = 0; i < 8; i++) {
		char what[64];
		snprintf(what, sizeof what, "saddlp 8h lane%d", i);
		int32_t want = (int32_t)(int8_t)v8[2 * i] + (int32_t)(int8_t)v8[2 * i + 1];
		check_u32(what, o16[i], (uint32_t)(uint16_t)want);
	}

	printf("check uadalp-8h-16b\n");
	fflush(stdout);
	/* Seeded non-zero, so an ADALP that overwrites (i.e. an ADDLP) fails. */
	const uint16_t seed[8] = {1, 0x1000, 2, 0x2000, 3, 0x3000, 4, 0xfffe};
	for (int i = 0; i < 8; i++) {
		acc16[i] = seed[i];
	}
	memset(o16, 0, sizeof o16);
	uadalp_8h_16b(o16);
	for (int i = 0; i < 8; i++) {
		char what[64];
		snprintf(what, sizeof what, "uadalp 8h lane%d (accumulate, not overwrite)", i);
		uint32_t sum = (uint32_t)seed[i] + (uint32_t)v8[2 * i] + (uint32_t)v8[2 * i + 1];
		check_u32(what, o16[i], (uint32_t)(uint16_t)sum);
	}

	printf("check uaddlp-4s-8h\n");
	fflush(stdout);
	const uint16_t v16[8] = {
		0xffff, 0xffff, 0x8000, 0x8001, 0x0001, 0x0002, 0x7fff, 0x7fff,
	};
	for (int i = 0; i < 8; i++) {
		in16[i] = v16[i];
	}
	uint32_t o32[4] = {0, 0, 0, 0};
	uaddlp_4s_8h(o32);
	for (int i = 0; i < 4; i++) {
		char what[64];
		snprintf(what, sizeof what, "uaddlp 4s lane%d", i);
		check_u32(what, o32[i], (uint32_t)v16[2 * i] + (uint32_t)v16[2 * i + 1]);
	}

	printf("check uaddlp-4h-8b\n");
	fflush(stdout);
	memset(o16, 0, sizeof o16);
	uaddlp_4h_8b(o16);
	for (int i = 0; i < 4; i++) {
		char what[64];
		snprintf(what, sizeof what, "uaddlp 4h lane%d", i);
		check_u32(what, o16[i], (uint32_t)v8[2 * i] + (uint32_t)v8[2 * i + 1]);
	}
	/* A 64-bit form must ZERO the upper half rather than leave it, which is
	   what a 128-bit implementation reused for the short form would do. */
	uint64_t hi;
	memcpy(&hi, &o16[4], 8);
	check_u64("uaddlp 4h zeroes the upper half", hi, 0);

	/* CNT on its own, BEFORE the popcount chain that uses it. Isolating it
	   matters: a cnt that only handles the low 64 bits of a 16B operand
	   produces a chain whose upper words are zero, which reads exactly like a
	   broken uaddlp and is not one. */
	printf("check cnt-16b\n");
	fflush(stdout);
	uint8_t o8[16];
	memset(o8, 0, sizeof o8);
	__asm__ volatile(
		"ld1 {v0.16b}, [%1]\n\t"
		"cnt v0.16b, v0.16b\n\t"
		"st1 {v0.16b}, [%0]\n\t"
		: : "r"(o8), "r"(in8) : "v0", "memory");
	for (int i = 0; i < 16; i++) {
		char what[48];
		snprintf(what, sizeof what, "cnt 16b lane%d", i);
		check_u32(what, o8[i], (uint32_t)__builtin_popcount(v8[i]));
	}

	/* And the idiom this was actually found by: a vector popcount. */
	printf("check popcount\n");
	fflush(stdout);
	uint32_t pc[4];
	memset(pc, 0, sizeof pc);
	__asm__ volatile(
		"ld1 {v0.16b}, [%1]\n\t"
		"cnt v0.16b, v0.16b\n\t"
		"uaddlp v0.8h, v0.16b\n\t"
		"uaddlp v0.4s, v0.8h\n\t"
		"st1 {v0.4s}, [%0]\n\t"
		: : "r"(pc), "r"(in8) : "v0", "memory");
	for (int i = 0; i < 4; i++) {
		uint32_t want = 0;
		for (int b = 0; b < 4; b++) {
			want += (uint32_t)__builtin_popcount(v8[4 * i + b]);
		}
		char what[64];
		snprintf(what, sizeof what, "popcount word%d", i);
		check_u32(what, pc[i], want);
	}

	if (failures == 0) {
		printf("ADDLP-OK\n");
	}
	return failures == 0 ? 0 : 1;
}
`

// TestPairwiseWideningAddUnderEcvisor is the regression guard for patch 0054.
// Pre-patch the guest dies on the first `uaddlp` with SIGILL, so a regression is
// loud rather than a wrong number.
func TestPairwiseWideningAddUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "addlp", addlpGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "addlp")

	out := runWasm(t, ctx, wasm)
	assertAddlpGuestPassed(t, out)
}

// TestPairwiseWideningAddNativeBaseline runs the same guest on the hardware, so
// the expected values are pinned to what aarch64 does rather than to what the
// lifter was written to do.
func TestPairwiseWideningAddNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "addlp", addlpGuestSrc)

	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx, []string{"-v", abs + ":/w"}, "/w/addlp")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertAddlpGuestPassed(t, out)
}

func assertAddlpGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "ADDLP-OK") {
		t.Errorf("guest did not reach ADDLP-OK; full output:\n%s", out)
	}
}
