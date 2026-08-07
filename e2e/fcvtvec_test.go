package e2e

import (
	"strings"
	"testing"
)

// fcvtVecGuestSrc exercises the ASIMD (vector) float-to-integer conversions
// added by patches/0041-fcvt-vector-asimdmisc.patch.
//
// Patch 0028 covered the SCALAR family and this one is a separate encoding
// entirely -- two-register misc, not float2int -- so nothing about the scalar
// guard says anything here. It surfaced as `fcvtzs v30.2s, v30.2s` inside
// libicuuc, which stopped initdb's post-bootstrap phase with `_ecv_unreached`
// long after the scalar forms were working.
//
// The expectations are written out per input rather than compared against a
// second implementation, and the same guest runs natively so the host CPU --
// which executes the real instructions -- is the oracle. The interesting inputs
// are the ties, because that is the only place the five rounding modes disagree:
//
//	value   Z (trunc)  N (even)   A (away)   P (ceil)   M (floor)
//	 2.5       2          2          3          3          2
//	 3.5       3          4          4          4          3
//	-2.5      -2         -2         -3         -2         -3
//
// Both lanes of every vector carry a DIFFERENT value. A per-lane bug -- reading
// lane 0 twice, or writing the result of lane 0 into both -- is invisible when
// the lanes agree, and it is the failure mode a vector implementation actually
// has.
const fcvtVecGuestSrc = `#define _GNU_SOURCE
#include <limits.h>
#include <stdint.h>
#include <stdio.h>

typedef float    v2f __attribute__((vector_size(8)));
typedef int32_t  v2i __attribute__((vector_size(8)));
typedef uint32_t v2u __attribute__((vector_size(8)));
typedef float    v4f __attribute__((vector_size(16)));
typedef int32_t  v4i __attribute__((vector_size(16)));
typedef double   v2d __attribute__((vector_size(16)));
typedef int64_t  v2l __attribute__((vector_size(16)));
typedef uint64_t v2ul __attribute__((vector_size(16)));

static int failures = 0;
#define CHECK2(got, w0, w1, what) do { \
	if ((got)[0] != (w0) || (got)[1] != (w1)) { \
		printf("FAIL %s: got {%lld,%lld} want {%lld,%lld}\n", what, \
		       (long long)(got)[0], (long long)(got)[1], \
		       (long long)(w0), (long long)(w1)); \
		failures++; \
	} \
} while (0)

/* 2S: two single-precision lanes out of a 64-bit vector register -- the exact
   shape libicuuc used. */
#define DEF_2S(name, insn, dtype) \
	static inline dtype name(v2f v) { dtype r; \
		__asm__(insn " %0.2s, %1.2s" : "=w"(r) : "w"(v)); return r; }
/* 2D: two double-precision lanes out of a 128-bit vector register. */
#define DEF_2D(name, insn, dtype) \
	static inline dtype name(v2d v) { dtype r; \
		__asm__(insn " %0.2d, %1.2d" : "=w"(r) : "w"(v)); return r; }

DEF_2S(fcvtzs_2s, "fcvtzs", v2i)
DEF_2S(fcvtns_2s, "fcvtns", v2i)
DEF_2S(fcvtps_2s, "fcvtps", v2i)
DEF_2S(fcvtms_2s, "fcvtms", v2i)
DEF_2S(fcvtas_2s, "fcvtas", v2i)
DEF_2S(fcvtzu_2s, "fcvtzu", v2u)
DEF_2S(fcvtnu_2s, "fcvtnu", v2u)
DEF_2S(fcvtpu_2s, "fcvtpu", v2u)
DEF_2S(fcvtmu_2s, "fcvtmu", v2u)
DEF_2S(fcvtau_2s, "fcvtau", v2u)

DEF_2D(fcvtzs_2d, "fcvtzs", v2l)
DEF_2D(fcvtns_2d, "fcvtns", v2l)
DEF_2D(fcvtps_2d, "fcvtps", v2l)
DEF_2D(fcvtms_2d, "fcvtms", v2l)
DEF_2D(fcvtas_2d, "fcvtas", v2l)
DEF_2D(fcvtzu_2d, "fcvtzu", v2ul)

/* 4S: the Q-bit form, a different ISEL from 2S. */
static inline v4i fcvtzs_4s(v4f v) { v4i r;
	__asm__("fcvtzs %0.4s, %1.4s" : "=w"(r) : "w"(v)); return r; }
static inline v4i fcvtms_4s(v4f v) { v4i r;
	__asm__("fcvtms %0.4s, %1.4s" : "=w"(r) : "w"(v)); return r; }

int main(void) {
	/* Ties, single precision. Lanes differ so a lane mix-up cannot hide. */
	v2f t = {2.5f, -2.5f};
	CHECK2(fcvtzs_2s(t),  2, -2, "fcvtzs.2s ties (truncate)");
	CHECK2(fcvtns_2s(t),  2, -2, "fcvtns.2s ties-to-even");
	CHECK2(fcvtps_2s(t),  3, -2, "fcvtps.2s ceil");
	CHECK2(fcvtms_2s(t),  2, -3, "fcvtms.2s floor");
	CHECK2(fcvtas_2s(t),  3, -3, "fcvtas.2s ties-away");

	/* 3.5 is the other tie: nearest-even goes UP here and DOWN from 2.5, which
	   is what separates ties-to-even from plain truncation on both. */
	v2f u = {3.5f, 2.5f};
	CHECK2(fcvtns_2s(u), 4, 2, "fcvtns.2s ties-to-even both directions");
	CHECK2(fcvtzs_2s(u), 3, 2, "fcvtzs.2s truncate");

	/* Non-ties: ceil and floor must still differ from truncation. */
	v2f n = {2.1f, -2.1f};
	CHECK2(fcvtps_2s(n),  3, -2, "fcvtps.2s non-tie");
	CHECK2(fcvtms_2s(n),  2, -3, "fcvtms.2s non-tie");
	CHECK2(fcvtzs_2s(n),  2, -2, "fcvtzs.2s non-tie");

	/* Exact values must not move. */
	v2f e = {7.0f, -7.0f};
	CHECK2(fcvtns_2s(e), 7, -7, "fcvtns.2s exact");
	CHECK2(fcvtps_2s(e), 7, -7, "fcvtps.2s exact");
	CHECK2(fcvtms_2s(e), 7, -7, "fcvtms.2s exact");

	/* Unsigned: a negative input saturates to 0, it does not wrap. */
	v2f g = {-0.5f, 2.5f};
	CHECK2(fcvtzu_2s(g), 0u, 2u, "fcvtzu.2s saturates a negative to 0");
	CHECK2(fcvtnu_2s(g), 0u, 2u, "fcvtnu.2s ties-to-even");
	CHECK2(fcvtau_2s(g), 0u, 3u, "fcvtau.2s ties-away");
	CHECK2(fcvtpu_2s(g), 0u, 3u, "fcvtpu.2s ceil");
	CHECK2(fcvtmu_2s(g), 0u, 2u, "fcvtmu.2s floor");

	/* Out of range saturates to the destination's extremes. */
	v2f big = {1e30f, -1e30f};
	CHECK2(fcvtzs_2s(big), INT32_MAX, INT32_MIN, "fcvtzs.2s saturates out of range");

	/* Double precision, 2D. */
	v2d dt = {2.5, -2.5};
	CHECK2(fcvtzs_2d(dt),  2, -2, "fcvtzs.2d ties (truncate)");
	CHECK2(fcvtns_2d(dt),  2, -2, "fcvtns.2d ties-to-even");
	CHECK2(fcvtps_2d(dt),  3, -2, "fcvtps.2d ceil");
	CHECK2(fcvtms_2d(dt),  2, -3, "fcvtms.2d floor");
	CHECK2(fcvtas_2d(dt),  3, -3, "fcvtas.2d ties-away");
	v2d dg = {-0.5, 2.9};
	CHECK2(fcvtzu_2d(dg), 0ull, 2ull, "fcvtzu.2d saturates a negative to 0");

	/* 4S is its own ISEL: all four lanes must be distinct. */
	v4f q = {2.5f, -2.5f, 3.5f, -3.5f};
	v4i qz = fcvtzs_4s(q), qm = fcvtms_4s(q);
	if (qz[0] != 2 || qz[1] != -2 || qz[2] != 3 || qz[3] != -3) {
		printf("FAIL fcvtzs.4s: got {%d,%d,%d,%d} want {2,-2,3,-3}\n",
		       qz[0], qz[1], qz[2], qz[3]);
		failures++;
	}
	if (qm[0] != 2 || qm[1] != -3 || qm[2] != 3 || qm[3] != -4) {
		printf("FAIL fcvtms.4s: got {%d,%d,%d,%d} want {2,-3,3,-4}\n",
		       qm[0], qm[1], qm[2], qm[3]);
		failures++;
	}

	if (failures == 0) printf("FCVTVEC-OK\n");
	return failures == 0 ? 0 : 1;
}
`

// vecNeighboursGuestSrc covers the vector conversions that were ALREADY
// implemented next to the ones patch 0041 adds -- FRINT{N,A,P,M,Z} and
// SCVTF/UCVTF in their ASIMD forms.
//
// They are here because the postgres closure executes them (four `frintm` and
// two `scvtf` vector sites) and nothing checked them. That is the dangerous
// shape: a missing instruction gives `_ecv_unreached`, which is loud, but a
// WRONG one is a silent bad answer. Having just written the FCVT vector
// semantics from the same templates, the cheapest way to find out whether the
// neighbours agree with the hardware is to ask.
const vecNeighboursGuestSrc = `#define _GNU_SOURCE
#include <stdint.h>
#include <stdio.h>
#include <string.h>

typedef float    v2f __attribute__((vector_size(8)));
typedef int32_t  v2i __attribute__((vector_size(8)));
typedef uint32_t v2u __attribute__((vector_size(8)));
typedef double   v2d __attribute__((vector_size(16)));

static int failures = 0;
/* The vector is evaluated ONCE into a local: these are inline-asm wrappers, and
   re-evaluating in the printf would report a different execution than the one
   that was compared -- which is exactly how a first version of this read "got
   {2,-2} want {2,-2}" and still failed. Bit patterns for the same reason: %g
   prints 2 for anything close enough to 2. */
#define CHECKF2(expr, w0, w1, what) do { \
	__typeof__(expr) g_ = (expr); \
	if (g_[0] != (w0) || g_[1] != (w1)) { \
		printf("FAIL %s: got {%.9g,%.9g} [%08llx,%08llx] want {%.9g,%.9g}\n", what, \
		       (double)g_[0], (double)g_[1], \
		       (unsigned long long)fbits(g_[0]), (unsigned long long)fbits(g_[1]), \
		       (double)(w0), (double)(w1)); \
		failures++; \
	} \
} while (0)

/* Raw bits of a float or double, whichever the lane holds. */
#define fbits(x) _Generic((x), \
	float: fbits32, double: fbits64, default: fbits64)(x)
static uint32_t fbits32(float v) { uint32_t b; memcpy(&b, &v, 4); return b; }
static uint64_t fbits64(double v) { uint64_t b; memcpy(&b, &v, 8); return b; }

#define DEF_F2S(name, insn) \
	static inline v2f name(v2f v) { v2f r; \
		__asm__(insn " %0.2s, %1.2s" : "=w"(r) : "w"(v)); return r; }
#define DEF_F2D(name, insn) \
	static inline v2d name(v2d v) { v2d r; \
		__asm__(insn " %0.2d, %1.2d" : "=w"(r) : "w"(v)); return r; }

DEF_F2S(frintn_2s, "frintn")
DEF_F2S(frinta_2s, "frinta")
DEF_F2S(frintp_2s, "frintp")
DEF_F2S(frintm_2s, "frintm")
DEF_F2S(frintz_2s, "frintz")
DEF_F2D(frintm_2d, "frintm")

static inline v2f scvtf_2s(v2i v) { v2f r;
	__asm__("scvtf %0.2s, %1.2s" : "=w"(r) : "w"(v)); return r; }
static inline v2f ucvtf_2s(v2u v) { v2f r;
	__asm__("ucvtf %0.2s, %1.2s" : "=w"(r) : "w"(v)); return r; }

int main(void) {
	/* FRINT* returns a FLOAT, so the ties are still the discriminating inputs
	   but the result stays in the float domain. */
	v2f t = {2.5f, -2.5f};
	CHECKF2(frintn_2s(t),  2.0f, -2.0f, "frintn.2s ties-to-even");
	CHECKF2(frinta_2s(t),  3.0f, -3.0f, "frinta.2s ties-away");
	CHECKF2(frintp_2s(t),  3.0f, -2.0f, "frintp.2s ceil");
	CHECKF2(frintm_2s(t),  2.0f, -3.0f, "frintm.2s floor");
	CHECKF2(frintz_2s(t),  2.0f, -2.0f, "frintz.2s truncate");

	v2f u = {3.5f, 0.5f};
	CHECKF2(frintn_2s(u), 4.0f, 0.0f, "frintn.2s ties-to-even both directions");

	v2f n = {2.1f, -2.9f};
	CHECKF2(frintp_2s(n),  3.0f, -2.0f, "frintp.2s non-tie");
	CHECKF2(frintm_2s(n),  2.0f, -3.0f, "frintm.2s non-tie");

	v2d dt = {2.5, -2.5};
	CHECKF2(frintm_2d(dt), 2.0, -3.0, "frintm.2d floor");

	/* Integer to float, both signednesses. -1 as unsigned is the case that
	   separates them: 4294967295, not -1. */
	v2i si = {-1, 3};
	CHECKF2(scvtf_2s(si), -1.0f, 3.0f, "scvtf.2s signed");
	v2u ui = {4294967295u, 3u};
	CHECKF2(ucvtf_2s(ui), 4294967296.0f, 3.0f, "ucvtf.2s unsigned");

	/* CONTROL 0, and the one that localises everything above. Build a vector
	   with no conversion at all and read lane 1 two ways on the SAME bytes:
	   umov (integer lane) and 'mov s0, v.s[1]' (scalar float lane, the DUP
	   element encoding). If only the float read is wrong, nothing above is a
	   conversion bug -- they all just READ their result through the broken
	   extraction. */
	{
		v2f raw = {1.5f, -9.25f};
		uint32_t lane1_umov;
		float lane1_dup;
		__asm__("umov %w0, %1.s[1]" : "=r"(lane1_umov) : "w"(raw));
		__asm__("mov %s0, %1.s[1]" : "=w"(lane1_dup) : "w"(raw));
		if (lane1_umov != 0xc1140000u) {
			printf("FAIL CONTROL umov lane1: got %08x want c1140000\n", lane1_umov);
			failures++;
		}
		if (fbits32(lane1_dup) != 0xc1140000u) {
			printf("FAIL CONTROL mov s,v.s[1] (DUP element): got %g [%08x] want -9.25 [c1140000]\n",
			       (double)lane1_dup, fbits32(lane1_dup));
			failures++;
		}
	}

	/* The B and H element-to-scalar forms, which patch 0043 routes to an S
	   destination rather than the kRegB/kRegH classes nothing else in the
	   decoder writes. Architecturally MOV <Bd>, <Vn>.B[i] sets Vd to the
	   zero-extended element, so reading the result back as a 32-bit lane is
	   the observable, and the top bits must be CLEAR -- an implementation that
	   left the rest of Vd alone would pass a check that only looked at the
	   low byte. */
	{
		typedef unsigned char v8b __attribute__((vector_size(8)));
		typedef unsigned short v4h __attribute__((vector_size(8)));
		v8b bytes = {0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88};
		v4h halves = {0x1111, 0x2222, 0x3333, 0x4444};
		float bd, hd;
		__asm__("mov %b0, %1.b[3]" : "=w"(bd) : "w"(bytes));
		__asm__("mov %h0, %1.h[2]" : "=w"(hd) : "w"(halves));
		if (fbits32(bd) != 0x44u) {
			printf("FAIL mov b,v.b[3]: got %08x want 00000044\n", fbits32(bd));
			failures++;
		}
		if (fbits32(hd) != 0x3333u) {
			printf("FAIL mov h,v.h[2]: got %08x want 00003333\n", fbits32(hd));
			failures++;
		}
	}

	/* CONTROL. Every check above produces a FLOAT vector; every check in the
	   FCVT guard produces an INTEGER vector and passes. If a plain float-vector
	   arithmetic op also loses lane 1, the fault is not in the conversions at
	   all but in returning a float vector from a vector semantic, which would
	   reach far past this file. */
	v2f a = {1.0f, 10.0f}, b = {0.25f, 0.5f};
	v2f sum; __asm__("fadd %0.2s, %1.2s, %2.2s" : "=w"(sum) : "w"(a), "w"(b));
	CHECKF2(sum, 1.25f, 10.5f, "CONTROL fadd.2s (plain float vector arithmetic)");
	v2f mul; __asm__("fmul %0.2s, %1.2s, %2.2s" : "=w"(mul) : "w"(a), "w"(b));
	CHECKF2(mul, 0.25f, 5.0f, "CONTROL fmul.2s");

	if (failures == 0) printf("VECNEIGH-OK\n");
	return failures == 0 ? 0 : 1;
}
`

// TestVectorRoundAndIntToFloatUnderEcvisor checks the vector FRINT/SCVTF/UCVTF
// forms the postgres closure executes. See vecNeighboursGuestSrc for why.
func TestVectorRoundAndIntToFloatUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "vecneigh", vecNeighboursGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "vecneigh")

	assertVecNeighGuestPassed(t, runWasm(t, ctx, wasm))
}

// TestVectorRoundAndIntToFloatNativeBaseline is the oracle for the above.
func TestVectorRoundAndIntToFloatNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "vecneigh", vecNeighboursGuestSrc)

	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/vecneigh")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertVecNeighGuestPassed(t, out)
}

func assertVecNeighGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "VECNEIGH-OK") {
		t.Errorf("guest did not reach VECNEIGH-OK; full output:\n%s", out)
	}
}

// byElementGuestSrc exercises the BY-ELEMENT vector multiplies.
//
// `fmul <Vd>.2S, <Vn>.2S, <Vm>.S[<idx>]` broadcasts ONE lane of the second
// operand across the multiply. It is a different encoding from the
// vector-by-vector form, and the earlier guard here tested only that one --
// which is how this slipped through twice.
//
// It matters because of where it sits. ICU computes a hash table's water marks
// as `(int32_t)(length * ratio)` for two ratios at once, and that compiles to
// exactly three instructions:
//
//	scvtf  s31, w3                    (float) length
//	fmul   v30.2s, v30.2s, v31.s[0]   both ratios * length
//	fcvtzs v30.2s, v30.2s             back to two int32s
//
// The third is the instruction that stopped initdb before patch 0041. If the
// SECOND is wrong, highWaterMark comes out wrong instead of missing: every
// insert then looks like an overflow, the table rehashes on every put, and
// primeIndex walks to the top of ICU's PRIMES table -- 18 doublings, ending in
// a 48 GB allocation and U_MEMORY_ALLOCATION_ERROR.
//
// The lane INDEX is the part to pin. An implementation that always broadcasts
// lane 0 passes any test whose index is 0, so both indices are checked with
// operands whose lanes differ.
const byElementGuestSrc = `#define _GNU_SOURCE
#include <stdint.h>
#include <stdio.h>
#include <string.h>

typedef float   v2f __attribute__((vector_size(8)));
typedef int32_t v2i __attribute__((vector_size(8)));
typedef float   v4f __attribute__((vector_size(16)));
typedef double  v2d __attribute__((vector_size(16)));

static int failures = 0;
static uint32_t fb(float v) { uint32_t b; memcpy(&b, &v, 4); return b; }
#define CHECK2(expr, w0, w1, what) do { \
	__typeof__(expr) g_ = (expr); \
	if (g_[0] != (w0) || g_[1] != (w1)) { \
		printf("FAIL %s: got {%g,%g} want {%g,%g}\n", what, \
		       (double) g_[0], (double) g_[1], (double) (w0), (double) (w1)); \
		failures++; \
	} \
} while (0)

/* Every constant vector is built by memcpy from a volatile array rather than
   written as an initialiser. Left to itself the compiler emits
   'fmov v1.2s, #1.0' -- the 2S vector-immediate form, which the lifter does not
   implement (patch 0010 covers only the 2D form) -- and the guest dies on that
   instead of testing what it is here to test. */
static volatile float FA[2] = {2.0f, 3.0f};
static volatile float FB[2] = {5.0f, 7.0f};
static volatile float FONE[2] = {1.0f, 1.0f};
static volatile float QA[4] = {1.0f, 2.0f, 3.0f, 4.0f};
static volatile float QB[4] = {10.0f, 20.0f, 30.0f, 40.0f};
static volatile double DA[2] = {1.5, 2.5};
static volatile double DB[2] = {4.0, 8.0};
static volatile float HALFZERO[2] = {0.5f, 0.0f};
static volatile float ELEM[2] = {32749.0f, 999.0f};

static v2f load2f(volatile float *p) { v2f v; float t[2] = {p[0], p[1]}; memcpy(&v, t, 8); return v; }
static v4f load4f(volatile float *p) { v4f v; float t[4] = {p[0], p[1], p[2], p[3]}; memcpy(&v, t, 16); return v; }
static v2d load2d(volatile double *p) { v2d v; double t[2] = {p[0], p[1]}; memcpy(&v, t, 16); return v; }

int main(void) {
	v2f a = load2f(FA);
	v2f b = load2f(FB);
	v2f r0, r1;
	__asm__("fmul %0.2s, %1.2s, %2.s[0]" : "=w"(r0) : "w"(a), "w"(b));
	__asm__("fmul %0.2s, %1.2s, %2.s[1]" : "=w"(r1) : "w"(a), "w"(b));
	CHECK2(r0, 10.0f, 15.0f, "fmul.2s by element [0]");
	CHECK2(r1, 14.0f, 21.0f, "fmul.2s by element [1]");

	/* 4S, and a lane that is neither the first nor the last. */
	v4f qa = load4f(QA);
	v4f qb = load4f(QB);
	v4f q2;
	__asm__("fmul %0.4s, %1.4s, %2.s[2]" : "=w"(q2) : "w"(qa), "w"(qb));
	if (q2[0] != 30.0f || q2[1] != 60.0f || q2[2] != 90.0f || q2[3] != 120.0f) {
		printf("FAIL fmul.4s by element [2]: got {%g,%g,%g,%g} want {30,60,90,120}\n",
		       (double) q2[0], (double) q2[1], (double) q2[2], (double) q2[3]);
		failures++;
	}

	/* Double precision, both lanes. */
	v2d da = load2d(DA), db = load2d(DB), d0, d1;
	__asm__("fmul %0.2d, %1.2d, %2.d[0]" : "=w"(d0) : "w"(da), "w"(db));
	__asm__("fmul %0.2d, %1.2d, %2.d[1]" : "=w"(d1) : "w"(da), "w"(db));
	CHECK2(d0, 6.0, 10.0, "fmul.2d by element [0]");
	CHECK2(d1, 12.0, 20.0, "fmul.2d by element [1]");

	/* FMLA by element: multiply-accumulate into the destination. */
	v2f acc = load2f(FONE);
	__asm__("fmla %0.2s, %1.2s, %2.s[1]" : "+w"(acc) : "w"(a), "w"(b));
	CHECK2(acc, 15.0f, 22.0f, "fmla.2s by element [1]");

	/* THE bug this file exists for: the element register of a by-element
	   multiply is five bits, M:Rm, and the decoder used Rm alone -- so
	   v31.s[0] read V15. Silent for exactly the reason it survived: a compiler
	   that allocates a LOW element register produces correct code, so the same
	   instruction is right or wrong depending on register allocation.
	   Both a scalar-written high register and a low one are checked, because
	   the low case is what made this invisible. */
	{
		static volatile int32_t len32 = 32749;
		v2f rv = load2f(HALFZERO);
		int32_t n = len32;

		v2f hi;
		__asm__("scvtf s31, %w1\n\tfmul %0.2s, %2.2s, v31.s[0]"
		        : "=w"(hi) : "r"(n), "w"(rv) : "v31");
		CHECK2(hi, 16374.5f, 0.0f, "by-element from a HIGH register (v31)");

		v2f lo;
		__asm__("scvtf s7, %w1\n\tfmul %0.2s, %2.2s, v7.s[0]"
		        : "=w"(lo) : "r"(n), "w"(rv) : "v7");
		CHECK2(lo, 16374.5f, 0.0f, "by-element from a LOW register (v7)");

		/* v16 is the first register that needs the M bit, so it is the exact
		   boundary the decoder got wrong. */
		v2f edge;
		__asm__("scvtf s16, %w1\n\tfmul %0.2s, %2.2s, v16.s[0]"
		        : "=w"(edge) : "r"(n), "w"(rv) : "v16");
		CHECK2(edge, 16374.5f, 0.0f, "by-element from v16, the M-bit boundary");

		/* FMLA takes the same fix; without it this reads V15 too. */
		v2f acc2 = load2f(HALFZERO);
		__asm__("scvtf s30, %w1\n\tfmla %0.2s, %2.2s, v30.s[0]"
		        : "+w"(acc2) : "r"(n), "w"(rv) : "v30");
		CHECK2(acc2, 16375.0f, 0.0f, "fmla by element from a high register (v30)");
	}

	/* The exact ICU shape: (int32_t)(length * ratio) for two ratios at once. */
	{
		int32_t length = 32749;              /* PRIMES[11], where the runaway began */
		static volatile float ratios[2] = {0.5f, 0.0f}; /* ICU's water ratios */
		v2f rv = load2f(ratios);
		v2i marks;
		__asm__("scvtf s31, %w1\n\t"
		        "fmul %0.2s, %2.2s, v31.s[0]\n\t"
		        "fcvtzs %0.2s, %0.2s"
		        : "=w"(marks) : "r"(length), "w"(rv) : "v31");
		if (marks[0] != 16374 || marks[1] != 0) {
			printf("FAIL water marks: got {%d,%d} want {16374,0} -- a zero high "
			       "water mark makes every hash insert look like an overflow\n",
			       marks[0], marks[1]);
			failures++;
		}
	}

	(void) fb;
	if (failures == 0) printf("BYELEM-OK\n");
	return failures == 0 ? 0 : 1;
}
`

// TestVectorByElementMultiplyUnderEcvisor guards FMUL/FMLA by element. See
// byElementGuestSrc for why the lane index is the load-bearing part.
func TestVectorByElementMultiplyUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "byelem", byElementGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "byelem")
	assertByElemGuestPassed(t, runWasm(t, ctx, wasm))
}

// TestVectorByElementMultiplyNativeBaseline is the oracle.
func TestVectorByElementMultiplyNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "byelem", byElementGuestSrc)

	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/byelem")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertByElemGuestPassed(t, out)
}

func assertByElemGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "BYELEM-OK") {
		t.Errorf("guest did not reach BYELEM-OK; full output:\n%s", out)
	}
}

// TestFCVTVectorUnderEcvisor guards the ASIMD FCVT{Z,N,P,M,A}{S,U} family.
func TestFCVTVectorUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "fcvtvec", fcvtVecGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "fcvtvec")

	out := runWasm(t, ctx, wasm)
	assertFCVTVecGuestPassed(t, out)
}

// TestFCVTVectorNativeBaseline runs the same guest on the host CPU, which
// executes the real instructions and is therefore the oracle for every
// expectation above.
func TestFCVTVectorNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "fcvtvec", fcvtVecGuestSrc)

	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/fcvtvec")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertFCVTVecGuestPassed(t, out)
}

func assertFCVTVecGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "FCVTVEC-OK") {
		t.Errorf("guest did not reach FCVTVEC-OK; full output:\n%s", out)
	}
}
