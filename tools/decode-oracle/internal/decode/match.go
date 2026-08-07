// Copyright 2026 The raptormark Authors
// SPDX-License-Identifier: LGPL-2.1-or-later

package decode

import (
	"fmt"
	"math/bits"
	"sort"
	"strings"
)

// Value is one operand of a matched instruction, with its decoded value.
type Value struct {
	Name string
	// Value is what QEMU would hand its translator: sign-extended where the
	// field says so, and passed through !function where one is named.
	Value int64
	// Raw is the value before !function was applied. Equal to Value when there
	// is no function. Reported separately because the raw bits are what a
	// decoder patch has to extract, while Value is what the bits mean.
	Raw int64
	// Src describes the provenance for the report: "20:5" for a direct
	// extraction, "%hl" for a complex field, "const" for a constant.
	Src string
	// Fn is the !function applied, "" when none.
	Fn string
	// Unresolved is set when Fn names a function this package does not
	// implement. Value then holds Raw, and the report must not present it as a
	// decoded value. Reporting a raw number as though it were a decoded one is
	// exactly the silent-wrongness this whole tool exists to remove.
	Unresolved bool
}

// Match is the result of decoding one encoding.
type Match struct {
	Insn   *Insn
	Values []Value
}

// Match returns the pattern QEMU's decoder would select for enc, or nil when
// no pattern matches.
//
// A nil result is a real answer, not a failure. It means one of: the encoding
// is architecturally undefined; it is data that elflift lifted as code (very
// common -- `enc=0x00000000` padding dominates a raw report); or it lives in a
// corner of A64 the vendored pin does not cover. The caller distinguishes these
// by cross-checking against a disassembler, which is what the corpus mode does.
func (t *Table) Match(enc uint32) *Match {
	for _, in := range t.Insns {
		if enc&in.Mask == in.Bits {
			return &Match{Insn: in, Values: t.values(in, enc)}
		}
	}
	return nil
}

// values decodes every operand of in against enc.
func (t *Table) values(in *Insn, enc uint32) []Value {
	// Direct extractions and constants first: a %field may reference them by
	// name (decodetree's named_field), so they must exist before the complex
	// fields are evaluated.
	byName := map[string]int64{}
	out := make([]Value, 0, len(in.Operands))

	for _, op := range in.Operands {
		switch op.Kind {
		case OpBits:
			v := extract(enc, op.Lsb, op.Width, op.Signed)
			byName[op.Name] = v
			out = append(out, Value{
				Name: op.Name, Value: v, Raw: v,
				Src: fmt.Sprintf("%d:%d", op.Lsb, op.Width),
			})
		case OpConst:
			byName[op.Name] = op.Const
			out = append(out, Value{
				Name: op.Name, Value: op.Const, Raw: op.Const, Src: "const",
			})
		}
	}

	for _, op := range in.Operands {
		if op.Kind != OpField {
			continue
		}
		raw := evalField(op.Field, enc, byName)
		v := Value{
			Name: op.Name, Raw: raw, Value: raw,
			Src: "%" + op.Field.name, Fn: op.Field.fn,
		}
		if op.Field.fn != "" {
			fn, ok := fieldFuncs[op.Field.fn]
			if !ok {
				v.Unresolved = true
			} else {
				v.Value = fn(raw)
			}
		}
		byName[op.Name] = v.Value
		out = append(out, v)
	}

	return out
}

// evalField computes a %field's concatenated value.
//
// decodetree.rst: parts concatenate most-significant first, each shifted left
// by the total width of the parts that follow it. Signedness is per-part: only
// the leading part is ever signed in practice, but the rule is per-part and is
// implemented that way rather than assumed.
func evalField(f *fieldDef, enc uint32, byName map[string]int64) int64 {
	var acc int64
	rest := 0
	for _, p := range f.parts {
		rest += p.width
	}
	for _, p := range f.parts {
		rest -= p.width
		var v int64
		switch p.source {
		case fromInsn:
			v = extract(enc, p.lsb, p.width, p.signed)
		case fromNamed:
			// "extract(a->sz, 0, 3)": the referenced field's VALUE is the
			// source word, not the instruction.
			src, ok := byName[p.ref]
			if !ok {
				// The referencing format is used by patterns that supply the
				// name; a pattern that does not is a table error, but reading
				// zero here is the conservative answer and Validate reports it.
				src = 0
			}
			v = extract(uint32(src), 0, p.width, p.signed)
		}
		acc |= v << uint(rest)
	}
	return acc
}

// extract pulls width bits at lsb out of enc, sign-extending when signed.
func extract(enc uint32, lsb, width int, signed bool) int64 {
	if width <= 0 || lsb < 0 || lsb >= 32 {
		return 0
	}
	if lsb+width > 32 {
		width = 32 - lsb
	}
	v := int64((enc >> uint(lsb)) & uint32(mask(0, width)))
	if signed && width < 64 && v&(1<<uint(width-1)) != 0 {
		v -= 1 << uint(width)
	}
	return v
}

// fieldFuncs implements decodetree's `!function=` helpers.
//
// These are transcribed from QEMU at the vendored pin, NOT inferred from their
// names -- see third_party/qemu-decode/PROVENANCE.md, which cites the file and
// line of each. A helper that is subtly wrong produces a plausible number, and
// a plausible wrong number is worse here than no number at all, which is why an
// unknown name is reported as Unresolved rather than silently passed through.
var fieldFuncs = map[string]func(int64) int64{
	// target/arm/tcg/translate.h
	"plus_1":  func(x int64) int64 { return x + 1 },
	"plus_2":  func(x int64) int64 { return x + 2 },
	"times_4": func(x int64) int64 { return x * 4 },
	"times_8": func(x int64) int64 { return x * 8 },
	"rsub_64": func(x int64) int64 { return 64 - x },
	"rsub_32": func(x int64) int64 { return 32 - x },
	"rsub_16": func(x int64) int64 { return 16 - x },
	"rsub_8":  func(x int64) int64 { return 8 - x },
	"shl_12":  func(x int64) int64 { return x << 12 },
	"xor_2":   func(x int64) int64 { return x ^ 2 },

	// target/arm/tcg/translate-a64.c:63
	//   unsigned imm = x >> 3; unsigned scale = extract32(x, 0, 3);
	//   return imm << scale;
	"uimm_scaled": func(x int64) int64 { return (x >> 3) << uint(x&7) },

	// target/arm/tcg/translate-a64.c:71, with LOG2_TAG_GRANULE == 4
	// (target/arm/cpu.h:2716).
	"scale_by_log2_tag_granule": func(x int64) int64 { return x << 4 },

	// --- sve.decode. target/arm/tcg/translate.h ---
	"plus_8":  func(x int64) int64 { return x + 8 },
	"plus_12": func(x int64) int64 { return x + 12 },
	"times_2": func(x int64) int64 { return x * 2 },

	// target/arm/tcg/translate-sve.c:50
	//   x >>= 3;  /* discard imm3 */
	//   return 31 - clz32(x);
	//
	// clz32(0) is 32 (include/qemu/host-utils.h:165), so a zero tsz yields -1
	// -- a NEGATIVE esz that QEMU's trans functions test for explicitly. It is
	// reproduced rather than clamped: -1 is the table saying "this encoding is
	// unallocated for this size", and clamping it to 0 would invent a lane
	// width for an instruction that has none.
	"tszimm_esz": func(x int64) int64 { return tszimmEsz(x) },

	// target/arm/tcg/translate-sve.c:56 and :71. Both return esz unchanged
	// when it is negative; QEMU's comment says the value is unused in that
	// case and only avoids undefined behaviour.
	"tszimm_shr": func(x int64) int64 {
		esz := tszimmEsz(x)
		if esz < 0 {
			return esz
		}
		return (16 << uint(esz)) - x
	},
	"tszimm_shl": func(x int64) int64 {
		esz := tszimmEsz(x)
		if esz < 0 {
			return esz
		}
		return x - (8 << uint(esz))
	},

	// target/arm/tcg/translate-sve.c:82 and :87.
	//   (int8_t)x  << (x & 0x100 ? 8 : 0)
	//   (uint8_t)x << (x & 0x100 ? 8 : 0)
	// The cast is to EIGHT bits and the shift is decided by bit 8, which is
	// outside them -- so the shift flag survives the truncation it looks like
	// it should be lost to.
	"expand_imm_sh8s": func(x int64) int64 { return int64(int8(x)) << shift8(x) },
	"expand_imm_sh8u": func(x int64) int64 { return int64(uint8(x)) << shift8(x) },

	// target/arm/tcg/translate-sve.c:95
	//   static const uint8_t dtype[5] = { 0, 5, 10, 15, 18 };
	//   return dtype[msz];
	"msz_dtype": func(x int64) int64 {
		dtype := [5]int64{0, 5, 10, 15, 18}
		if x < 0 || int(x) >= len(dtype) {
			// Out of range cannot happen from a 2-bit msz field, and QEMU
			// would read out of bounds if it did. Returning the input keeps
			// this total without inventing a plausible dtype.
			return x
		}
		return dtype[x]
	},
}

// tszimmEsz is translate-sve.c's tszimm_esz: discard imm3, then take the
// position of the highest set bit, or -1 when none is set.
func tszimmEsz(x int64) int64 {
	v := uint32(x) >> 3
	if v == 0 {
		return -1 // 31 - clz32(0) == 31 - 32
	}
	return int64(31 - bits.LeadingZeros32(v))
}

// shift8 is the `x & 0x100 ? 8 : 0` shared by expand_imm_sh8s/u.
func shift8(x int64) uint {
	if x&0x100 != 0 {
		return 8
	}
	return 0
}

// Problem is one finding from Validate.
type Problem struct {
	Kind string
	Msg  string
}

func (p Problem) String() string { return p.Kind + ": " + p.Msg }

// Validate re-checks the invariants decodetree enforces when it generates a
// decoder, so that this package's linear first-match-wins scan is known to
// agree with QEMU's decision tree rather than assumed to.
//
// It answers the question AGENTS.md insists on asking of any check: what would
// a PASS look like if the claim were false? If the parser mishandled the
// format/pattern overlay -- the subtlest thing it does, and the thing that
// distinguishes the four identical-looking SLI_v lines -- patterns would stop
// being mutually exclusive and this reports it. A parser bug that leaves
// patterns accidentally still-disjoint is caught by the corpus differential
// instead.
//
// Two invariants:
//
//   - Every pattern defines all 32 bits ("If any fixedbit_elt or field_elt
//     appear, then all bits must be defined").
//   - No two patterns overlap unless they share an enclosing `{ }` group.
//     Patterns overlap when some encoding satisfies both, i.e. when they agree
//     on every bit they both fix.
func (t *Table) Validate() []Problem {
	var out []Problem

	for _, in := range t.Insns {
		if in.covered != ^uint32(0) {
			out = append(out, Problem{
				Kind: "uncovered-bits",
				Msg: fmt.Sprintf("%s (line %d) leaves bits %#08x undefined",
					in.Name, in.Line, ^in.covered),
			})
		}
	}

	for i, a := range t.Insns {
		for _, b := range t.Insns[i+1:] {
			if a.Group != noGroup && a.Group == b.Group {
				continue // legal overlap, resolved by order
			}
			both := a.Mask & b.Mask
			if (a.Bits^b.Bits)&both != 0 {
				continue // they disagree on a shared fixed bit: disjoint
			}
			out = append(out, Problem{
				Kind: "overlap",
				Msg: fmt.Sprintf("%s (line %d) and %s (line %d) both match %#08x",
					a.Name, a.Line, b.Name, b.Line, a.Bits|b.Bits),
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Msg < out[j].Msg
	})
	return out
}

// Names returns the distinct pattern names in the table, sorted. Used by tests
// that assert a re-vendored table still carries the families raptormark owes
// coverage on.
func (t *Table) Names() []string {
	seen := map[string]bool{}
	var out []string
	for _, in := range t.Insns {
		if !seen[in.Name] {
			seen[in.Name] = true
			out = append(out, in.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Describe renders a match as the indented block the report prints.
func (m *Match) Describe() string {
	if m == nil {
		return "no match"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s", m.Insn.Name)
	if m.Insn.Format != "" {
		fmt.Fprintf(&b, " @%s", m.Insn.Format)
	}
	if m.Insn.ArgSet != "" {
		fmt.Fprintf(&b, " &%s", m.Insn.ArgSet)
	}
	fmt.Fprintf(&b, "  (%s:%d)\n", m.Insn.Source, m.Insn.Line)
	fmt.Fprintf(&b, "  fixedmask=%#08x fixedbits=%#08x\n", m.Insn.Mask, m.Insn.Bits)

	// Shared with WriteLookups so the same operand cannot render two ways
	// depending on which entry point asked.
	if s := formatOperands(m.Values); s != "" {
		fmt.Fprintf(&b, "  %s\n", s)
	}
	return b.String()
}
