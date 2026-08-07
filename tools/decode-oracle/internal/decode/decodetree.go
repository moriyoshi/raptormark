// Copyright 2026 The raptormark Authors
// SPDX-License-Identifier: LGPL-2.1-or-later

// Package decode is a decode oracle for aarch64 instruction encodings.
//
// # Why this exists
//
// elfconv's aarch64 decoder is incomplete, and elflift reports every rejected
// instruction as `[ecv-undecoded] vma=... enc=... fn=...` (elfconv patch 0057).
// That report names encodings, not instructions, and turning a 32-bit encoding
// into "which instruction is this, and where are its operand fields" by hand is
// the single most error-prone step in closing a coverage gap -- see AGENTS.md:
// "Instruction masks derived by hand are wrong more often than not."
//
// Two cheaper routes were tried first and are recorded as dead ends in
// .agents/docs/TODO.md. Grepping elfconv's stub decoders by MNEMONIC reports 83
// false positives, because objdump prints ALIASES (`cmp`, `mov`, `tst`, `lsl`,
// `neg`) whose canonical decoders are implemented; the mnemonic -> decoder map
// is many-to-one and only some variants are stubs. Hand-narrowing to encoding
// groups worked once, for patch 0056, and missed `usubw` and `cmlt` -- two more
// half-hour lifts. The unit of truth is the raw encoding, and nothing else.
//
// So this package matches raw encodings against QEMU's decodetree tables, which
// are a declarative machine-readable spec of the A64 encoding space. The payoff
// is concentrated exactly where raptormark's debt is: TODO.md calls `st1` "74
// distinct post-index forms", and a64.decode expresses that same space as seven
// ST_mult patterns over one @ldst_mult format, handing back `rpt` and `selem` as
// named values. `tbl`, which TODO.md notes "needs consecutive-register-group
// handling", is one pattern line with `len` as a field.
//
// # What this is NOT
//
// This is a naming and field-extraction oracle. It carries no instruction
// semantics, and taking QEMU's decoder is explicitly not a step toward taking
// QEMU's backend: TCG's IR is integer-only and its FP and FP-vector paths are
// helper calls into softfloat, which lowered to wasm would put an interpreter
// back into raptormark's output.
//
// A match names the pattern QEMU would select. It does not prove the guest
// instruction is architecturally defined, and it says nothing about whether
// elfconv's implementation of it would be correct.
//
// # Fidelity
//
// The parser implements decodetree as specified in QEMU's
// docs/devel/decodetree.rst at the vendored pin. Two deliberate departures,
// both of which only ever make this MORE conservative than QEMU:
//
//  1. Matching is a linear first-match-wins scan in file order rather than the
//     generated decision tree. These agree exactly when no two patterns outside
//     an overlap group overlap -- which decodetree enforces at generation time,
//     and which Table.Validate re-checks here rather than assuming.
//
//  2. Inside an overlap group, QEMU tries the next pattern when a translate
//     function returns false. There are no translate functions here, so the
//     first pattern whose bits match wins. At the vendored pin the only overlap
//     group is the Hint group, where this picks the specific hint over the
//     catch-all NOP -- the answer a human wants anyway.
package decode

import (
	"fmt"
	"strconv"
	"strings"
)

// partSource says where one component of a %field definition reads from.
type partSource int

const (
	// fromInsn extracts a bit range out of the instruction word.
	fromInsn partSource = iota
	// fromNamed extracts out of the value of another named field.
	//
	// decodetree.rst: "A named_field refers to some other field in the
	// instruction pattern or format." At the vendored pin the sole user is
	// `%uimm_scaled 10:12 sz:3`, whose `sz` is supplied by the PATTERN that
	// uses @ldst_uimm -- and by different patterns as either a field (`STR_i
	// sz:2 ...`) or a constant (`LDR_i ... sz=0`). Both must resolve, which is
	// why evaluation works over merged operands rather than over the insn.
	fromNamed
)

// fieldPart is one component of a %field. Multiple parts concatenate.
type fieldPart struct {
	source partSource
	lsb    int    // fromInsn
	ref    string // fromNamed
	width  int
	signed bool
}

// fieldDef is a `%name ...` definition.
type fieldDef struct {
	name  string
	parts []fieldPart
	fn    string // !function=, "" when absent
	line  int
}

// argSet is a `&name ...` definition. Argument TYPES are parsed and discarded:
// they exist so QEMU can render a C struct, and nothing here renders C.
type argSet struct {
	name string
	args []string
	line int
}

// placed is an inline named field (`rn:5`) at a resolved bit position.
type placed struct {
	name   string
	lsb    int
	width  int
	signed bool
}

// layout is the part of a format or pattern line that describes bits: the
// fixed-bit runs, the inline named fields, the %field references, the constant
// assignments and the argument set.
type layout struct {
	fixedMask uint32
	fixedBits uint32
	// covered is every bit accounted for by a fixed bit, an inline field, or an
	// explicit '-' ignore. decodetree requires full coverage once any bit
	// element appears; Validate checks it rather than trusting it.
	covered uint32
	placed  []placed
	// refs maps an argument name to the %field supplying it. `%hl` is recorded
	// as "hl" -> "hl"; the renaming form `idx=%hl` as "idx" -> "hl".
	refs   map[string]string
	consts map[string]int64
	argSet string
	fmtRef string // patterns only
}

func newLayout() *layout {
	return &layout{refs: map[string]string{}, consts: map[string]int64{}}
}

// format is an `@name ...` definition.
type format struct {
	name string
	lay  *layout
	line int
}

// pattern is one instruction pattern, before resolution.
type pattern struct {
	name string
	lay  *layout
	line int
	// group is the outermost enclosing `{ }` group, or -1 for none.
	group int
}

// Table is a parsed decodetree file, ready to match against.
type Table struct {
	// Insns is every pattern in file order. Matching scans it in this order,
	// so the order is load-bearing, not incidental.
	Insns []*Insn

	fields  map[string]*fieldDef
	argSets map[string]*argSet
	formats map[string]*format
}

// OperandKind distinguishes how an operand gets its value.
type OperandKind int

const (
	// OpBits is a direct extraction of a contiguous bit range.
	OpBits OperandKind = iota
	// OpField is a %field: possibly disjoint, possibly signed, possibly passed
	// through a !function.
	OpField
	// OpConst is a constant assigned by the pattern or format, e.g. `selem=4`.
	// These carry real information -- `rpt` and `selem` for ST_mult exist only
	// as constants -- so they are reported alongside extracted operands.
	OpConst
)

// Operand is one argument of a resolved pattern.
type Operand struct {
	Name string
	Kind OperandKind

	// OpBits
	Lsb    int
	Width  int
	Signed bool

	// OpField
	Field *fieldDef

	// OpConst
	Const int64
}

// Insn is a resolved pattern: a pattern merged with the format it references.
type Insn struct {
	// Name is the decodetree pattern name, e.g. "ST_mult". It is NOT a
	// mnemonic and deliberately so; a mnemonic is the thing that could not be
	// used here.
	Name string
	// Format is the @format the pattern referenced, "" when it declared its
	// bits inline.
	Format string
	// ArgSet is the &argset name, "" when decodetree would have inferred one.
	ArgSet string

	// Mask and Bits are the match condition: (enc & Mask) == Bits.
	Mask uint32
	Bits uint32

	// Operands are in a stable order: extracted operands by descending bit
	// position, then %field references, then constants, each group sorted by
	// name. Stable so a report diffs cleanly between runs.
	Operands []Operand

	// Source is the decode file this pattern came from, e.g. "a64.decode".
	// Needed once more than one table is in play: "SLI_v" and an SVE pattern
	// of the same shape are answers from different decoders, and a report that
	// did not say which would be ambiguous about where to go and look.
	Source string
	// Line is the 1-based line in the source file, so a report can point at
	// the upstream table.
	Line int

	// Overlap records that this pattern was inside a `{ }` group.
	Overlap bool
	// Group identifies the OUTERMOST `{ }` group enclosing this pattern, or -1
	// when there is none. Two patterns may legally overlap only when they share
	// a group; Validate needs to tell "same group" from "both happen to be in
	// some group", so this is an identity and not just a flag.
	Group int

	covered uint32
}

// Parse reads a decodetree source file.
//
// name is used only in error messages.
func Parse(name, src string) (*Table, error) {
	p := &parser{
		file: name,
		t: &Table{
			fields:  map[string]*fieldDef{},
			argSets: map[string]*argSet{},
			formats: map[string]*format{},
		},
		curGroup: noGroup,
	}
	if err := p.run(src); err != nil {
		return nil, err
	}
	if err := p.resolve(); err != nil {
		return nil, err
	}
	return p.t, nil
}

// noGroup marks a pattern that is not inside any `{ }` overlap group.
const noGroup = -1

type parser struct {
	file string
	t    *Table
	pats []*pattern

	// curGroup is the outermost enclosing `{ }` group in effect, and stack
	// holds the value to restore when the current group closes. A `[ ]` group
	// pushes without changing curGroup: a no-overlap group nested inside an
	// overlap group is still, as far as overlap is concerned, that outer group.
	curGroup int
	stack    []int
	nGroups  int
}

// logicalLine is a source line after comment stripping and continuation
// joining, with the line number of its FIRST physical line.
type logicalLine struct {
	text string
	num  int
}

func (p *parser) run(src string) error {
	for _, ln := range logicalLines(src) {
		if err := p.line(ln); err != nil {
			return err
		}
	}
	if len(p.stack) != 0 {
		return fmt.Errorf("%s: unclosed group at end of file", p.file)
	}
	return nil
}

// logicalLines strips `#` comments and joins `\` continuations.
//
// Comments are stripped BEFORE continuations are joined, matching
// decodetree.py: a comment ends the physical line, and a `\` inside a comment
// is not a continuation.
func logicalLines(src string) []logicalLine {
	var out []logicalLine
	var acc strings.Builder
	start := 0
	for i, raw := range strings.Split(src, "\n") {
		num := i + 1
		s := raw
		if j := strings.IndexByte(s, '#'); j >= 0 {
			s = s[:j]
		}
		s = strings.TrimRight(s, " \t\r")
		cont := strings.HasSuffix(s, `\`)
		if cont {
			s = strings.TrimSuffix(s, `\`)
		}
		if acc.Len() == 0 {
			start = num
			// Leading whitespace is significant only for group nesting, which
			// is checked against a lone brace; patterns are keyed by their
			// first token, so trimming here is safe and simplifies every
			// downstream tokenizer.
			acc.WriteString(strings.TrimLeft(s, " \t"))
		} else {
			acc.WriteString(" ")
			acc.WriteString(strings.TrimLeft(s, " \t"))
		}
		if cont {
			continue
		}
		text := strings.TrimSpace(acc.String())
		acc.Reset()
		if text != "" {
			out = append(out, logicalLine{text: text, num: start})
		}
	}
	// A trailing continuation with no following line.
	if s := strings.TrimSpace(acc.String()); s != "" {
		out = append(out, logicalLine{text: s, num: start})
	}
	return out
}

func (p *parser) line(ln logicalLine) error {
	switch ln.text {
	case "{":
		p.stack = append(p.stack, p.curGroup)
		if p.curGroup == noGroup {
			p.curGroup = p.nGroups
			p.nGroups++
		}
		return nil
	case "[":
		p.stack = append(p.stack, p.curGroup)
		return nil
	case "}", "]":
		if len(p.stack) == 0 {
			return fmt.Errorf("%s:%d: %q with no open group", p.file, ln.num, ln.text)
		}
		p.curGroup = p.stack[len(p.stack)-1]
		p.stack = p.stack[:len(p.stack)-1]
		return nil
	}

	fields := strings.Fields(ln.text)
	head, rest := fields[0], fields[1:]
	switch {
	case strings.HasPrefix(head, "%"):
		return p.fieldDef(ln, head[1:], rest)
	case strings.HasPrefix(head, "&"):
		return p.argSetDef(ln, head[1:], rest)
	case strings.HasPrefix(head, "@"):
		return p.formatDef(ln, head[1:], rest)
	default:
		return p.patternDef(ln, head, rest)
	}
}

func (p *parser) fieldDef(ln logicalLine, name string, toks []string) error {
	if _, dup := p.t.fields[name]; dup {
		return fmt.Errorf("%s:%d: duplicate field %%%s", p.file, ln.num, name)
	}
	f := &fieldDef{name: name, line: ln.num}
	for _, tok := range toks {
		if fn, ok := strings.CutPrefix(tok, "!function="); ok {
			f.fn = fn
			continue
		}
		lhs, rhs, ok := strings.Cut(tok, ":")
		if !ok {
			return fmt.Errorf("%s:%d: field %%%s: bad component %q", p.file, ln.num, name, tok)
		}
		signed := strings.HasPrefix(rhs, "s")
		width, err := strconv.Atoi(strings.TrimPrefix(rhs, "s"))
		if err != nil || width <= 0 || width > 64 {
			return fmt.Errorf("%s:%d: field %%%s: bad width in %q", p.file, ln.num, name, tok)
		}
		part := fieldPart{width: width, signed: signed}
		if lsb, err := strconv.Atoi(lhs); err == nil {
			part.source = fromInsn
			part.lsb = lsb
			if lsb < 0 || lsb+width > 32 {
				return fmt.Errorf("%s:%d: field %%%s: %q is outside the instruction word", p.file, ln.num, name, tok)
			}
		} else {
			part.source = fromNamed
			part.ref = lhs
		}
		f.parts = append(f.parts, part)
	}
	if len(f.parts) == 0 && f.fn == "" {
		return fmt.Errorf("%s:%d: field %%%s has neither components nor !function", p.file, ln.num, name)
	}
	p.t.fields[name] = f
	return nil
}

func (p *parser) argSetDef(ln logicalLine, name string, toks []string) error {
	if _, dup := p.t.argSets[name]; dup {
		return fmt.Errorf("%s:%d: duplicate argument set &%s", p.file, ln.num, name)
	}
	a := &argSet{name: name, line: ln.num}
	for _, tok := range toks {
		if tok == "!extern" {
			continue
		}
		// `name` or `name:type`; the type is QEMU's C rendering concern.
		arg, _, _ := strings.Cut(tok, ":")
		a.args = append(a.args, arg)
	}
	if len(a.args) == 0 {
		return fmt.Errorf("%s:%d: argument set &%s is empty", p.file, ln.num, name)
	}
	p.t.argSets[name] = a
	return nil
}

func (p *parser) formatDef(ln logicalLine, name string, toks []string) error {
	if _, dup := p.t.formats[name]; dup {
		return fmt.Errorf("%s:%d: duplicate format @%s", p.file, ln.num, name)
	}
	lay, err := p.layout(ln, "format @"+name, toks, false)
	if err != nil {
		return err
	}
	p.t.formats[name] = &format{name: name, lay: lay, line: ln.num}
	return nil
}

func (p *parser) patternDef(ln logicalLine, name string, toks []string) error {
	lay, err := p.layout(ln, "pattern "+name, toks, true)
	if err != nil {
		return err
	}
	p.pats = append(p.pats, &pattern{name: name, lay: lay, line: ln.num, group: p.curGroup})
	return nil
}

// layout parses the bit-describing tail of a format or pattern line.
//
// Bit position runs MSB-first: the first bit element covers bit 31 and each
// subsequent element continues downward. Whitespace between elements is
// insignificant -- `0 . 001100` and `0.001100` describe the same eight bits --
// which is why elements are consumed by width rather than matched by column.
func (p *parser) layout(ln logicalLine, what string, toks []string, isPattern bool) (*layout, error) {
	lay := newLayout()
	pos := 32
	sawBits := false

	for _, tok := range toks {
		switch {
		case strings.HasPrefix(tok, "@"):
			if !isPattern {
				return nil, fmt.Errorf("%s:%d: %s: a format may not reference another format", p.file, ln.num, what)
			}
			if lay.fmtRef != "" {
				return nil, fmt.Errorf("%s:%d: %s: more than one format reference", p.file, ln.num, what)
			}
			lay.fmtRef = tok[1:]

		case strings.HasPrefix(tok, "&"):
			if lay.argSet != "" {
				return nil, fmt.Errorf("%s:%d: %s: more than one argument set", p.file, ln.num, what)
			}
			lay.argSet = tok[1:]

		case strings.HasPrefix(tok, "%"):
			// Incorporate a field under its own name.
			lay.refs[tok[1:]] = tok[1:]

		case isFixedBits(tok):
			sawBits = true
			for _, c := range tok {
				pos--
				if pos < 0 {
					return nil, fmt.Errorf("%s:%d: %s: bit pattern is longer than 32 bits", p.file, ln.num, what)
				}
				switch c {
				case '0':
					lay.fixedMask |= 1 << uint(pos)
					lay.covered |= 1 << uint(pos)
				case '1':
					lay.fixedMask |= 1 << uint(pos)
					lay.fixedBits |= 1 << uint(pos)
					lay.covered |= 1 << uint(pos)
				case '-':
					// Really ignored by the cpu: covered, never matched.
					lay.covered |= 1 << uint(pos)
				case '.':
					// To be covered by a field, or by the pattern that uses
					// this format. Left uncovered on purpose.
				}
			}

		case strings.Contains(tok, "="):
			lhs, rhs, _ := strings.Cut(tok, "=")
			if ref, ok := strings.CutPrefix(rhs, "%"); ok {
				// Renaming field reference: `idx=%hl`.
				lay.refs[lhs] = ref
				continue
			}
			v, err := strconv.ParseInt(rhs, 0, 64)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %s: bad constant %q", p.file, ln.num, what, tok)
			}
			lay.consts[lhs] = v

		case strings.Contains(tok, ":"):
			// Inline named field: `rn:5`, `imm:s9`.
			lhs, rhs, _ := strings.Cut(tok, ":")
			signed := strings.HasPrefix(rhs, "s")
			width, err := strconv.Atoi(strings.TrimPrefix(rhs, "s"))
			if err != nil || width <= 0 {
				return nil, fmt.Errorf("%s:%d: %s: bad field %q", p.file, ln.num, what, tok)
			}
			sawBits = true
			pos -= width
			if pos < 0 {
				return nil, fmt.Errorf("%s:%d: %s: field %q runs past bit 0", p.file, ln.num, what, tok)
			}
			lay.placed = append(lay.placed, placed{name: lhs, lsb: pos, width: width, signed: signed})
			lay.covered |= mask(pos, width)

		default:
			return nil, fmt.Errorf("%s:%d: %s: unrecognised element %q", p.file, ln.num, what, tok)
		}
	}

	// "If any fixedbit_elt or field_elt appear, then all bits must be defined."
	// A pattern that only references a format declares no bits of its own and
	// is exempt.
	if sawBits && pos != 0 {
		return nil, fmt.Errorf("%s:%d: %s: describes %d bits, want 32", p.file, ln.num, what, 32-pos)
	}
	return lay, nil
}

// isFixedBits reports whether tok is a fixed-bit run. The empty string is not.
func isFixedBits(tok string) bool {
	if tok == "" {
		return false
	}
	return strings.IndexFunc(tok, func(r rune) bool {
		return r != '0' && r != '1' && r != '.' && r != '-'
	}) < 0
}

// mask returns a width-bit mask at lsb.
func mask(lsb, width int) uint32 {
	if width >= 32 {
		return ^uint32(0)
	}
	return ((1 << uint(width)) - 1) << uint(lsb)
}

// resolve merges each pattern with its format and materialises Table.Insns.
func (p *parser) resolve() error {
	for _, pat := range p.pats {
		in, err := p.resolveOne(pat)
		if err != nil {
			return err
		}
		p.t.Insns = append(p.t.Insns, in)
	}
	return nil
}

func (p *parser) resolveOne(pat *pattern) (*Insn, error) {
	in := &Insn{
		Name:    pat.name,
		Source:  p.file,
		Line:    pat.line,
		Group:   pat.group,
		Overlap: pat.group != noGroup,
		ArgSet:  pat.lay.argSet,
	}

	// Start from the format, then overlay the pattern. Order matters: the
	// pattern's constants and fields win, which is what lets STR_i override
	// @ldst_uimm's inferred `sz` with `sz:2` and LDR_i override it with `sz=0`.
	lays := []*layout{}
	if pat.lay.fmtRef != "" {
		f, ok := p.t.formats[pat.lay.fmtRef]
		if !ok {
			return nil, fmt.Errorf("%s:%d: pattern %s references unknown format @%s",
				p.file, pat.line, pat.name, pat.lay.fmtRef)
		}
		in.Format = f.name
		if in.ArgSet == "" {
			in.ArgSet = f.lay.argSet
		}
		lays = append(lays, f.lay)
	}
	lays = append(lays, pat.lay)

	// Fixed bits combine. A genuine conflict -- the same bit fixed to 0 by one
	// and 1 by the other -- would mean the pattern can never match, so it is an
	// error here rather than a silently dead entry in the table.
	for _, lay := range lays {
		if both := in.Mask & lay.fixedMask; both != 0 {
			if (in.Bits & both) != (lay.fixedBits & both) {
				return nil, fmt.Errorf("%s:%d: pattern %s: fixed bits conflict with format @%s (mask %#08x)",
					p.file, pat.line, pat.name, in.Format, both)
			}
		}
		in.Mask |= lay.fixedMask
		in.Bits |= lay.fixedBits
		in.covered |= lay.covered
	}

	// Operands, in three groups so a report reads predictably.
	byName := map[string]Operand{}
	for _, lay := range lays {
		for _, pl := range lay.placed {
			byName[pl.name] = Operand{
				Name: pl.name, Kind: OpBits,
				Lsb: pl.lsb, Width: pl.width, Signed: pl.signed,
			}
		}
	}
	for _, lay := range lays {
		for arg, ref := range lay.refs {
			f, ok := p.t.fields[ref]
			if !ok {
				return nil, fmt.Errorf("%s:%d: pattern %s references unknown field %%%s",
					p.file, pat.line, pat.name, ref)
			}
			byName[arg] = Operand{Name: arg, Kind: OpField, Field: f}
		}
	}
	for _, lay := range lays {
		for arg, v := range lay.consts {
			byName[arg] = Operand{Name: arg, Kind: OpConst, Const: v}
		}
	}

	in.Operands = orderOperands(byName)

	// A %field covers the bits it reads, just as an inline field does. Without
	// this every pattern whose immediate is a disjoint field -- ADR, B, the
	// whole PC-relative family -- looks like it leaves bits undefined, which is
	// a bug in the coverage bookkeeping and not in the table. Parts that read
	// another field's value rather than the instruction cover nothing here.
	for _, op := range in.Operands {
		if op.Kind != OpField {
			continue
		}
		for _, part := range op.Field.parts {
			if part.source == fromInsn {
				in.covered |= mask(part.lsb, part.width)
			}
		}
	}

	return in, nil
}

// orderOperands puts extracted operands first by descending bit position, then
// %field operands by name, then constants by name. Deterministic output is not
// cosmetic here: the report is meant to be diffed between two lifts.
func orderOperands(m map[string]Operand) []Operand {
	var bits, fields, consts []Operand
	for _, op := range m {
		switch op.Kind {
		case OpBits:
			bits = append(bits, op)
		case OpField:
			fields = append(fields, op)
		case OpConst:
			consts = append(consts, op)
		}
	}
	sortBy(bits, func(a, b Operand) bool {
		if a.Lsb != b.Lsb {
			return a.Lsb > b.Lsb
		}
		return a.Name < b.Name
	})
	sortBy(fields, func(a, b Operand) bool { return a.Name < b.Name })
	sortBy(consts, func(a, b Operand) bool { return a.Name < b.Name })
	return append(append(bits, fields...), consts...)
}

// sortBy is an insertion sort. The slices here are single-digit length, and an
// insertion sort keeps the comparator honest about being a strict ordering.
func sortBy(s []Operand, less func(a, b Operand) bool) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && less(s[j], s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
