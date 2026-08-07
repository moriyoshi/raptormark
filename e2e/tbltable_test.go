package e2e

import (
	"debug/elf"
	"encoding/binary"
	"strings"
	"testing"
)

// The guard for elfconv patch 0063: TBL with 3- and 4-register tables.
//
// # Why this family
//
// An `[ecv-undecoded]` inventory of the postgres closure (2026-08-19) found
// 2,950 real undecoded sites over 398 encodings, and `tbl` was the largest
// single family at **706 sites -- every one of them the 4-register 16B form**.
// The same family leads an independent inventory taken from the cryptography
// image, so these are lifter-wide gaps rather than a postgres peculiarity.
//
// Before the patch the decoders existed only as generated stubs returning
// `false` in `Decode.cpp`, so every one of those sites lifted to `__ecv_warning`
// and executing one killed the guest.
//
// # What the inputs discriminate
//
// A table lookup is easy to implement in a way that is right for most inputs:
//
//   - The table is FOUR registers concatenated. The bytes are all distinct
//     (`i*7+11`), so assembling them in the wrong order changes the result;
//     a constant fill would hide it completely.
//   - Indices reach into all four source registers (0, 16, 32, 48, 63), so a
//     table built from only the first one or two still fails.
//   - Out-of-range indices (64, 200, 255) must yield ZERO. That is the rule that
//     makes TBL not a permute, and it is compared against the TABLE size, so a
//     bound of 16 or of the element count passes every small index and fails
//     these.
//   - The 3-register form uses index 48, which is INSIDE a 64-byte table and
//     outside a 48-byte one -- so it separates "reads the right table size" from
//     "reads a table".
//   - The wrap case's table is v30, v31, v0, v1: the register number wraps
//     modulo 32. No site in the measured corpus wraps, so nothing but this test
//     covers it.
//
// Every expected value comes from running the same binary natively; the guest
// prints checksums and the test compares the whole output.
//
// Neutralized against the pre-patch builder, not by argument: on
// `raptormark-builder:jt0062` this guest dies with `no lifted instruction at
// 0x4005a0`, which is the address of the first `tbl`.
func TestTBLTableLookupMatchesNative(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elfPath := compileGuest(t, ctx, dir, "tbltab", tblGuestSrc)

	// The fixture must contain the encoding the inventory found, or it is
	// testing some other instruction that happens to be spelled `tbl`.
	assertContainsEncoding(t, elfPath, "tbltab", 0x4e006200)

	wasm := liftOne(t, ctx, img, dir, elfPath, "tbltab")
	got := runWasm(t, ctx, wasm)

	// The native oracle: this binary is aarch64 and the builder image runs
	// natively, so the same guest can be run directly for the expected values.
	native, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/tbltab")
	if err != nil {
		t.Fatalf("running the guest natively: %v\n%s", err, native)
	}

	for _, line := range strings.Split(strings.TrimSpace(native), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.Contains(got, line) {
			t.Errorf("lifted output does not match the native oracle.\n"+
				"missing: %s\nA `no lifted instruction` failure here means the TBL\n"+
				"decoders are stubs again; a WRONG checksum means the table is being\n"+
				"assembled or bounded incorrectly.\nlifted:\n%s", line, got)
		}
	}
	if !strings.Contains(got, "TBLGUEST-OK") {
		t.Errorf("the guest did not run to completion:\n%s", got)
	}
}

// assertContainsEncoding fails unless `sym`'s body contains the 32-bit little
// endian instruction word `want`.
func assertContainsEncoding(t *testing.T, path, sym string, want uint32) {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, s := range f.Sections {
		if s.Flags&elf.SHF_EXECINSTR == 0 {
			continue
		}
		data, err := s.Data()
		if err != nil {
			continue
		}
		for i := 0; i+4 <= len(data); i += 4 {
			if binary.LittleEndian.Uint32(data[i:i+4]) == want {
				t.Logf("fixture carries encoding 0x%08x, one of the 706 postgres sites", want)
				return
			}
		}
	}
	t.Fatalf("encoding 0x%08x is not in %s; the fixture does not exercise the "+
		"instruction the inventory found", want, path)
}

// ⚠️ The wrap case is a raw `.inst`: GNU as rejects the wrapped register-list
// syntax `{v30.16b-v1.16b}`. 0x4e0563c5 was computed by hand and CONFIRMED
// against objdump, which disassembles it as
// `tbl v5.16b, {v30.16b, v31.16b, v0.16b, v1.16b}, v5.16b`.
const tblGuestSrc = `#include <stdint.h>
#include <stdio.h>
#include <string.h>

static void tbl4_16b(const uint8_t *tab, const uint8_t *idx, uint8_t *out) {
	__asm__ volatile("ldr q16, [%0]\n\tldr q17, [%0, #16]\n\t"
	                 "ldr q18, [%0, #32]\n\tldr q19, [%0, #48]\n\t"
	                 "ldr q0, [%1]\n\t"
	                 "tbl v0.16b, {v16.16b-v19.16b}, v0.16b\n\t"
	                 "str q0, [%2]\n\t"
	                 :
	                 : "r"(tab), "r"(idx), "r"(out)
	                 : "memory", "v0", "v16", "v17", "v18", "v19");
}

static void tbl4_8b(const uint8_t *tab, const uint8_t *idx, uint8_t *out) {
	__asm__ volatile("ldr q16, [%0]\n\tldr q17, [%0, #16]\n\t"
	                 "ldr q18, [%0, #32]\n\tldr q19, [%0, #48]\n\t"
	                 "ldr d0, [%1]\n\t"
	                 "tbl v0.8b, {v16.16b-v19.16b}, v0.8b\n\t"
	                 "str d0, [%2]\n\t"
	                 :
	                 : "r"(tab), "r"(idx), "r"(out)
	                 : "memory", "v0", "v16", "v17", "v18", "v19");
}

static void tbl3_16b(const uint8_t *tab, const uint8_t *idx, uint8_t *out) {
	__asm__ volatile("ldr q20, [%0]\n\tldr q21, [%0, #16]\n\tldr q22, [%0, #32]\n\t"
	                 "ldr q3, [%1]\n\t"
	                 "tbl v3.16b, {v20.16b-v22.16b}, v3.16b\n\t"
	                 "str q3, [%2]\n\t"
	                 :
	                 : "r"(tab), "r"(idx), "r"(out)
	                 : "memory", "v3", "v20", "v21", "v22");
}

static void tbl4_wrap(const uint8_t *tab, const uint8_t *idx, uint8_t *out) {
	__asm__ volatile("ldr q30, [%0]\n\tldr q31, [%0, #16]\n\t"
	                 "ldr q0, [%0, #32]\n\tldr q1, [%0, #48]\n\t"
	                 "ldr q5, [%1]\n\t"
	                 ".inst 0x4e0563c5\n\t"
	                 "str q5, [%2]\n\t"
	                 :
	                 : "r"(tab), "r"(idx), "r"(out)
	                 : "memory", "v0", "v1", "v5", "v30", "v31");
}

static unsigned long sum(const uint8_t *p, int n) {
	unsigned long s = 0;
	for (int i = 0; i < n; i++)
		s = s * 131 + p[i];
	return s;
}

int main(void) {
	uint8_t tab[64], idx[16], out[16];
	for (int i = 0; i < 64; i++)
		tab[i] = (uint8_t) (i * 7 + 11);
	static const uint8_t I[16] = {0, 16, 32, 48, 63, 17, 33, 49, 64, 200, 255, 1, 15, 47, 62, 31};
	memcpy(idx, I, 16);

	memset(out, 0xEE, 16);
	tbl4_16b(tab, idx, out);
	printf("TBL4-16B %lu %u %u %u %u %u\n", sum(out, 16), out[0], out[4], out[8], out[9], out[10]);

	memset(out, 0xEE, 16);
	tbl4_8b(tab, idx, out);
	printf("TBL4-8B %lu %u %u\n", sum(out, 8), out[0], out[7]);

	memset(out, 0xEE, 16);
	tbl3_16b(tab, idx, out);
	printf("TBL3-16B %lu %u %u\n", sum(out, 16), out[3], out[4]);

	memset(out, 0xEE, 16);
	tbl4_wrap(tab, idx, out);
	printf("TBL4-WRAP %lu %u %u\n", sum(out, 16), out[0], out[4]);

	printf("TBLGUEST-OK\n");
	return 0;
}
`
