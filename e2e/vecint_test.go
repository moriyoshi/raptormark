package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// vecintGuestSrc exercises the vector integer forms patch 0056 implements:
// NEG, ABS (ASIMDMISC_R), MLA, MLS (ASIMDSAME_ONLY) and USUBL (ASIMDDIFF_L).
//
// cryptography's Rust extension stops on `neg v28.2s, v31.2s` inside a
// constant-time cmov. The other four were chosen by MEASUREMENT rather than by
// taking the whole encoding group: of 59 stubbed vector mnemonics, these are the
// five its 37.89 MiB image actually contains. Implementing the reciprocal
// estimates alongside them would have meant shipping approximations nothing
// could verify.
//
// Discriminating inputs, one per way this goes wrong:
//
//   - NEG and ABS of the most negative value must WRAP to itself, not saturate.
//     Saturating is SQNEG/SQABS, different instructions and still stubs, and a
//     saturating implementation is correct on every other input.
//   - MLA/MLS must READ the destination. Seeded non-zero, so an implementation
//     copied from MUL fails instead of coinciding.
//   - USUBL must widen BEFORE subtracting, so a smaller-minus-larger pair gives
//     a large positive wide value rather than a truncated one. Feeding only
//     larger-minus-smaller pairs would hide it.
const vecintGuestSrc = `#define _GNU_SOURCE
#include <stdint.h>
#include <stdio.h>
#include <string.h>

static int failures = 0;
static void chk64(const char *what, uint64_t got, uint64_t want)
{
	if (got != want) {
		printf("FAIL %s: got 0x%016llx want 0x%016llx\n", what,
		       (unsigned long long)got, (unsigned long long)want);
		failures++;
	}
}

static volatile int32_t i32[4];
static volatile uint16_t u16[8];
static volatile uint16_t acc[8];
static volatile uint32_t u32a[4];
static volatile uint32_t u32b[4];

int main(void)
{
	/* INT32_MIN first: NEG and ABS of it must come back INT32_MIN. */
	const int32_t v32[4] = {(int32_t)0x80000000, -1, 7, (int32_t)0xfffffff9};
	for (int i = 0; i < 4; i++) {
		i32[i] = v32[i];
	}

	printf("check neg-4s\n");
	fflush(stdout);
	int32_t o32[4];
	memset(o32, 0, sizeof o32);
	__asm__ volatile("ld1 {v0.4s}, [%1]\n\tneg v0.4s, v0.4s\n\tst1 {v0.4s}, [%0]\n\t"
			 : : "r"(o32), "r"(i32) : "v0", "memory");
	for (int i = 0; i < 4; i++) {
		char w[48];
		snprintf(w, sizeof w, "neg 4s lane%d", i);
		chk64(w, (uint32_t)o32[i], (uint32_t)(0u - (uint32_t)v32[i]));
	}

	printf("check abs-4s\n");
	fflush(stdout);
	memset(o32, 0, sizeof o32);
	__asm__ volatile("ld1 {v0.4s}, [%1]\n\tabs v0.4s, v0.4s\n\tst1 {v0.4s}, [%0]\n\t"
			 : : "r"(o32), "r"(i32) : "v0", "memory");
	for (int i = 0; i < 4; i++) {
		char w[48];
		snprintf(w, sizeof w, "abs 4s lane%d (INT_MIN must WRAP, not saturate)", i);
		uint32_t want = v32[i] < 0 ? (uint32_t)(0u - (uint32_t)v32[i]) : (uint32_t)v32[i];
		chk64(w, (uint32_t)o32[i], want);
	}

	printf("check mla-mls-8h\n");
	fflush(stdout);
	const uint16_t a[8] = {0xffff, 2, 3, 0x8000, 5, 6, 7, 0x1234};
	const uint16_t s[8] = {0x1111, 0x2222, 3, 4, 5, 6, 7, 8};
	for (int i = 0; i < 8; i++) {
		u16[i] = a[i];
		acc[i] = s[i];
	}
	uint16_t o16[8];
	memset(o16, 0, sizeof o16);
	__asm__ volatile("ld1 {v1.8h}, [%2]\n\tld1 {v0.8h}, [%1]\n\t"
			 "mla v1.8h, v0.8h, v0.8h\n\tst1 {v1.8h}, [%0]\n\t"
			 : : "r"(o16), "r"(u16), "r"(acc) : "v0", "v1", "memory");
	for (int i = 0; i < 8; i++) {
		char w[64];
		snprintf(w, sizeof w, "mla 8h lane%d (accumulate, not overwrite)", i);
		chk64(w, o16[i], (uint16_t)(s[i] + (uint16_t)(a[i] * a[i])));
	}
	memset(o16, 0, sizeof o16);
	__asm__ volatile("ld1 {v1.8h}, [%2]\n\tld1 {v0.8h}, [%1]\n\t"
			 "mls v1.8h, v0.8h, v0.8h\n\tst1 {v1.8h}, [%0]\n\t"
			 : : "r"(o16), "r"(u16), "r"(acc) : "v0", "v1", "memory");
	for (int i = 0; i < 8; i++) {
		char w[64];
		snprintf(w, sizeof w, "mls 8h lane%d", i);
		chk64(w, o16[i], (uint16_t)(s[i] - (uint16_t)(a[i] * a[i])));
	}

	printf("check usubl-2d-2s\n");
	fflush(stdout);
	/* Lane 0 is smaller minus larger: widened first this is a huge positive
	   64-bit value; subtracted narrow it would truncate to 32 bits. */
	const uint32_t x[4] = {1, 0xffffffffu, 0x80000000u, 5};
	const uint32_t y[4] = {0xffffffffu, 1, 1, 5};
	for (int i = 0; i < 4; i++) {
		u32a[i] = x[i];
		u32b[i] = y[i];
	}
	uint64_t o64[2];
	memset(o64, 0, sizeof o64);
	__asm__ volatile("ld1 {v0.4s}, [%1]\n\tld1 {v1.4s}, [%2]\n\t"
			 "usubl v2.2d, v0.2s, v1.2s\n\tst1 {v2.2d}, [%0]\n\t"
			 : : "r"(o64), "r"(u32a), "r"(u32b) : "v0", "v1", "v2", "memory");
	for (int i = 0; i < 2; i++) {
		char w[64];
		snprintf(w, sizeof w, "usubl 2d lane%d (widen BEFORE subtract)", i);
		chk64(w, o64[i], (uint64_t)x[i] - (uint64_t)y[i]);
	}

	if (failures == 0) {
		printf("VECINT-OK\n");
	}
	return failures == 0 ? 0 : 1;
}
`

// TestVectorIntegerUnderEcvisor is the regression guard for patch 0056.
func TestVectorIntegerUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	elf := compileGuest(t, ctx, dir, "vecint", vecintGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "vecint")
	assertVecintGuestPassed(t, runWasm(t, ctx, wasm))
}

// TestVectorIntegerNativeBaseline pins the expectations to the hardware.
func TestVectorIntegerNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "vecint", vecintGuestSrc)
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx, []string{"-v", abs + ":/w"}, "/w/vecint")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertVecintGuestPassed(t, out)
}

func assertVecintGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "VECINT-OK") {
		t.Errorf("guest did not reach VECINT-OK; full output:\n%s", out)
	}
}
