package e2e

import (
	"strings"
	"testing"
)

// crc32GuestSrc exercises the ARMv8 CRC32 extension, added to the lifter by
// patches/0027-crc32-dp-2src.patch.
//
// It surfaced on PostgreSQL, which decides whether to use its hardware CRC32C
// path by EXECUTING it under a SIGILL handler and comparing the result against
// its own software implementation. Before the patch the lifter emitted
// `__ecv_warning` for `crc32cx` and the module aborted mid-initdb.
//
// The instruction is requested with an inline `.arch armv8-a+crc` rather than a
// compiler flag, so the guest builds under the same plain `gcc -static -O2`
// every other e2e fixture uses.
//
// Correctness is pinned to the PUBLISHED check values for the string
// "123456789" -- 0xCBF43926 for CRC-32 and 0xE3069283 for CRC-32C -- not merely
// to agreement between two of my own implementations, which would pass just as
// happily if the polynomial were wrong in both. The bytewise reference is kept
// alongside so a mismatch says which of the two is out.
const crc32GuestSrc = `#define _GNU_SOURCE
#include <stdint.h>
#include <stdio.h>
#include <string.h>

static int failures = 0;
#define CHECK(cond, what) do { if (!(cond)) { printf("FAIL %s\n", what); failures++; } } while (0)

__asm__(".arch armv8-a+crc");

static inline uint32_t hw_crc32cb(uint32_t c, uint8_t v) {
	__asm__("crc32cb %w0, %w0, %w1" : "+r"(c) : "r"((uint32_t)v)); return c;
}
static inline uint32_t hw_crc32ch(uint32_t c, uint16_t v) {
	__asm__("crc32ch %w0, %w0, %w1" : "+r"(c) : "r"((uint32_t)v)); return c;
}
static inline uint32_t hw_crc32cw(uint32_t c, uint32_t v) {
	__asm__("crc32cw %w0, %w0, %w1" : "+r"(c) : "r"(v)); return c;
}
static inline uint32_t hw_crc32cx(uint32_t c, uint64_t v) {
	__asm__("crc32cx %w0, %w0, %1" : "+r"(c) : "r"(v)); return c;
}
static inline uint32_t hw_crc32b(uint32_t c, uint8_t v) {
	__asm__("crc32b %w0, %w0, %w1" : "+r"(c) : "r"((uint32_t)v)); return c;
}
static inline uint32_t hw_crc32x(uint32_t c, uint64_t v) {
	__asm__("crc32x %w0, %w0, %1" : "+r"(c) : "r"(v)); return c;
}

/* Reflected bytewise reference, the textbook equivalent of the instruction. */
static uint32_t ref(uint32_t crc, const uint8_t *p, size_t n, uint32_t poly) {
	for (size_t i = 0; i < n; i++) {
		crc ^= p[i];
		for (int b = 0; b < 8; b++)
			crc = (crc >> 1) ^ (poly & (uint32_t)(-(int32_t)(crc & 1)));
	}
	return crc;
}

int main(void) {
	const char *s = "123456789";
	const size_t n = 9;

	/* 1. Published check values, byte at a time. */
	uint32_t c = 0xFFFFFFFFu;
	for (size_t i = 0; i < n; i++) c = hw_crc32cb(c, (uint8_t)s[i]);
	printf("crc32c(\"123456789\") = %08x\n", ~c);
	CHECK(~c == 0xE3069283u, "CRC32C check value");

	uint32_t z = 0xFFFFFFFFu;
	for (size_t i = 0; i < n; i++) z = hw_crc32b(z, (uint8_t)s[i]);
	printf("crc32(\"123456789\")  = %08x\n", ~z);
	CHECK(~z == 0xCBF43926u, "CRC32 check value");

	/* 2. Every width must agree with the bytewise reference on the same data,
	      which is what catches a wrong byte order in the wider forms. */
	uint8_t buf[64];
	for (int i = 0; i < 64; i++) buf[i] = (uint8_t)(i * 7 + 3);

	uint32_t w = 0xFFFFFFFFu;
	for (int i = 0; i < 64; i += 8) { uint64_t v; memcpy(&v, buf + i, 8); w = hw_crc32cx(w, v); }
	CHECK(w == ref(0xFFFFFFFFu, buf, 64, 0x82F63B78u), "crc32cx vs reference");

	w = 0xFFFFFFFFu;
	for (int i = 0; i < 64; i += 4) { uint32_t v; memcpy(&v, buf + i, 4); w = hw_crc32cw(w, v); }
	CHECK(w == ref(0xFFFFFFFFu, buf, 64, 0x82F63B78u), "crc32cw vs reference");

	w = 0xFFFFFFFFu;
	for (int i = 0; i < 64; i += 2) { uint16_t v; memcpy(&v, buf + i, 2); w = hw_crc32ch(w, v); }
	CHECK(w == ref(0xFFFFFFFFu, buf, 64, 0x82F63B78u), "crc32ch vs reference");

	w = 0xFFFFFFFFu;
	for (int i = 0; i < 64; i += 8) { uint64_t v; memcpy(&v, buf + i, 8); w = hw_crc32x(w, v); }
	CHECK(w == ref(0xFFFFFFFFu, buf, 64, 0xEDB88320u), "crc32x vs reference");

	if (failures == 0) printf("CRC32-OK\n");
	return failures == 0 ? 0 : 1;
}
`

// TestCRC32UnderEcvisor guards the ARMv8 CRC32 lifting. See crc32GuestSrc for
// why it asserts published check values rather than self-consistency.
func TestCRC32UnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "crc32", crc32GuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "crc32")

	out := runWasm(t, ctx, wasm)
	assertCRC32GuestPassed(t, out)
}

// TestCRC32NativeBaseline runs the same guest on the host CPU, so a failure
// under ecvisor cannot be blamed on the fixture or on the check values.
func TestCRC32NativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "crc32", crc32GuestSrc)

	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/crc32")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertCRC32GuestPassed(t, out)
}

func assertCRC32GuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "CRC32-OK") {
		t.Errorf("guest did not reach CRC32-OK; full output:\n%s", out)
	}
}
