// Copyright 2026 The raptormark Authors
// SPDX-License-Identifier: LGPL-2.1-or-later

package decode

import (
	"testing"
)

// TestSVEValidates is the same faithfulness check TestA64Validates makes, on a
// second, independent 2,097-line file.
//
// It is worth having twice. a64.decode and sve.decode were written years apart
// and use the grammar differently: sve.decode has 26 group-delimiter lines and
// 156 continuations against a64.decode's 4 and 33. A parser bug that a64.decode
// happens not to exercise therefore gets a second chance to show up here, and
// this file's 929 patterns are as mutually exclusive as a64's 1,161.
//
// Per-table on purpose: see Decoder.Validate. a64 and sve are separate decoders
// and are ALLOWED to overlap each other.
func TestSVEValidates(t *testing.T) {
	tab, err := SVE()
	if err != nil {
		t.Fatal(err)
	}
	probs := tab.Validate()
	for i, p := range probs {
		if i >= 20 {
			t.Errorf("... and %d more", len(probs)-20)
			break
		}
		t.Errorf("%s", p)
	}
	if len(probs) != 0 {
		t.Errorf("%d problems in the vendored sve.decode", len(probs))
	}
}

// TestSVECarriesTheMeasuredGapFamilies guards a re-vendor.
//
// These are exactly the families the corpus differential found objdump naming
// and the a64-only table missing, across bash-glibc, aptget-glibc and
// postgres-glibc. They are glibc's SVE ifunc variants of the string and memory
// routines. A pin that lost any of them would silently reopen that gap.
func TestSVECarriesTheMeasuredGapFamilies(t *testing.T) {
	tab, err := SVE()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, n := range tab.Names() {
		have[n] = true
	}
	for _, want := range []string{
		"LD_zprr", "ST_zprr", // ld1b/ld1w/st1b/st1w with a register offset
		"WHILE_lt",         // whilelo
		"PTRUE",            // ptrue
		"CNT_r",            // cntb/cntd
		"XAR",              // xar
		"ZIP1_z", "ZIP2_z", // zip1/zip2
		"ADD_zzz", "EOR_zzz", // SVE add/eor
		"LSL_zpzi", "LSR_zpzi", // SVE predicated shifts
		"SEL_zpzz", // what `mov zN, pM/m, zK` really is
		"DUP_i", "INDEX_ii",
	} {
		if !have[want] {
			t.Errorf("vendored sve.decode has no %s pattern; is the pin too old?", want)
		}
	}
}

// TestSVEDecodesRealEncodings pins the SVE half of the oracle against objdump.
//
// Every encoding was produced by assembling the listed mnemonic with GNU as
// (`.arch armv8.2-a+sve2`) and reading it back with objdump 2.42 on this
// aarch64 host. Two of them -- 0x04b831ad and 0x04ab014a -- are also verbatim
// gap examples the corpus differential reported from postgres-glibc, which is
// what confirms these families are the ones that were actually missing.
//
// Matched through AArch64() rather than SVE() on purpose: that exercises the
// real dispatch order and proves a64.decode does not shadow any of them.
func TestSVEDecodesRealEncodings(t *testing.T) {
	dec, err := AArch64()
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		enc  uint32
		asm  string
		pat  string
		want map[string]int64
	}{
		{0x2598e100, "ptrue p0.s, vl8", "PTRUE",
			map[string]int64{"esz": 2, "pat": 8, "rd": 0}},
		{0x25211c00, "whilelo p0.b, x0, x1", "WHILE_lt",
			map[string]int64{"esz": 0, "rn": 0, "rm": 1, "sf": 1, "u": 1, "eq": 0}},

		// CNT_r's element size is the whole difference between cntb and cntd,
		// and its `imm` runs through !function=plus_1 -- raw 0 means one.
		{0x0420e3e7, "cntb x7", "CNT_r",
			map[string]int64{"esz": 0, "rd": 7, "imm": 1, "pat": 31}},
		{0x04e0e3e0, "cntd x0", "CNT_r",
			map[string]int64{"esz": 3, "rd": 0, "imm": 1}},

		// Single-register loads take dtype as a plain 4-bit field via
		// @rprr_load_dt. Nothing here exercises !function=msz_dtype -- see the
		// multi-register block below, which does.
		{0xa4044024, "ld1b {z4.b}, p0/z, [x1, x4]", "LD_zprr",
			map[string]int64{"dtype": 0, "rd": 4, "rn": 1, "rm": 4, "pg": 0}},
		{0xa5454036, "ld1w {z22.s}, p0/z, [x1, x5, lsl #2]", "LD_zprr",
			map[string]int64{"dtype": 10, "rd": 22, "rn": 1, "rm": 5}},
		{0xe40c4080, "st1b {z0.b}, p0, [x4, x12]", "ST_zprr",
			map[string]int64{"esz": 0, "msz": 0, "rd": 0, "rn": 4, "rm": 12}},

		// The MULTI-register loads (@rprr_load_msz) are the only users of
		// !function=msz_dtype, which is a lookup table {0,5,10,15,18} indexed
		// by msz. All four sizes are here because identity would give
		// 0/1/2/3 where the answer is 0/5/10/15 -- msz=0 coincides, so a test
		// with only ld2b would pass against a broken implementation.
		//
		// Added after neutralizing msz_dtype: the earlier cases above named it
		// in a comment but did not exercise it, and the identity substitution
		// failed only the unit test of the function table. Second time this
		// pass has caught a test asserting the wrong thing.
		{0xa421c000, "ld2b {z0.b-z1.b}, p0/z, [x0, x1]", "LD_zprr",
			map[string]int64{"dtype": 0, "nreg": 1, "rd": 0, "rn": 0, "rm": 1}},
		{0xa4a3c442, "ld2h {z2.h-z3.h}, p1/z, [x2, x3, lsl #1]", "LD_zprr",
			map[string]int64{"dtype": 5, "nreg": 1, "rd": 2, "rn": 2, "pg": 1}},
		{0xa525c884, "ld2w {z4.s-z5.s}, p2/z, [x4, x5, lsl #2]", "LD_zprr",
			map[string]int64{"dtype": 10, "nreg": 1, "rd": 4, "pg": 2}},
		{0xa5a7ccc6, "ld2d {z6.d-z7.d}, p3/z, [x6, x7, lsl #3]", "LD_zprr",
			map[string]int64{"dtype": 15, "nreg": 1, "rd": 6, "pg": 3}},
		{0xa5e9d108, "ld4d {z8.d-z11.d}, p4/z, [x8, x9, lsl #3]", "LD_zprr",
			map[string]int64{"dtype": 15, "nreg": 3, "rd": 8, "pg": 4}},

		{0x05b4624a, "zip1 z10.s, z18.s, z20.s", "ZIP1_z",
			map[string]int64{"esz": 2, "rd": 10, "rn": 18, "rm": 20}},
		{0x056e6554, "zip2 z20.h, z10.h, z14.h", "ZIP2_z",
			map[string]int64{"esz": 1, "rd": 20, "rn": 10, "rm": 14}},
		{0x04b831ad, "eor z13.d, z13.d, z24.d", "EOR_zzz",
			map[string]int64{"rd": 13, "rn": 13, "rm": 24}},
		{0x04ab014a, "add z10.s, z10.s, z11.s", "ADD_zzz",
			map[string]int64{"esz": 2, "rd": 10, "rn": 10, "rm": 11}},
		{0x04a14000, "index z0.s, #0, #1", "INDEX_ii",
			map[string]int64{"esz": 2, "rd": 0, "imm1": 0, "imm2": 1}},

		// The tszimm family. esz and the shift amount BOTH come out of one
		// tsz:imm3 field, by different functions.
		//
		// These two are the sharpest pair available: the raw fields are 13 and
		// 52, and only the correct rsub-style arithmetic turns them into the
		// #3 and #12 objdump prints. Swapping tszimm_shr for tszimm_shl gives
		// 5 and 20 instead.
		{0x042d34e9, "xar z9.b, z9.b, z7.b, #3", "XAR",
			map[string]int64{"esz": 0, "imm": 3, "rd": 9, "rn": 9, "rm": 7}},
		{0x04419689, "lsr z9.s, p5/m, z9.s, #12", "LSR_zpzi",
			map[string]int64{"esz": 2, "imm": 12, "rd": 9, "rn": 9, "pg": 5}},
		// LSL takes the OTHER function on a DIFFERENT raw value (44), and
		// arrives at the same 12. A swap gives 20 here too, so the pair cannot
		// coincide with the right answer.
		{0x04439d93, "lsl z19.s, p7/m, z19.s, #12", "LSL_zpzi",
			map[string]int64{"esz": 2, "imm": 12, "rd": 19, "rn": 19, "pg": 7}},

		// `mov zN, pM/m, zK` is an ALIAS of SEL. objdump prints the alias; the
		// decodetree pattern is the decode. This is the SVE instance of the
		// alias trap TODO.md records.
		{0x05b1fb91, "mov z17.s, p14/m, z28.s", "SEL_zpzz",
			map[string]int64{"esz": 2, "rd": 17, "rn": 28, "pg": 14}},

		// expand_imm_sh8s/u. The cast is to EIGHT bits while the shift flag is
		// bit 8, outside them -- 383 is the case that catches an implementation
		// that truncates before testing the flag: it must give 127<<8 = 32512,
		// not 127 and not -1<<8.
		{0x2578d000, "mov z0.h, #-128 (DUP_i)", "DUP_i",
			map[string]int64{"esz": 1, "rd": 0, "imm": -128}},
		{0x2578efe1, "mov z1.h, #32512 (DUP_i)", "DUP_i",
			map[string]int64{"esz": 1, "rd": 1, "imm": 32512}},
		{0x2560dfe2, "add z2.h, z2.h, #255", "ADD_zzi",
			map[string]int64{"esz": 1, "rd": 2, "imm": 255}},
		{0x2560e023, "add z3.h, z3.h, #256", "ADD_zzi",
			map[string]int64{"esz": 1, "rd": 3, "imm": 256}},
		{0x2523d904, "subr z4.b, z4.b, #200", "SUBR_zzi",
			map[string]int64{"esz": 0, "rd": 4, "imm": 200}},
	} {
		m := dec.Match(c.enc)
		if m == nil {
			t.Errorf("%#08x (%s): no match", c.enc, c.asm)
			continue
		}
		if m.Insn.Name != c.pat {
			t.Errorf("%#08x (%s): matched %s from %s, want %s\n%s",
				c.enc, c.asm, m.Insn.Name, m.Insn.Source, c.pat, m.Describe())
			continue
		}
		if m.Insn.Source != "sve.decode" {
			t.Errorf("%#08x (%s): matched in %s, want sve.decode -- a64 is shadowing it",
				c.enc, c.asm, m.Insn.Source)
		}
		for name, want := range c.want {
			if got := val(t, m, name); got != want {
				t.Errorf("%#08x (%s): %s = %d, want %d\n%s", c.enc, c.asm, name, got, want, m.Describe())
			}
		}
	}
}

func TestSVEFieldFuncsMatchQEMU(t *testing.T) {
	// Transcribed from target/arm/tcg/translate-sve.c and translate.h at the
	// vendored pin; see PROVENANCE.md.
	for _, c := range []struct {
		fn   string
		in   int64
		want int64
	}{
		{"plus_8", 0, 8},
		{"plus_12", 1, 13},
		{"times_2", 3, 6},

		// tszimm_esz: discard imm3, then the index of the top set bit.
		{"tszimm_esz", 13, 0}, // 13>>3 = 1
		{"tszimm_esz", 44, 2}, // 44>>3 = 5
		{"tszimm_esz", 52, 2}, // 52>>3 = 6
		{"tszimm_esz", 0, -1}, // clz32(0) == 32, so 31-32
		{"tszimm_esz", 7, -1}, // 7>>3 == 0, likewise
		{"tszimm_esz", 64, 3}, // 64>>3 = 8

		// (16 << esz) - x, and x - (8 << esz). Both return esz unchanged when
		// it is negative, which is QEMU's own behaviour and not a clamp.
		{"tszimm_shr", 13, 3},
		{"tszimm_shr", 52, 12},
		{"tszimm_shr", 0, -1},
		{"tszimm_shl", 44, 12},
		{"tszimm_shl", 13, 5},
		{"tszimm_shl", 0, -1},

		// The 8-bit cast happens BEFORE the shift, and the shift flag is bit 8.
		{"expand_imm_sh8s", 128, -128},  // int8(0x80), no shift
		{"expand_imm_sh8s", 383, 32512}, // int8(0x7f)=127, bit 8 set: 127<<8
		{"expand_imm_sh8s", 256, 0},     // int8(0x00)=0, shifted, still 0
		{"expand_imm_sh8u", 255, 255},
		{"expand_imm_sh8u", 257, 256},   // uint8(0x01)=1, bit 8 set: 1<<8
		{"expand_imm_sh8u", 384, 32768}, // uint8(0x80)=128, bit 8 set

		// dtype[] = {0, 5, 10, 15, 18}
		{"msz_dtype", 0, 0},
		{"msz_dtype", 1, 5},
		{"msz_dtype", 2, 10},
		{"msz_dtype", 3, 15},
		{"msz_dtype", 4, 18},
	} {
		f, ok := fieldFuncs[c.fn]
		if !ok {
			t.Errorf("%s not implemented", c.fn)
			continue
		}
		if got := f(c.in); got != c.want {
			t.Errorf("%s(%d) = %d, want %d", c.fn, c.in, got, c.want)
		}
	}
}

// TestDispatchOrderIsEarliestTableWins documents and enforces the rule that
// makes two tables safe to hold at once.
//
// QEMU runs `!disas_a64() && !disas_sme() && !disas_sve()`
// (translate-a64.c:11200), so where two tables both match, the EARLIER one is
// the answer. Tested with synthetic tables rather than by hunting for a real
// a64/sve collision: the rule must hold whether or not the current pin happens
// to contain one.
func TestDispatchOrderIsEarliestTableWins(t *testing.T) {
	first, err := Parse("first.decode", "FIRST  1111 -------- -------- -------- rd:4\n")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Parse("second.decode", "SECOND 1111 -------- -------- -------- rd:4\n")
	if err != nil {
		t.Fatal(err)
	}

	// The two overlap completely, which is legal ACROSS tables.
	d := &Decoder{Tables: []*Table{first, second}}
	m := d.Match(0xf0000000)
	if m == nil {
		t.Fatal("no match")
	}
	if m.Insn.Name != "FIRST" || m.Insn.Source != "first.decode" {
		t.Errorf("matched %s from %s, want FIRST from first.decode", m.Insn.Name, m.Insn.Source)
	}

	// Reversed, the other one wins: the order is the whole mechanism.
	d = &Decoder{Tables: []*Table{second, first}}
	if m := d.Match(0xf0000000); m == nil || m.Insn.Name != "SECOND" {
		t.Errorf("reversed: got %v, want SECOND", m)
	}

	// And a cross-table overlap must NOT be reported as a problem, or every
	// real re-vendor would fail validation.
	if probs := (&Decoder{Tables: []*Table{first, second}}).Validate(); len(probs) != 0 {
		for _, p := range probs {
			t.Errorf("cross-table overlap wrongly reported: %s", p)
		}
	}
}

// TestAArch64OrderMatchesQEMU is a shape check on the shipped decoder.
func TestAArch64OrderMatchesQEMU(t *testing.T) {
	d, err := AArch64()
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Tables) != 2 {
		t.Fatalf("got %d tables, want 2", len(d.Tables))
	}
	for i, want := range []string{"a64.decode", "sve.decode"} {
		if len(d.Tables[i].Insns) == 0 {
			t.Fatalf("table %d is empty", i)
		}
		if got := d.Tables[i].Insns[0].Source; got != want {
			t.Errorf("table %d is %s, want %s (QEMU tries a64 before sve)", i, got, want)
		}
	}
}
