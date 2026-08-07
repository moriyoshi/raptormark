package e2e

import (
	"strings"
	"testing"
)

// mullGuestSrc exercises the ASIMDDIFF widening-multiply family added by
// patches/0051-asimddiff-widening-multiply-family.patch: {S,U}MULL{2},
// UMLAL{2} and {S,U}MLSL{2}. SMLAL{2} came earlier (patch 0005) and is
// re-checked here so the family has one guard rather than two halves of one.
//
// It surfaced in redis: the lifted redis-server stopped at
//
//	69241c: 2e7ec232  umull v18.4s, v17.4h, v30.4h
//
// with _ecv_unreached, one instruction after a uaddw2 the lifter already
// decoded. Autovectorized loops emit the widening adds and the widening
// multiplies together, so a decoder that has one and not the other stops in the
// middle of a loop body.
//
// THREE BUGS THIS IS SHAPED TO CATCH, none of which a casual test sees:
//
//  1. Multiplying before widening. 0xffff * 0xffff is 0xfffe0001, which needs
//     the 32-bit destination; a 16-bit multiply gives 0x0001. Every unsigned
//     case below includes a product that overflows the source width, and the
//     signed cases include one that overflows into the sign bit.
//  2. Reading the wrong half in the "2" forms. The low and high halves of every
//     input carry DIFFERENT values, so an implementation that reads lanes 0..n
//     instead of n..2n produces a different answer rather than the same one.
//  3. Dropping the accumulator. MLAL/MLSL read-modify-write their destination,
//     which is a different operand action from MULL's write-only one; the
//     accumulator is seeded non-zero and differently per lane, so an
//     implementation that overwrites it instead of accumulating is visible.
//
// Expectations come from a scalar reference that widens first and multiplies in
// the wide type, straight from the ISA definition. The same guest runs natively
// under TestWideningMultiplyNativeBaseline, where the host CPU executes the real
// instructions -- so the reference is checked against hardware, and only then
// used as the oracle for the lifted run.
const mullGuestSrc = `#define _GNU_SOURCE
#include <stdint.h>
#include <stdio.h>

typedef uint8_t  u8x8   __attribute__((vector_size(8)));
typedef uint8_t  u8x16  __attribute__((vector_size(16)));
typedef uint16_t u16x4  __attribute__((vector_size(8)));
typedef uint16_t u16x8  __attribute__((vector_size(16)));
typedef uint32_t u32x2  __attribute__((vector_size(8)));
typedef uint32_t u32x4  __attribute__((vector_size(16)));
typedef uint64_t u64x2  __attribute__((vector_size(16)));
typedef int8_t   s8x8   __attribute__((vector_size(8)));
typedef int8_t   s8x16  __attribute__((vector_size(16)));
typedef int16_t  s16x4  __attribute__((vector_size(8)));
typedef int16_t  s16x8  __attribute__((vector_size(16)));
typedef int32_t  s32x2  __attribute__((vector_size(8)));
typedef int32_t  s32x4  __attribute__((vector_size(16)));
typedef int64_t  s64x2  __attribute__((vector_size(16)));

static int failures = 0;

static void fail_u(const char *what, int lane, uint64_t got, uint64_t want) {
	printf("FAIL %s lane %d: got %llu want %llu\n", what, lane,
	       (unsigned long long) got, (unsigned long long) want);
	failures++;
}

static void fail_s(const char *what, int lane, int64_t got, int64_t want) {
	printf("FAIL %s lane %d: got %lld want %lld\n", what, lane,
	       (long long) got, (long long) want);
	failures++;
}

/* Inputs are held in volatile arrays and loaded through memory so the compiler
   cannot constant-fold the whole computation and skip the instruction under
   test. A folded check passes without ever executing what it names. */
#define LOAD(vtype, etype, name, ...) \
	static volatile etype name##_src[] = { __VA_ARGS__ }; \
	vtype name; \
	{ etype tmp[sizeof(name) / sizeof(etype)]; \
	  for (unsigned i = 0; i < sizeof(name) / sizeof(etype); i++) tmp[i] = name##_src[i]; \
	  __builtin_memcpy(&name, tmp, sizeof(name)); }

/* --- the six shapes, one asm wrapper each ------------------------------- */

#define DEF_MULL(fn, insn, dv, sv, dt, st) \
	static inline dv fn(sv a, sv b) { dv r; \
		__asm__(insn " %0." dt ", %1." st ", %2." st : "=w"(r) : "w"(a), "w"(b)); return r; }

#define DEF_MLXL(fn, insn, dv, sv, dt, st) \
	static inline dv fn(dv acc, sv a, sv b) { \
		__asm__(insn " %0." dt ", %1." st ", %2." st : "+w"(acc) : "w"(a), "w"(b)); return acc; }

DEF_MULL(umull_8h8b,  "umull",  u16x8, u8x8,  "8h", "8b")
DEF_MULL(umull_4s4h,  "umull",  u32x4, u16x4, "4s", "4h")
DEF_MULL(umull_2d2s,  "umull",  u64x2, u32x2, "2d", "2s")
DEF_MULL(umull2_8h,   "umull2", u16x8, u8x16, "8h", "16b")
DEF_MULL(umull2_4s,   "umull2", u32x4, u16x8, "4s", "8h")
DEF_MULL(umull2_2d,   "umull2", u64x2, u32x4, "2d", "4s")

DEF_MULL(smull_8h8b,  "smull",  s16x8, s8x8,  "8h", "8b")
DEF_MULL(smull_4s4h,  "smull",  s32x4, s16x4, "4s", "4h")
DEF_MULL(smull_2d2s,  "smull",  s64x2, s32x2, "2d", "2s")
DEF_MULL(smull2_8h,   "smull2", s16x8, s8x16, "8h", "16b")
DEF_MULL(smull2_4s,   "smull2", s32x4, s16x8, "4s", "8h")
DEF_MULL(smull2_2d,   "smull2", s64x2, s32x4, "2d", "4s")

DEF_MLXL(umlal_4s4h,  "umlal",  u32x4, u16x4, "4s", "4h")
DEF_MLXL(umlal2_4s,   "umlal2", u32x4, u16x8, "4s", "8h")
DEF_MLXL(umlsl_4s4h,  "umlsl",  u32x4, u16x4, "4s", "4h")
DEF_MLXL(umlsl2_4s,   "umlsl2", u32x4, u16x8, "4s", "8h")
DEF_MLXL(smlal_4s4h,  "smlal",  s32x4, s16x4, "4s", "4h")
DEF_MLXL(smlal2_4s,   "smlal2", s32x4, s16x8, "4s", "8h")
DEF_MLXL(smlsl_4s4h,  "smlsl",  s32x4, s16x4, "4s", "4h")
DEF_MLXL(smlsl2_4s,   "smlsl2", s32x4, s16x8, "4s", "8h")
DEF_MLXL(umlal_2d2s,  "umlal",  u64x2, u32x2, "2d", "2s")
DEF_MLXL(smlal_8h8b,  "smlal",  s16x8, s8x8,  "8h", "8b")

int main(void) {
	/* ---- UMULL 4S4H: the exact shape redis stopped on ------------------ */
	{
		LOAD(u16x4, uint16_t, a, 0xffff, 0x1234, 2, 0x8000)
		LOAD(u16x4, uint16_t, b, 0xffff, 0x0010, 3, 0x8000)
		u32x4 got = umull_4s4h(a, b);
		for (int i = 0; i < 4; i++) {
			uint32_t want = (uint32_t) a_src[i] * (uint32_t) b_src[i];
			if (got[i] != want) fail_u("umull 4s4h", i, got[i], want);
		}
		/* Written out, because this is the lane that separates "widen then
		   multiply" from "multiply then widen": a 16-bit product gives 1. */
		if (got[0] != 0xfffe0001u) fail_u("umull 4s4h truncation", 0, got[0], 0xfffe0001u);
	}

	/* ---- UMULL2 4S: high half only ------------------------------------- */
	{
		LOAD(u16x8, uint16_t, a, 1, 2, 3, 4, 0xffff, 0x00ff, 0x1000, 0x8001)
		LOAD(u16x8, uint16_t, b, 5, 6, 7, 8, 0xffff, 0x0101, 0x0010, 0x0002)
		u32x4 got = umull2_4s(a, b);
		for (int i = 0; i < 4; i++) {
			uint32_t want = (uint32_t) a_src[i + 4] * (uint32_t) b_src[i + 4];
			if (got[i] != want) fail_u("umull2 4s", i, got[i], want);
			/* The low-half product for the same lane, which is what a
			   half-selection bug would return instead. */
			uint32_t low = (uint32_t) a_src[i] * (uint32_t) b_src[i];
			if (got[i] == low && want != low) fail_u("umull2 4s read the LOW half", i, got[i], want);
		}
	}

	/* ---- UMULL 8H8B and 2D2S ------------------------------------------- */
	{
		LOAD(u8x8, uint8_t, a, 255, 128, 3, 17, 0, 200, 7, 64)
		LOAD(u8x8, uint8_t, b, 255, 2, 5, 15, 99, 200, 0, 4)
		u16x8 got = umull_8h8b(a, b);
		for (int i = 0; i < 8; i++) {
			uint16_t want = (uint16_t) ((uint16_t) a_src[i] * (uint16_t) b_src[i]);
			if (got[i] != want) fail_u("umull 8h8b", i, got[i], want);
		}
		if (got[0] != 65025) fail_u("umull 8h8b 255*255", 0, got[0], 65025);
	}
	{
		LOAD(u32x2, uint32_t, a, 0xffffffffu, 0x00010001u)
		LOAD(u32x2, uint32_t, b, 0xffffffffu, 0x00010001u)
		u64x2 got = umull_2d2s(a, b);
		for (int i = 0; i < 2; i++) {
			uint64_t want = (uint64_t) a_src[i] * (uint64_t) b_src[i];
			if (got[i] != want) fail_u("umull 2d2s", i, got[i], want);
		}
	}

	/* ---- UMULL2 8H and 2D ---------------------------------------------- */
	{
		LOAD(u8x16, uint8_t, a, 1,2,3,4,5,6,7,8, 255,254,128,100,9,3,1,0)
		LOAD(u8x16, uint8_t, b, 9,9,9,9,9,9,9,9, 255,3,2,10,11,12,13,14)
		u16x8 got = umull2_8h(a, b);
		for (int i = 0; i < 8; i++) {
			uint16_t want = (uint16_t) ((uint16_t) a_src[i + 8] * (uint16_t) b_src[i + 8]);
			if (got[i] != want) fail_u("umull2 8h", i, got[i], want);
		}
	}
	{
		LOAD(u32x4, uint32_t, a, 1, 2, 0xffffffffu, 0x12345678u)
		LOAD(u32x4, uint32_t, b, 3, 4, 0xffffffffu, 0x10u)
		u64x2 got = umull2_2d(a, b);
		for (int i = 0; i < 2; i++) {
			uint64_t want = (uint64_t) a_src[i + 2] * (uint64_t) b_src[i + 2];
			if (got[i] != want) fail_u("umull2 2d", i, got[i], want);
		}
	}

	/* ---- SMULL: sign extension is the whole question -------------------- */
	{
		LOAD(s16x4, int16_t, a, -32768, -3, 1000, 32767)
		LOAD(s16x4, int16_t, b, 32767, 7, -1000, 32767)
		s32x4 got = smull_4s4h(a, b);
		for (int i = 0; i < 4; i++) {
			int32_t want = (int32_t) a_src[i] * (int32_t) b_src[i];
			if (got[i] != want) fail_s("smull 4s4h", i, got[i], want);
		}
		/* Zero-extending -3 instead of sign-extending it gives 458745, not -21. */
		if (got[1] != -21) fail_s("smull 4s4h sign extension", 1, got[1], -21);
	}
	{
		LOAD(s8x8, int8_t, a, -128, -1, 127, -50, 0, 3, -7, 100)
		LOAD(s8x8, int8_t, b, -128, 127, 127, 2, -9, -3, -7, -1)
		s16x8 got = smull_8h8b(a, b);
		for (int i = 0; i < 8; i++) {
			int16_t want = (int16_t) ((int16_t) a_src[i] * (int16_t) b_src[i]);
			if (got[i] != want) fail_s("smull 8h8b", i, got[i], want);
		}
		if (got[0] != 16384) fail_s("smull 8h8b -128*-128", 0, got[0], 16384);
	}
	{
		LOAD(s32x2, int32_t, a, -2147483648, 65536)
		LOAD(s32x2, int32_t, b, 2147483647, -65536)
		s64x2 got = smull_2d2s(a, b);
		for (int i = 0; i < 2; i++) {
			int64_t want = (int64_t) a_src[i] * (int64_t) b_src[i];
			if (got[i] != want) fail_s("smull 2d2s", i, got[i], want);
		}
	}
	{
		LOAD(s8x16, int8_t, a, 1,1,1,1,1,1,1,1, -128,-1,127,-50,0,3,-7,100)
		LOAD(s8x16, int8_t, b, 1,1,1,1,1,1,1,1, -128,127,127,2,-9,-3,-7,-1)
		s16x8 got = smull2_8h(a, b);
		for (int i = 0; i < 8; i++) {
			int16_t want = (int16_t) ((int16_t) a_src[i + 8] * (int16_t) b_src[i + 8]);
			if (got[i] != want) fail_s("smull2 8h", i, got[i], want);
		}
	}
	{
		LOAD(s16x8, int16_t, a, 2,2,2,2, -32768, -3, 1000, 32767)
		LOAD(s16x8, int16_t, b, 2,2,2,2, 32767, 7, -1000, 32767)
		s32x4 got = smull2_4s(a, b);
		for (int i = 0; i < 4; i++) {
			int32_t want = (int32_t) a_src[i + 4] * (int32_t) b_src[i + 4];
			if (got[i] != want) fail_s("smull2 4s", i, got[i], want);
		}
	}
	{
		LOAD(s32x4, int32_t, a, 1, 1, -2147483648, 65536)
		LOAD(s32x4, int32_t, b, 1, 1, 2147483647, -65536)
		s64x2 got = smull2_2d(a, b);
		for (int i = 0; i < 2; i++) {
			int64_t want = (int64_t) a_src[i + 2] * (int64_t) b_src[i + 2];
			if (got[i] != want) fail_s("smull2 2d", i, got[i], want);
		}
	}

	/* ---- MLAL / MLSL: the accumulator must survive ---------------------- */
	{
		LOAD(u32x4, uint32_t, acc0, 1000, 0xfffffff0u, 7, 0)
		LOAD(u16x4, uint16_t, a, 0xffff, 0x10, 6, 300)
		LOAD(u16x4, uint16_t, b, 0xffff, 0x20, 7, 300)
		u32x4 got = umlal_4s4h(acc0, a, b);
		for (int i = 0; i < 4; i++) {
			uint32_t want = acc0_src[i] + (uint32_t) a_src[i] * (uint32_t) b_src[i];
			if (got[i] != want) fail_u("umlal 4s4h", i, got[i], want);
			uint32_t noacc = (uint32_t) a_src[i] * (uint32_t) b_src[i];
			if (got[i] == noacc && want != noacc) fail_u("umlal 4s4h DROPPED the accumulator", i, got[i], want);
		}
	}
	{
		LOAD(u32x4, uint32_t, acc0, 5, 6, 7, 8)
		LOAD(u16x8, uint16_t, a, 1, 2, 3, 4, 0xffff, 9, 10, 11)
		LOAD(u16x8, uint16_t, b, 1, 2, 3, 4, 0xffff, 9, 10, 11)
		u32x4 got = umlal2_4s(acc0, a, b);
		for (int i = 0; i < 4; i++) {
			uint32_t want = acc0_src[i] + (uint32_t) a_src[i + 4] * (uint32_t) b_src[i + 4];
			if (got[i] != want) fail_u("umlal2 4s", i, got[i], want);
		}
	}
	{
		LOAD(u32x4, uint32_t, acc0, 0xffffffffu, 100000, 50, 0)
		LOAD(u16x4, uint16_t, a, 0xffff, 100, 7, 1)
		LOAD(u16x4, uint16_t, b, 0xffff, 100, 7, 1)
		u32x4 got = umlsl_4s4h(acc0, a, b);
		for (int i = 0; i < 4; i++) {
			/* Unsigned wraparound is the defined behaviour, and lane 3 (0 - 1)
			   is the one that says so. */
			uint32_t want = acc0_src[i] - (uint32_t) a_src[i] * (uint32_t) b_src[i];
			if (got[i] != want) fail_u("umlsl 4s4h", i, got[i], want);
		}
		if (got[3] != 0xffffffffu) fail_u("umlsl 4s4h wraparound", 3, got[3], 0xffffffffu);
	}
	{
		LOAD(u32x4, uint32_t, acc0, 1000000, 2000000, 3000000, 4000000)
		LOAD(u16x8, uint16_t, a, 0, 0, 0, 0, 100, 200, 300, 400)
		LOAD(u16x8, uint16_t, b, 0, 0, 0, 0, 10, 20, 30, 40)
		u32x4 got = umlsl2_4s(acc0, a, b);
		for (int i = 0; i < 4; i++) {
			uint32_t want = acc0_src[i] - (uint32_t) a_src[i + 4] * (uint32_t) b_src[i + 4];
			if (got[i] != want) fail_u("umlsl2 4s", i, got[i], want);
		}
	}
	{
		LOAD(s32x4, int32_t, acc0, -1000, 5, 0, 2147483647)
		LOAD(s16x4, int16_t, a, -32768, -3, 1000, 0)
		LOAD(s16x4, int16_t, b, 32767, 7, -1000, 0)
		s32x4 got = smlal_4s4h(acc0, a, b);
		for (int i = 0; i < 4; i++) {
			int32_t want = (int32_t) (acc0_src[i] + (int32_t) a_src[i] * (int32_t) b_src[i]);
			if (got[i] != want) fail_s("smlal 4s4h", i, got[i], want);
		}
	}
	{
		LOAD(s32x4, int32_t, acc0, 1, 2, 3, 4)
		LOAD(s16x8, int16_t, a, 9, 9, 9, 9, -32768, -3, 1000, 7)
		LOAD(s16x8, int16_t, b, 9, 9, 9, 9, 32767, 7, -1000, -7)
		s32x4 got = smlal2_4s(acc0, a, b);
		for (int i = 0; i < 4; i++) {
			int32_t want = (int32_t) (acc0_src[i] + (int32_t) a_src[i + 4] * (int32_t) b_src[i + 4]);
			if (got[i] != want) fail_s("smlal2 4s", i, got[i], want);
		}
	}
	{
		LOAD(s32x4, int32_t, acc0, 0, -5, 1000000, 17)
		LOAD(s16x4, int16_t, a, -32768, -3, 1000, 0)
		LOAD(s16x4, int16_t, b, 32767, 7, -1000, 0)
		s32x4 got = smlsl_4s4h(acc0, a, b);
		for (int i = 0; i < 4; i++) {
			int32_t want = (int32_t) (acc0_src[i] - (int32_t) a_src[i] * (int32_t) b_src[i]);
			if (got[i] != want) fail_s("smlsl 4s4h", i, got[i], want);
		}
	}
	{
		LOAD(s32x4, int32_t, acc0, 11, 22, 33, 44)
		LOAD(s16x8, int16_t, a, 4, 4, 4, 4, -100, 200, -300, 400)
		LOAD(s16x8, int16_t, b, 4, 4, 4, 4, 300, -400, 500, -600)
		s32x4 got = smlsl2_4s(acc0, a, b);
		for (int i = 0; i < 4; i++) {
			int32_t want = (int32_t) (acc0_src[i] - (int32_t) a_src[i + 4] * (int32_t) b_src[i + 4]);
			if (got[i] != want) fail_s("smlsl2 4s", i, got[i], want);
		}
	}

	/* The two accumulate shapes at the other element widths, so a per-width
	   template error is not hidden by the 4S coverage above. */
	{
		LOAD(u64x2, uint64_t, acc0, 1, 0xffffffffffffffffull)
		LOAD(u32x2, uint32_t, a, 0xffffffffu, 2)
		LOAD(u32x2, uint32_t, b, 0xffffffffu, 3)
		u64x2 got = umlal_2d2s(acc0, a, b);
		for (int i = 0; i < 2; i++) {
			uint64_t want = acc0_src[i] + (uint64_t) a_src[i] * (uint64_t) b_src[i];
			if (got[i] != want) fail_u("umlal 2d2s", i, got[i], want);
		}
	}
	{
		LOAD(s16x8, int16_t, acc0, -1, 2, -3, 4, -5, 6, -7, 8)
		LOAD(s8x8, int8_t, a, -128, -1, 127, -50, 0, 3, -7, 100)
		LOAD(s8x8, int8_t, b, -128, 127, 127, 2, -9, -3, -7, -1)
		s16x8 got = smlal_8h8b(acc0, a, b);
		for (int i = 0; i < 8; i++) {
			int16_t want = (int16_t) (acc0_src[i] + (int16_t) ((int16_t) a_src[i] * (int16_t) b_src[i]));
			if (got[i] != want) fail_s("smlal 8h8b", i, got[i], want);
		}
	}

	if (failures == 0) printf("MULL-OK\n");
	return failures == 0 ? 0 : 1;
}
`

// TestWideningMultiplyUnderEcvisor guards the ASIMDDIFF widening-multiply
// family. Before patch 0051 the module did not even reach main: the lifter
// emitted _ecv_unreached for every one of these encodings.
func TestWideningMultiplyUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "mull", mullGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "mull")
	assertMullGuestPassed(t, runWasm(t, ctx, wasm))
}

// TestWideningMultiplyNativeBaseline runs the same guest on the host CPU, which
// executes the real instructions. It is what makes the scalar reference inside
// the guest an oracle rather than a second guess.
func TestWideningMultiplyNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "mull", mullGuestSrc)

	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/mull")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertMullGuestPassed(t, out)
}

func assertMullGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "MULL-OK") {
		t.Errorf("guest did not reach MULL-OK; full output:\n%s", out)
	}
}
