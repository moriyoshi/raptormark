// Copyright 2026 The raptormark Authors
// SPDX-License-Identifier: LGPL-2.1-or-later

package decode

import (
	"fmt"
	"strconv"
	"strings"
)

// Lookup is everything known about one encoding.
//
// This is the unit an agent actually wants. Both the `-enc` CLI flag and the
// MCP `decode_encoding` tool render it, so the two cannot drift: a wrong answer
// is wrong in both places or neither.
type Lookup struct {
	Enc uint32 `json:"enc"`
	// Hex is Enc as it appears in elflift's report, so a result can be pasted
	// back into a grep.
	Hex string `json:"hex"`

	// Objdump is the independent disassembler's rendering, empty when no
	// disassembler was available. Undefined records that it declined the word.
	Objdump   string `json:"objdump,omitempty"`
	Undefined bool   `json:"objdumpUndefined,omitempty"`

	// Matched is false when no decodetree pattern claims this encoding. That is
	// a real answer -- padding, data lifted as code, or a corner of the ISA the
	// pinned tables do not cover -- and not an error.
	Matched bool   `json:"matched"`
	Pattern string `json:"pattern,omitempty"`
	Format  string `json:"format,omitempty"`
	ArgSet  string `json:"argSet,omitempty"`
	Source  string `json:"source,omitempty"`
	Line    int    `json:"line,omitempty"`

	Mask uint32 `json:"fixedmask,omitempty"`
	Bits uint32 `json:"fixedbits,omitempty"`

	Operands []Value `json:"operands,omitempty"`
}

// ParseEncoding accepts a 32-bit encoding as hex, with or without an 0x prefix.
//
// Deliberately hex-only. Every source of these -- elflift's report, objdump,
// a disassembly listing -- writes hex, and accepting decimal would silently
// reinterpret a bare "10" as sixteen.
func ParseEncoding(s string) (uint32, error) {
	t := strings.TrimSpace(s)
	t = strings.TrimPrefix(strings.TrimPrefix(t, "0x"), "0X")
	if t == "" {
		return 0, fmt.Errorf("empty encoding")
	}
	v, err := strconv.ParseUint(t, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("%q is not a 32-bit hex encoding", s)
	}
	return uint32(v), nil
}

// LookupEncodings resolves each encoding against the tables, and against the
// disassembler when one is available.
//
// dis may be nil, in which case the Objdump field is left empty. The result is
// in input order, one entry per input, so a caller can zip it against its own
// list without matching on value.
func LookupEncodings(dec *Decoder, dis *Disasm, encs []uint32) []Lookup {
	var lines map[uint32]Line
	if dis != nil && dis.Available() {
		// A disassembler failure is not fatal here: the decodetree answer is
		// the product, and objdump is the cross-check.
		lines, _ = dis.Decode(encs)
	}

	out := make([]Lookup, 0, len(encs))
	for _, e := range encs {
		l := Lookup{Enc: e, Hex: fmt.Sprintf("%#08x", e)}
		if line, ok := lines[e]; ok && !line.Failed {
			l.Objdump = line.Text
			l.Undefined = line.Undefined
		}
		if m := dec.Match(e); m != nil {
			l.Matched = true
			l.Pattern = m.Insn.Name
			l.Format = m.Insn.Format
			l.ArgSet = m.Insn.ArgSet
			l.Source = m.Insn.Source
			l.Line = m.Insn.Line
			l.Mask = m.Insn.Mask
			l.Bits = m.Insn.Bits
			l.Operands = m.Values
		}
		out = append(out, l)
	}
	return out
}

// WriteLookups renders lookups in the same shape the report's per-encoding
// section uses, so an answer looks the same however it was asked for.
func WriteLookups(b *strings.Builder, ls []Lookup) {
	for i, l := range ls {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(b, "enc=%s\n", l.Hex)
		if l.Objdump != "" {
			fmt.Fprintf(b, "  objdump: %s\n", l.Objdump)
		}
		if !l.Matched {
			b.WriteString("  no decodetree pattern matches this encoding\n")
			// Say WHY this is not necessarily a defect, because the most
			// common instance by far is padding and the second is data.
			switch {
			case l.Enc == PaddingEnc:
				b.WriteString("  (all-zero word: padding lifted as code, not an instruction)\n")
			case l.Undefined:
				b.WriteString("  (objdump also declines it: undefined encoding or data)\n")
			default:
				b.WriteString("  (objdump decoded it, so this is a real gap in the pinned tables)\n")
			}
			continue
		}
		fmt.Fprintf(b, "  %s", l.Pattern)
		if l.Format != "" {
			fmt.Fprintf(b, " @%s", l.Format)
		}
		if l.ArgSet != "" {
			fmt.Fprintf(b, " &%s", l.ArgSet)
		}
		fmt.Fprintf(b, "  (%s:%d)\n", l.Source, l.Line)
		fmt.Fprintf(b, "    fixedmask=%#08x fixedbits=%#08x\n", l.Mask, l.Bits)
		if s := formatOperands(l.Operands); s != "" {
			fmt.Fprintf(b, "    %s\n", s)
		}
	}
}

// formatOperands renders one operand line, shared with Match.Describe.
func formatOperands(vs []Value) string {
	var parts []string
	for _, v := range vs {
		switch {
		case v.Unresolved:
			parts = append(parts, fmt.Sprintf("%s=<raw %d, !function=%s unimplemented>", v.Name, v.Raw, v.Fn))
		case v.Fn != "":
			parts = append(parts, fmt.Sprintf("%s=%d(%s %d)", v.Name, v.Value, v.Fn, v.Raw))
		default:
			parts = append(parts, fmt.Sprintf("%s=%d", v.Name, v.Value))
		}
	}
	return strings.Join(parts, " ")
}
