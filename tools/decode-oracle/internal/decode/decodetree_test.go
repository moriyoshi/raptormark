// Copyright 2026 The raptormark Authors
// SPDX-License-Identifier: LGPL-2.1-or-later

package decode

import (
	"fmt"
	"strings"
	"testing"
)

// val returns the decoded value of one operand, failing if it is absent.
func val(t *testing.T, m *Match, name string) int64 {
	t.Helper()
	if m == nil {
		t.Fatalf("no match, wanted operand %q", name)
	}
	for _, v := range m.Values {
		if v.Name == name {
			if v.Unresolved {
				t.Fatalf("operand %q is unresolved (!function=%s not implemented)", name, v.Fn)
			}
			return v.Value
		}
	}
	t.Fatalf("pattern %s has no operand %q; has %s", m.Insn.Name, name, m.Describe())
	return 0
}

// ---------------------------------------------------------------------------
// Parser unit tests, over inline snippets.
// ---------------------------------------------------------------------------

// ldstMultSnippet is the Advanced SIMD load/store-multiple block, copied from
// a64.decode. It is the fixture that matters most for constants: `rpt` and
// `selem` exist ONLY as const_elts, and they are the whole reason this family
// collapses from the "74 distinct post-index forms" TODO.md counts by hand into
// seven lines.
const ldstMultSnippet = `
&ldst_mult      rm rn rt sz q p rpt selem
@ldst_mult      . q:1 ...... p:1 . . rm:5 .... sz:2 rn:5 rt:5 &ldst_mult
ST_mult         0 . 001100 . 0 0 ..... 0000 .. ..... ..... @ldst_mult rpt=1 selem=4
ST_mult         0 . 001100 . 0 0 ..... 0010 .. ..... ..... @ldst_mult rpt=4 selem=1
ST_mult         0 . 001100 . 0 0 ..... 0100 .. ..... ..... @ldst_mult rpt=1 selem=3
ST_mult         0 . 001100 . 0 0 ..... 0110 .. ..... ..... @ldst_mult rpt=3 selem=1
ST_mult         0 . 001100 . 0 0 ..... 0111 .. ..... ..... @ldst_mult rpt=1 selem=1
ST_mult         0 . 001100 . 0 0 ..... 1000 .. ..... ..... @ldst_mult rpt=1 selem=2
ST_mult         0 . 001100 . 0 0 ..... 1010 .. ..... ..... @ldst_mult rpt=2 selem=1
`

func TestParseFormatAndConstants(t *testing.T) {
	tab, err := Parse("snippet", ldstMultSnippet)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(tab.Insns), 7; got != want {
		t.Fatalf("parsed %d patterns, want %d", got, want)
	}

	// Every bit must be accounted for, and the seven patterns must be mutually
	// exclusive. If the format overlay were dropped this still passes, so it is
	// a floor and not the real check -- see TestShiftInsertFormatDisambiguation.
	if probs := tab.Validate(); len(probs) != 0 {
		for _, p := range probs {
			t.Errorf("%s", p)
		}
	}

	// st1 {v0.16b}, [x0], #16  -- objdump 2.42 on aarch64.
	m := tab.Match(0x4c9f7000)
	if m == nil {
		t.Fatal("0x4c9f7000 did not match ST_mult")
	}
	if m.Insn.Name != "ST_mult" {
		t.Errorf("matched %s, want ST_mult", m.Insn.Name)
	}
	// q comes from the FORMAT, opcode-derived rpt/selem from the PATTERN, and
	// rm=31 is what makes this the immediate-offset post-index form.
	for _, c := range []struct {
		name string
		want int64
	}{
		{"q", 1}, {"p", 1}, {"rm", 31}, {"sz", 0}, {"rn", 0}, {"rt", 0},
		{"rpt", 1}, {"selem", 1},
	} {
		if got := val(t, m, c.name); got != c.want {
			t.Errorf("0x4c9f7000 %s = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestShiftInsertFormatDisambiguation is the load-bearing parser test.
//
// The four SLI_v pattern lines are BYTE-IDENTICAL. Everything that separates an
// 8-bit lane from a 64-bit one lives in the @q_shli_* format they name. A
// matcher that ignores format fixed bits matches all four encodings against the
// first line and reports esz=0 for every one of them -- a plausible, wrong,
// silent answer, which is the exact failure mode this package exists to remove.
func TestShiftInsertFormatDisambiguation(t *testing.T) {
	tab, err := A64()
	if err != nil {
		t.Fatal(err)
	}

	// objdump 2.42, aarch64 host. One per lane width, with DIFFERENT shift
	// amounts so a matcher that returns the right esz but the wrong imm cannot
	// coincide with the right answer.
	for _, c := range []struct {
		enc      uint32
		asm      string
		pat      string
		esz, imm int64
	}{
		{0x6f0b5420, "sli v0.16b, v1.16b, #3", "SLI_v", 0, 3},
		{0x6f155420, "sli v0.8h, v1.8h, #5", "SLI_v", 1, 5},
		{0x6f315420, "sli v0.4s, v1.4s, #17", "SLI_v", 2, 17},
		{0x6f615420, "sli v0.2d, v1.2d, #33", "SLI_v", 3, 33},
		// SRI's immediate runs through %neon_rshift_iN !function=rsub_N, so
		// these also check that the right rsub width is selected: the raw bits
		// are 5, 11, 15 and 31, and only the correct rsub_N turns each into the
		// shift objdump prints.
		//
		// ALL FOUR lane widths are here on purpose. An earlier version tested
		// only b and d; substituting rsub_64 for rsub_32 -- a plausible
		// mis-transcription -- then failed no behavioral test at all, only the
		// unit test of the function table. Found by neutralizing rsub_32.
		{0x6f0d4420, "sri v0.16b, v1.16b, #3", "SRI_v", 0, 3},
		{0x6f1b4420, "sri v0.8h, v1.8h, #5", "SRI_v", 1, 5},
		{0x6f2f4420, "sri v0.4s, v1.4s, #17", "SRI_v", 2, 17},
		{0x6f5f4420, "sri v0.2d, v1.2d, #33", "SRI_v", 3, 33},
		// Scalar forms: no q operand, esz always 3.
		{0x7f615420, "sli d0, d1, #33", "SLI_s", 3, 33},
		{0x7f5f4420, "sri d0, d1, #33", "SRI_s", 3, 33},
	} {
		m := tab.Match(c.enc)
		if m == nil {
			t.Errorf("%#08x (%s): no match", c.enc, c.asm)
			continue
		}
		if m.Insn.Name != c.pat {
			t.Errorf("%#08x (%s): matched %s, want %s", c.enc, c.asm, m.Insn.Name, c.pat)
			continue
		}
		if got := val(t, m, "esz"); got != c.esz {
			t.Errorf("%#08x (%s): esz = %d, want %d", c.enc, c.asm, got, c.esz)
		}
		if got := val(t, m, "imm"); got != c.imm {
			t.Errorf("%#08x (%s): imm = %d, want %d", c.enc, c.asm, got, c.imm)
		}
		if got := val(t, m, "rn"); got != 1 {
			t.Errorf("%#08x (%s): rn = %d, want 1", c.enc, c.asm, got)
		}
	}
}

func TestFieldConcatenationAndSignExtension(t *testing.T) {
	// %disp12 0:s1 1:1 2:10 is the worked example in decodetree.rst:
	//   sextract(i,0,1) << 11 | extract(i,1,1) << 10 | extract(i,2,10)
	src := `
%disp12  0:s1 1:1 2:10
%plain   16:3
&d       d p
@f       ........ ........ ........ ........ &d d=%disp12 p=%plain
T        ........ ........ ........ ........ @f
`
	tab, err := Parse("snippet", src)
	if err != nil {
		t.Fatal(err)
	}
	// bit0 = 1 -> sextract(...,1) = -1, contributing -1<<11 = -2048.
	// bit1 = 0. bits 11:2 = 0x155 -> 341.
	const enc = 0x0000_0555 // 0b0101_0101_0101
	m := tab.Match(enc)
	if got, want := val(t, m, "d"), int64(-2048+341); got != want {
		t.Errorf("d = %d, want %d", got, want)
	}
	if got, want := val(t, m, "p"), int64((enc>>16)&7); got != want {
		t.Errorf("p = %d, want %d", got, want)
	}
}

func TestNamedFieldReference(t *testing.T) {
	// decodetree.rst's named_field example shape, and the one real use in
	// a64.decode: %uimm_scaled reads `sz`, which the PATTERN supplies -- as a
	// field in one pattern and as a constant in another. Both must work.
	src := `
%uimm_scaled   10:12 sz:3 !function=uimm_scaled
&li            rn rt imm
@ldst_uimm     .. ... . .. .. ............ rn:5 rt:5 &li imm=%uimm_scaled
FromField      sz:2 111 0 01 00 ............ ..... ..... @ldst_uimm
FromConst      11 111 0 01 01 ............ ..... ..... @ldst_uimm sz=3
`
	tab, err := Parse("snippet", src)
	if err != nil {
		t.Fatal(err)
	}

	// uimm_scaled(x) = (x >> 3) << (x & 7), where x = imm12:sz(3 bits).
	// Choose imm12 = 8 and sz = 2, so x = 8<<3 | 2 = 66, giving 8 << 2 = 32.
	// A matcher that ignored the named field would read sz as 0 and return 8 --
	// a different number, so this discriminates.
	encField := uint32(0)<<30 | 0b111<<27 | 0<<26 | 0b01<<24 | 0b00<<22 | 8<<10
	encField |= 0b10 << 30 // sz = 2
	m := tab.Match(encField)
	if m == nil || m.Insn.Name != "FromField" {
		t.Fatalf("FromField did not match: %v", m)
	}
	if got, want := val(t, m, "imm"), int64(32); got != want {
		t.Errorf("imm from field sz: got %d, want %d", got, want)
	}

	// sz supplied as a constant 3: x = 8<<3 | 3 = 67, giving 8 << 3 = 64.
	encConst := uint32(0b11)<<30 | 0b111<<27 | 0<<26 | 0b01<<24 | 0b01<<22 | 8<<10
	m = tab.Match(encConst)
	if m == nil || m.Insn.Name != "FromConst" {
		t.Fatalf("FromConst did not match: %v", m)
	}
	if got, want := val(t, m, "imm"), int64(64); got != want {
		t.Errorf("imm from const sz: got %d, want %d", got, want)
	}
}

func TestOverlapGroupPrefersSpecificPattern(t *testing.T) {
	// The Hint group's shape: specific hints inside a no-overlap group, with a
	// catch-all NOP after it, all inside one overlap group.
	src := `
{
  [
    YIELD       1101 0101 0000 0011 0010 0000 001 11111
    WFE         1101 0101 0000 0011 0010 0000 010 11111
  ]
  NOP           1101 0101 0000 0011 0010 ---- --- 11111
}
`
	tab, err := Parse("snippet", src)
	if err != nil {
		t.Fatal(err)
	}
	// Every pattern here is legally overlapping, so Validate must stay quiet.
	if probs := tab.Validate(); len(probs) != 0 {
		for _, p := range probs {
			t.Errorf("unexpected: %s", p)
		}
	}
	if m := tab.Match(0xd503203f); m == nil || m.Insn.Name != "YIELD" {
		t.Errorf("yield: got %v, want YIELD", m)
	}
	if m := tab.Match(0xd503205f); m == nil || m.Insn.Name != "WFE" {
		t.Errorf("wfe: got %v, want WFE", m)
	}
	// The canonical NOP falls through to the catch-all.
	if m := tab.Match(0xd503201f); m == nil || m.Insn.Name != "NOP" {
		t.Errorf("nop: got %v, want NOP", m)
	}
}

func TestParseRejectsMalformedInput(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"short pattern", "T  0000 0000"},
		{"unknown format", "T  ........ ........ ........ ........ @nope"},
		{"unknown field", "@f ........ ........ ........ ........ x=%nope\nT ........ ........ ........ ........ @f"},
		{"unclosed group", "{\n  T  ........ ........ ........ ........\n"},
		{"stray close", "}"},
		{"duplicate field", "%a 0:5\n%a 0:5"},
		{"field off the end", "%a 30:5"},
	} {
		if _, err := Parse(c.name, c.src); err == nil {
			t.Errorf("%s: parsed without error, want a diagnostic", c.name)
		}
	}
}

func TestCommentsAndContinuations(t *testing.T) {
	src := `
# a leading comment
&a  rn rd scale
@f  .. ...... p:1 .. rm:5 ...... rn:5 rd:5 \
    &a scale=3     # trailing comment on a continued line
T   00 000000 . 00 ..... ...... ..... .....  @f
`
	tab, err := Parse("snippet", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(tab.Insns) != 1 {
		t.Fatalf("got %d patterns, want 1", len(tab.Insns))
	}
	m := tab.Match(0x00800020)
	if m == nil {
		t.Fatal("no match")
	}
	if got := val(t, m, "scale"); got != 3 {
		t.Errorf("scale = %d, want 3 (constant lost across the continuation)", got)
	}
	if got := val(t, m, "p"); got != 1 {
		t.Errorf("p = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// Tests against the real vendored table.
// ---------------------------------------------------------------------------

// TestA64Validates is the faithfulness check on the parser.
//
// decodetree GUARANTEES that patterns outside an overlap group are mutually
// exclusive, and that every pattern defines all 32 bits. Both properties are
// destroyed by a mis-implemented format overlay. If this is green, the linear
// first-match-wins scan in Match agrees with the decision tree QEMU generates.
func TestA64Validates(t *testing.T) {
	tab, err := A64()
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
		t.Errorf("%d problems in the vendored a64.decode", len(probs))
	}
}

// TestA64CarriesTheHeadFamilies guards a re-vendor.
//
// The whole point of pinning a recent QEMU is that these families are in the
// decodetree rather than in translate-a64.c's legacy path. A re-vendor to an
// older tag would not fail to parse -- it would just quietly stop naming the
// encodings raptormark owes coverage on. Per .agents/docs/TODO.md the head of
// the debt is st1 (736 sites), tbl (706) and sli (535).
func TestA64CarriesTheHeadFamilies(t *testing.T) {
	tab, err := A64()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, n := range tab.Names() {
		have[n] = true
	}
	for _, want := range []string{
		"ST_mult", "LD_mult", // st1/st2/st3/st4 multiple-structures
		"ST_single", "LD_single", "LD_single_repl", // single-structure and replicating
		"TBL_TBX",                          // tbl/tbx
		"SLI_v", "SRI_v", "SLI_s", "SRI_s", // shift-and-insert
		"CNT_v", // popcount, patch 0055's family
	} {
		if !have[want] {
			t.Errorf("vendored a64.decode has no %s pattern; is the pin too old?", want)
		}
	}
}

// TestA64DecodesRealEncodings pins the oracle against objdump.
//
// Every encoding here was produced by assembling the listed mnemonic with GNU
// as and reading it back with objdump 2.42 on this aarch64 host, per AGENTS.md:
// verify against a real encoding and put that encoding in the test.
func TestA64DecodesRealEncodings(t *testing.T) {
	tab, err := A64()
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		enc  uint32
		asm  string
		pat  string
		want map[string]int64
	}{
		// Load/store multiple structures. rpt/selem are the opcode-derived
		// constants, and they are what distinguishes st1-of-4-registers from
		// st4-of-one-register: both are "four registers' worth".
		{0x4c007000, "st1 {v0.16b}, [x0]", "ST_mult",
			map[string]int64{"q": 1, "p": 0, "rpt": 1, "selem": 1, "sz": 0, "rn": 0}},
		{0x4c00a020, "st1 {v0.16b-v1.16b}, [x1]", "ST_mult",
			map[string]int64{"rpt": 2, "selem": 1, "rn": 1}},
		{0x4c006842, "st1 {v2.4s-v4.4s}, [x2]", "ST_mult",
			map[string]int64{"rpt": 3, "selem": 1, "sz": 2, "rn": 2, "rt": 2}},
		{0x4c002460, "st1 {v0.8h-v3.8h}, [x3]", "ST_mult",
			map[string]int64{"rpt": 4, "selem": 1, "sz": 1, "rn": 3}},
		{0x4c9f7000, "st1 {v0.16b}, [x0], #16", "ST_mult",
			map[string]int64{"p": 1, "rm": 31, "rpt": 1, "selem": 1}},
		{0x4c857000, "st1 {v0.16b}, [x0], x5", "ST_mult",
			map[string]int64{"p": 1, "rm": 5, "rpt": 1, "selem": 1}},
		{0x4c008800, "st2 {v0.4s-v1.4s}, [x0]", "ST_mult",
			map[string]int64{"rpt": 1, "selem": 2, "sz": 2}},
		{0x4c000c00, "st4 {v0.2d-v3.2d}, [x0]", "ST_mult",
			map[string]int64{"rpt": 1, "selem": 4, "sz": 3}},
		{0x4c407000, "ld1 {v0.16b}, [x0]", "LD_mult",
			map[string]int64{"rpt": 1, "selem": 1}},
		{0x4cdf0400, "ld4 {v0.8h-v3.8h}, [x0], #64", "LD_mult",
			map[string]int64{"p": 1, "rm": 31, "rpt": 1, "selem": 4, "sz": 1}},

		// Single-structure. index is a disjoint field (Q:size:S), and selem
		// runs through !function=plus_1 -- raw bits 0 means one register.
		{0x0d000c00, "st1 {v0.b}[3], [x0]", "ST_single",
			map[string]int64{"scale": 0, "index": 3, "selem": 1}},
		{0x0d409000, "ld1 {v0.s}[1], [x0]", "LD_single",
			map[string]int64{"scale": 2, "index": 1, "selem": 1}},
		{0x4d40c800, "ld1r {v0.4s}, [x0]", "LD_single_repl",
			map[string]int64{"q": 1, "scale": 2, "selem": 1}},

		// Table lookup. len is the consecutive-register-group count TODO.md
		// says this family needs, handed over as a field.
		{0x4e020020, "tbl v0.16b, {v1.16b}, v2.16b", "TBL_TBX",
			map[string]int64{"q": 1, "rm": 2, "len": 0, "tbx": 0, "rn": 1}},
		{0x0e032020, "tbl v0.8b, {v1.16b-v2.16b}, v3.8b", "TBL_TBX",
			map[string]int64{"q": 0, "rm": 3, "len": 1, "tbx": 0}},
		{0x4e045020, "tbx v0.16b, {v1.16b-v3.16b}, v4.16b", "TBL_TBX",
			map[string]int64{"rm": 4, "len": 2, "tbx": 1}},
		{0x4e056380, "tbl v0.16b, {v28.16b-v31.16b}, v5.16b", "TBL_TBX",
			map[string]int64{"rm": 5, "len": 3, "tbx": 0, "rn": 28}},
	} {
		m := tab.Match(c.enc)
		if m == nil {
			t.Errorf("%#08x (%s): no match", c.enc, c.asm)
			continue
		}
		if m.Insn.Name != c.pat {
			t.Errorf("%#08x (%s): matched %s, want %s\n%s", c.enc, c.asm, m.Insn.Name, c.pat, m.Describe())
			continue
		}
		for name, want := range c.want {
			if got := val(t, m, name); got != want {
				t.Errorf("%#08x (%s): %s = %d, want %d\n%s", c.enc, c.asm, name, got, want, m.Describe())
			}
		}
	}
}

// TestA64ReportsNoMatchRatherThanGuessing: an encoding with no pattern must
// come back nil. The all-zeros word matters specifically -- it is the padding
// elflift lifts as code, and TODO.md records it as 8,159 sites on the crypto
// image, so it dominates any raw report and must be identifiable.
func TestA64ReportsNoMatch(t *testing.T) {
	tab, err := A64()
	if err != nil {
		t.Fatal(err)
	}
	if m := tab.Match(0x00000000); m != nil {
		t.Errorf("all-zero word matched %s; padding must be reported as unmatched\n%s",
			m.Insn.Name, m.Describe())
	}
}

func TestFieldFuncsMatchQEMU(t *testing.T) {
	// Transcribed from QEMU at the vendored pin; see PROVENANCE.md for the
	// file and line of each. Spot-checked against values that would differ
	// under a plausible mis-transcription (e.g. rsub_32 vs rsub_64).
	for _, c := range []struct {
		fn   string
		in   int64
		want int64
	}{
		{"plus_1", 0, 1},
		{"plus_2", 1, 3},
		{"times_4", 3, 12},
		{"times_8", 3, 24},
		{"rsub_64", 31, 33},
		{"rsub_32", 15, 17},
		{"rsub_16", 5, 11},
		{"rsub_8", 5, 3},
		{"shl_12", 1, 4096},
		{"xor_2", 1, 3},
		{"xor_2", 2, 0},
		{"uimm_scaled", 8<<3 | 2, 32},
		{"scale_by_log2_tag_granule", 1, 16},
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

// TestEveryFieldFuncUsedIsImplemented walks EVERY vendored table rather than a
// hand-written list, so a re-vendor that introduces a new !function= fails here
// instead of silently reporting an Unresolved operand in the middle of a
// worklist.
func TestEveryFieldFuncUsedIsImplemented(t *testing.T) {
	dec, err := AArch64()
	if err != nil {
		t.Fatal(err)
	}
	missing := map[string]bool{}
	for _, tab := range dec.Tables {
		for _, f := range tab.fields {
			if f.fn == "" {
				continue
			}
			if _, ok := fieldFuncs[f.fn]; !ok {
				missing[f.fn] = true
			}
		}
	}
	if len(missing) > 0 {
		var names []string
		for n := range missing {
			names = append(names, n)
		}
		t.Errorf("the vendored tables use !function= helpers this package does not implement: %s",
			strings.Join(names, ", "))
	}
}

func TestDescribeIsStable(t *testing.T) {
	tab, err := A64()
	if err != nil {
		t.Fatal(err)
	}
	m := tab.Match(0x4c9f7000)
	if m == nil {
		t.Fatal("no match")
	}
	first := m.Describe()
	for i := 0; i < 8; i++ {
		if got := tab.Match(0x4c9f7000).Describe(); got != first {
			t.Fatalf("Describe is not deterministic:\n%s\nvs\n%s", first, got)
		}
	}
	if !strings.Contains(first, "ST_mult") {
		t.Errorf("Describe lost the pattern name: %s", first)
	}
	if !strings.Contains(first, fmt.Sprintf("%#08x", m.Insn.Mask)) {
		t.Errorf("Describe lost the mask: %s", first)
	}
}
