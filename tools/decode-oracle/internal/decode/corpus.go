// Copyright 2026 The raptormark Authors
// SPDX-License-Identifier: LGPL-2.1-or-later

package decode

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"strings"
)

// CorpusResult is the outcome of decoding every word of a real binary with both
// the decodetree table and an independent disassembler.
//
// This is the differential that decides whether the vendored pin is good enough
// to trust. Its power comes from scale and from independence: the fused
// fixtures carry millions of instructions that nobody labelled by hand, and
// objdump's opinion of them was formed by a completely separate decoder.
//
// The comparison is deliberately on COVERAGE, not on names. Mapping decodetree
// pattern names to objdump mnemonics would need an alias table, and building
// one by hand is the very activity that produced the 83 false positives
// .agents/docs/TODO.md warns about. "Did both decoders think this word was an
// instruction?" needs no such table and is exactly the question that matters.
type CorpusResult struct {
	Path     string
	Sections []string

	// Words is every 4-byte word in executable sections, including data and
	// padding that happens to live there. Distinct is how many were unique.
	Words    int
	Distinct int

	// Counted by SITE, not by distinct encoding, so a rare pattern cannot
	// dominate the rate.
	BothDecoded  int
	BothRejected int
	// TableOnly: the table matched, objdump did not. Usually harmless -- a
	// pattern may be architecturally allocated but not implemented by this
	// binutils -- but worth seeing.
	TableOnly int
	// ObjdumpOnly is the gap that matters: objdump decoded it and the vendored
	// table did not. A high count means the pin is too old or the parser is
	// dropping patterns.
	ObjdumpOnly int
	// DisasmFailed counts words the disassembler CRASHED on rather than
	// declining. These are excluded from the coverage ratio entirely: a
	// binutils assertion failure is the absence of a second opinion, and
	// silently counting it either way would move the headline number for a
	// reason that has nothing to do with the decode table.
	DisasmFailed int

	// Gaps lists the ObjdumpOnly mnemonics by site count, worst first.
	Gaps []Gap
	// Mapping records the (pattern, mnemonic) pairs observed, worst first. Not
	// asserted on; it is the raw material for anyone who later wants an alias
	// table, and it makes the alias trap visible rather than theoretical.
	Mapping []PatternMnemonic
}

// Gap is one mnemonic objdump decodes and the table does not.
type Gap struct {
	Mnemonic string
	Sites    int
	Example  uint32
}

// PatternMnemonic is one observed correspondence.
type PatternMnemonic struct {
	Pattern  string
	Mnemonic string
	Sites    int
}

// Coverage is the fraction of words both decoders agreed were instructions,
// out of those objdump could decode. It is the headline number.
func (r *CorpusResult) Coverage() float64 {
	den := r.BothDecoded + r.ObjdumpOnly
	if den == 0 {
		return 1
	}
	return float64(r.BothDecoded) / float64(den)
}

// RunCorpus decodes every executable word of an ELF with both decoders.
func RunCorpus(path string, dec *Decoder, dis *Disasm) (*CorpusResult, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	res := &CorpusResult{Path: path}
	counts := map[uint32]int{}

	for _, s := range f.Sections {
		// Executable PROGBITS only. SHT_NOBITS has no bytes to read, and a
		// non-executable section is data by declaration.
		if s.Type != elf.SHT_PROGBITS || s.Flags&elf.SHF_EXECINSTR == 0 {
			continue
		}
		data, err := s.Data()
		if err != nil {
			return nil, fmt.Errorf("reading section %s: %w", s.Name, err)
		}
		res.Sections = append(res.Sections, s.Name)
		for i := 0; i+4 <= len(data); i += 4 {
			counts[binary.LittleEndian.Uint32(data[i:])]++
			res.Words++
		}
	}
	if res.Words == 0 {
		return nil, fmt.Errorf("%s: no executable PROGBITS sections", path)
	}
	res.Distinct = len(counts)

	encs := make([]uint32, 0, len(counts))
	for e := range counts {
		encs = append(encs, e)
	}
	lines, err := dis.Decode(encs)
	if err != nil {
		return nil, err
	}

	gaps := map[string]*Gap{}
	pairs := map[PatternMnemonic]int{}

	for enc, n := range counts {
		m := dec.Match(enc)
		l, haveLine := lines[enc]
		if !haveLine || l.Failed {
			res.DisasmFailed += n
			continue
		}
		objOK := !l.Undefined

		switch {
		case m != nil && objOK:
			res.BothDecoded += n
			pairs[PatternMnemonic{Pattern: m.Insn.Name, Mnemonic: l.Mnemonic}] += n
		case m != nil && !objOK:
			res.TableOnly += n
		case m == nil && objOK:
			res.ObjdumpOnly += n
			g := gaps[l.Mnemonic]
			if g == nil {
				g = &Gap{Mnemonic: l.Mnemonic, Example: enc}
				gaps[l.Mnemonic] = g
			}
			g.Sites += n
		default:
			res.BothRejected += n
		}
	}

	for _, g := range gaps {
		res.Gaps = append(res.Gaps, *g)
	}
	sort.Slice(res.Gaps, func(i, j int) bool {
		if res.Gaps[i].Sites != res.Gaps[j].Sites {
			return res.Gaps[i].Sites > res.Gaps[j].Sites
		}
		return res.Gaps[i].Mnemonic < res.Gaps[j].Mnemonic
	})

	for pm, n := range pairs {
		pm.Sites = n
		res.Mapping = append(res.Mapping, pm)
	}
	sort.Slice(res.Mapping, func(i, j int) bool {
		if res.Mapping[i].Sites != res.Mapping[j].Sites {
			return res.Mapping[i].Sites > res.Mapping[j].Sites
		}
		if res.Mapping[i].Pattern != res.Mapping[j].Pattern {
			return res.Mapping[i].Pattern < res.Mapping[j].Pattern
		}
		return res.Mapping[i].Mnemonic < res.Mapping[j].Mnemonic
	})

	return res, nil
}

// Write renders the corpus differential. topGaps and topPairs limit the two
// tail sections; <= 0 means all.
func (r *CorpusResult) Write(w io.Writer, topGaps, topPairs int) error {
	b := &strings.Builder{}
	fmt.Fprintf(b, "decode differential: %s\n", r.Path)
	fmt.Fprintf(b, "  sections: %s\n", strings.Join(r.Sections, " "))
	fmt.Fprintf(b, "  %s words, %s distinct encodings\n", comma(r.Words), comma(r.Distinct))
	fmt.Fprintf(b, "\n  by site:\n")
	fmt.Fprintf(b, "    both decoded      %12s\n", comma(r.BothDecoded))
	fmt.Fprintf(b, "    both rejected     %12s   (data, padding, genuinely undefined)\n", comma(r.BothRejected))
	fmt.Fprintf(b, "    decodetree only   %12s   (allocated but not known to this binutils)\n", comma(r.TableOnly))
	fmt.Fprintf(b, "    objdump only      %12s   <- the gap that matters\n", comma(r.ObjdumpOnly))
	if r.DisasmFailed > 0 {
		fmt.Fprintf(b, "    disassembler crashed %9s   (no second opinion; excluded from coverage)\n",
			comma(r.DisasmFailed))
	}
	fmt.Fprintf(b, "\n  coverage: %.4f%% of objdump-decodable words also match a decodetree pattern\n",
		100*r.Coverage())

	if len(r.Gaps) > 0 {
		fmt.Fprintf(b, "\n  objdump-only mnemonics, worst first:\n")
		for i, g := range r.Gaps {
			if topGaps > 0 && i >= topGaps {
				fmt.Fprintf(b, "    ... and %d more\n", len(r.Gaps)-topGaps)
				break
			}
			fmt.Fprintf(b, "    %-16s %10s sites   e.g. %#08x\n", g.Mnemonic, comma(g.Sites), g.Example)
		}
	}

	if len(r.Mapping) > 0 {
		fmt.Fprintf(b, "\n  observed pattern -> mnemonic correspondences (informational):\n")
		for i, pm := range r.Mapping {
			if topPairs > 0 && i >= topPairs {
				fmt.Fprintf(b, "    ... and %d more\n", len(r.Mapping)-topPairs)
				break
			}
			fmt.Fprintf(b, "    %-22s %-14s %10s sites\n", pm.Pattern, pm.Mnemonic, comma(pm.Sites))
		}
	}

	_, err := io.WriteString(w, b.String())
	return err
}
