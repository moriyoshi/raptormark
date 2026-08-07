// Copyright 2026 The raptormark Authors
// SPDX-License-Identifier: LGPL-2.1-or-later

package decode

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// PaddingEnc is the all-zero word.
//
// elflift lifts padding as code, so a raw report is dominated by it: 8,159
// sites on the cryptography image against 2,805 real ones. .agents/docs/TODO.md
// says to drop it when consuming the list -- but it is REPORTED separately
// rather than discarded, because it doubles as a control. When patch 0058 was
// verified at scale, the padding count staying at 8,159 across both runs is
// what said instruction coverage had moved and function-boundary recovery had
// not.
const PaddingEnc uint32 = 0

// undecodedLine matches elfconv patch 0057's report:
//
//	[ecv-undecoded] vma=0x4a1234 enc=0x4c9f7000 fn=0x4a1000
//
// Anchored on the tag rather than on line start, so it survives being teed
// through a timestamper or interleaved with other container output -- which is
// how it arrives in practice, since RAPTORMARK_TRANSLATE_VERBOSE=1 forwards
// every container's stderr onto one stream.
var undecodedLine = regexp.MustCompile(
	`\[ecv-undecoded\]\s+vma=0x([0-9a-fA-F]+)\s+enc=0x([0-9a-fA-F]+)\s+fn=0x([0-9a-fA-F]+)`)

// Encoding is one distinct encoding and everything seen about it.
type Encoding struct {
	Enc uint32
	// Sites is how many instructions in the guest had this encoding.
	Sites int
	// Funcs is how many distinct lifted functions contained it. A high site
	// count concentrated in one function is a different kind of problem from
	// the same count spread over hundreds.
	Funcs int
	// FirstVMA is the lowest address seen, so a human can go look at one.
	FirstVMA uint64

	funcs map[uint64]bool
}

// Family groups encodings by the decodetree pattern they matched.
type Family struct {
	// Pattern is the decodetree pattern name, or "" for encodings that matched
	// nothing.
	Pattern   string
	Sites     int
	Encodings int
	// Mnemonics are the distinct disassembler renderings seen in this family,
	// sorted. Present so the report can be read against TODO.md, which counts
	// by mnemonic; NOT used to decide anything.
	Mnemonics []string
}

// Report is an aggregated undecoded-instruction report.
type Report struct {
	// Source names where the lines came from, for the header.
	Source string

	// Sites and Encodings exclude padding.
	Sites     int
	Encodings int
	// PaddingSites is the enc=0x00000000 count, kept as a control.
	PaddingSites int
	// Unmatched is the number of non-padding sites whose encoding matched no
	// decodetree pattern.
	Unmatched int

	Families []Family
	Detail   []*Encoding

	dec *Decoder
	dis map[uint32]Line
}

// ParseLog reads elflift's undecoded report from r.
//
// Lines that are not the report are ignored, so a whole translate log can be
// fed in verbatim.
func ParseLog(r io.Reader) (map[uint32]*Encoding, error) {
	out := map[uint32]*Encoding{}
	sc := bufio.NewScanner(r)
	// Container logs carry long compiler diagnostics; the default 64 KiB token
	// limit would turn one of those into a scan error and silently truncate the
	// report.
	sc.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
	for sc.Scan() {
		m := undecodedLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		vma, err1 := strconv.ParseUint(m[1], 16, 64)
		enc, err2 := strconv.ParseUint(m[2], 16, 32)
		fn, err3 := strconv.ParseUint(m[3], 16, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		e := out[uint32(enc)]
		if e == nil {
			e = &Encoding{Enc: uint32(enc), FirstVMA: vma, funcs: map[uint64]bool{}}
			out[uint32(enc)] = e
		}
		e.Sites++
		e.funcs[fn] = true
		if vma < e.FirstVMA {
			e.FirstVMA = vma
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading report: %w", err)
	}
	for _, e := range out {
		e.Funcs = len(e.funcs)
	}
	return out, nil
}

// Aggregate joins parsed encodings against the decode table and, when one is
// available, a disassembler.
//
// dis may be nil: the report then carries pattern names and fields but no
// mnemonics. That is a degraded report, not a wrong one.
func Aggregate(source string, encs map[uint32]*Encoding, dec *Decoder, dis map[uint32]Line) *Report {
	rep := &Report{Source: source, dec: dec, dis: dis}

	byPattern := map[string]*Family{}
	mnemonics := map[string]map[string]bool{}

	for _, e := range encs {
		if e.Enc == PaddingEnc {
			rep.PaddingSites += e.Sites
			continue
		}
		rep.Sites += e.Sites
		rep.Encodings++
		rep.Detail = append(rep.Detail, e)

		pat := ""
		if m := dec.Match(e.Enc); m != nil {
			pat = m.Insn.Name
		} else {
			rep.Unmatched += e.Sites
		}
		f := byPattern[pat]
		if f == nil {
			f = &Family{Pattern: pat}
			byPattern[pat] = f
			mnemonics[pat] = map[string]bool{}
		}
		f.Sites += e.Sites
		f.Encodings++
		if l, ok := dis[e.Enc]; ok && l.Mnemonic != "" && !l.Undefined {
			mnemonics[pat][l.Mnemonic] = true
		}
	}

	for pat, f := range byPattern {
		for mn := range mnemonics[pat] {
			f.Mnemonics = append(f.Mnemonics, mn)
		}
		sort.Strings(f.Mnemonics)
		rep.Families = append(rep.Families, *f)
	}

	// Ranked by sites, which is the order work should be done in. Ties broken
	// by name so two runs of the same input produce identical bytes.
	sort.Slice(rep.Families, func(i, j int) bool {
		if rep.Families[i].Sites != rep.Families[j].Sites {
			return rep.Families[i].Sites > rep.Families[j].Sites
		}
		return rep.Families[i].Pattern < rep.Families[j].Pattern
	})
	sort.Slice(rep.Detail, func(i, j int) bool {
		if rep.Detail[i].Sites != rep.Detail[j].Sites {
			return rep.Detail[i].Sites > rep.Detail[j].Sites
		}
		return rep.Detail[i].Enc < rep.Detail[j].Enc
	})
	return rep
}

// Write renders the report. top limits the per-encoding section; <= 0 means all.
func (rep *Report) Write(w io.Writer) error {
	b := &strings.Builder{}

	fmt.Fprintf(b, "undecoded instruction report\n")
	if rep.Source != "" {
		fmt.Fprintf(b, "  source: %s\n", rep.Source)
	}
	fmt.Fprintf(b, "  %s sites, %s distinct encodings\n",
		comma(rep.Sites), comma(rep.Encodings))
	fmt.Fprintf(b, "  %s padding sites (enc=0x00000000), excluded above and kept as a control\n",
		comma(rep.PaddingSites))
	if rep.Unmatched > 0 {
		fmt.Fprintf(b, "  %s sites matched no decodetree pattern -- see the <unmatched> family\n",
			comma(rep.Unmatched))
	}
	if rep.dis == nil {
		fmt.Fprintf(b, "  (no disassembler available: mnemonics omitted)\n")
	}

	fmt.Fprintf(b, "\nby family, ranked by sites:\n")
	for _, f := range rep.Families {
		name := f.Pattern
		if name == "" {
			name = "<unmatched>"
		}
		line := fmt.Sprintf("  %-22s %8s sites  %4s encodings", name, comma(f.Sites), comma(f.Encodings))
		if len(f.Mnemonics) > 0 {
			line += "   " + strings.Join(f.Mnemonics, ", ")
		}
		fmt.Fprintln(b, line)
	}

	fmt.Fprintf(b, "\nby encoding, ranked by sites:\n")
	for _, e := range rep.Detail {
		fmt.Fprintf(b, "\n  enc=%#08x  sites=%s  funcs=%s  first-vma=%#x\n",
			e.Enc, comma(e.Sites), comma(e.Funcs), e.FirstVMA)
		if l, ok := rep.dis[e.Enc]; ok {
			fmt.Fprintf(b, "    objdump: %s\n", l.Text)
		}
		m := rep.dec.Match(e.Enc)
		if m == nil {
			fmt.Fprintf(b, "    no decodetree pattern matches this encoding\n")
			continue
		}
		for _, line := range strings.Split(strings.TrimRight(m.Describe(), "\n"), "\n") {
			fmt.Fprintf(b, "    %s\n", line)
		}
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// comma formats n with thousands separators, matching how TODO.md writes these
// counts so the two can be compared at a glance.
func comma(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	out := strings.Join(parts, ",")
	if neg {
		out = "-" + out
	}
	return out
}
