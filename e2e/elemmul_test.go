package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// elemmulGuestSrc exercises the BY-ELEMENT widening multiplies that patch 0058
// implements: {S,U}MLAL{2}, {S,U}MLSL{2} and {S,U}MULL{2} in their
// `<Vm>.<Ts>[<index>]` forms.
//
// Patch 0051 implemented the register-register forms and called itself "the
// widening multiply family". These are a separate encoding class and were still
// stubs: 557 undecodable sites in the cryptography image, all of the shape
// `umlal <Vd>.2d, <Vn>.2s, <Vm>.s[i]`. Found by patch 0057's lift-time report.
//
// The inputs target the ways this specifically goes wrong:
//
//   - THE ELEMENT REGISTER IS 5 BITS (M:Rm) for 32-bit elements. Using Rm alone
//     reads the register 16 lower, which is correct for V0-V15 and silently
//     wrong above -- the defect patch 0045 documents, where a compiler that
//     happened to allocate a low register produced working code. So the element
//     register here is pinned to a HIGH one (v19), and the low register that
//     the bug would read instead (v3) is loaded with different values.
//   - THE INDEX ADDRESSES THE FULL 128-BIT ELEMENT REGISTER even when the
//     source arrangement is 64-bit. `v19.s[3]` with a `.2s` source is real
//     libcrypto code; a 64-bit view of the element operand cannot reach it.
//   - WIDENING BEFORE THE MULTIPLY. 0xffffffff squared needs 64 bits.
//   - ACCUMULATE vs OVERWRITE, and the `2` variant taking the HIGH half.
const elemmulGuestSrc = `#define _GNU_SOURCE
#include <stdint.h>
#include <stdio.h>
#include <string.h>

static int failures = 0;
static void chk(const char *what, uint64_t got, uint64_t want)
{
	if (got != want) {
		printf("FAIL %s: got 0x%016llx want 0x%016llx\n", what,
		       (unsigned long long)got, (unsigned long long)want);
		failures++;
	}
}

static volatile uint32_t src[4];
static volatile uint32_t elem_hi[4]; /* goes to v19 -- needs the M bit */
static volatile uint32_t elem_lo[4]; /* goes to v3  -- what the bug reads */
static volatile uint64_t acc[2];

int main(void)
{
	const uint32_t s[4] = {0xffffffffu, 3, 7, 0x80000000u};
	const uint32_t hi[4] = {11, 22, 33, 0xffffffffu};
	const uint32_t lo[4] = {101, 202, 303, 404};
	const uint64_t a[2] = {0x1000000000ULL, 5};
	for (int i = 0; i < 4; i++) {
		src[i] = s[i];
		elem_hi[i] = hi[i];
		elem_lo[i] = lo[i];
	}
	acc[0] = a[0];
	acc[1] = a[1];

	printf("check umlal-2d-2s-elem3\n");
	fflush(stdout);
	uint64_t o[2] = {0, 0};
	__asm__ volatile(
		"ld1 {v0.4s}, [%1]\n\t"
		"ld1 {v19.4s}, [%2]\n\t"
		"ld1 {v3.4s}, [%3]\n\t"
		"ld1 {v9.2d}, [%4]\n\t"
		"umlal v9.2d, v0.2s, v19.s[3]\n\t"
		"st1 {v9.2d}, [%0]\n\t"
		: : "r"(o), "r"(src), "r"(elem_hi), "r"(elem_lo), "r"(acc)
		: "v0", "v3", "v9", "v19", "memory");
	for (int i = 0; i < 2; i++) {
		char w[72];
		snprintf(w, sizeof w, "umlal 2d lane%d (element reg is M:Rm, index 3)", i);
		chk(w, o[i], a[i] + (uint64_t)s[i] * (uint64_t)hi[3]);
	}

	printf("check umlal2-2d-4s\n");
	fflush(stdout);
	memset(o, 0, sizeof o);
	__asm__ volatile(
		"ld1 {v0.4s}, [%1]\n\t"
		"ld1 {v19.4s}, [%2]\n\t"
		"ld1 {v9.2d}, [%3]\n\t"
		"umlal2 v9.2d, v0.4s, v19.s[1]\n\t"
		"st1 {v9.2d}, [%0]\n\t"
		: : "r"(o), "r"(src), "r"(elem_hi), "r"(acc)
		: "v0", "v9", "v19", "memory");
	for (int i = 0; i < 2; i++) {
		char w[72];
		snprintf(w, sizeof w, "umlal2 2d lane%d (HIGH half of the source)", i);
		chk(w, o[i], a[i] + (uint64_t)s[i + 2] * (uint64_t)hi[1]);
	}

	printf("check umull-2d-2s\n");
	fflush(stdout);
	memset(o, 0, sizeof o);
	__asm__ volatile(
		"ld1 {v0.4s}, [%1]\n\t"
		"ld1 {v19.4s}, [%2]\n\t"
		"ld1 {v9.2d}, [%3]\n\t"
		"umull v9.2d, v0.2s, v19.s[3]\n\t"
		"st1 {v9.2d}, [%0]\n\t"
		: : "r"(o), "r"(src), "r"(elem_hi), "r"(acc)
		: "v0", "v9", "v19", "memory");
	for (int i = 0; i < 2; i++) {
		char w[72];
		snprintf(w, sizeof w, "umull 2d lane%d (OVERWRITES, does not accumulate)", i);
		chk(w, o[i], (uint64_t)s[i] * (uint64_t)hi[3]);
	}

	printf("check smlal-2d-2s\n");
	fflush(stdout);
	memset(o, 0, sizeof o);
	__asm__ volatile(
		"ld1 {v0.4s}, [%1]\n\t"
		"ld1 {v19.4s}, [%2]\n\t"
		"ld1 {v9.2d}, [%3]\n\t"
		"smlal v9.2d, v0.2s, v19.s[3]\n\t"
		"st1 {v9.2d}, [%0]\n\t"
		: : "r"(o), "r"(src), "r"(elem_hi), "r"(acc)
		: "v0", "v9", "v19", "memory");
	for (int i = 0; i < 2; i++) {
		char w[72];
		snprintf(w, sizeof w, "smlal 2d lane%d (SIGNED lanes)", i);
		int64_t prod = (int64_t)(int32_t)s[i] * (int64_t)(int32_t)hi[3];
		chk(w, o[i], a[i] + (uint64_t)prod);
	}

	if (failures == 0) {
		printf("ELEMMUL-OK\n");
	}
	return failures == 0 ? 0 : 1;
}
`

// TestByElementWideningMultiplyUnderEcvisor guards patch 0058.
func TestByElementWideningMultiplyUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	elf := compileGuest(t, ctx, dir, "elemmul", elemmulGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "elemmul")
	assertElemmulGuestPassed(t, runWasm(t, ctx, wasm))
}

// TestByElementWideningMultiplyNativeBaseline pins the values to the hardware.
func TestByElementWideningMultiplyNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "elemmul", elemmulGuestSrc)
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx, []string{"-v", abs + ":/w"}, "/w/elemmul")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertElemmulGuestPassed(t, out)
}

func assertElemmulGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "ELEMMUL-OK") {
		t.Errorf("guest did not reach ELEMMUL-OK; full output:\n%s", out)
	}
}
