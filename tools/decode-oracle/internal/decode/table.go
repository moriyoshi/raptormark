// Copyright 2026 The raptormark Authors
// SPDX-License-Identifier: LGPL-2.1-or-later

package decode

import (
	"fmt"
	"sync"

	qemudecode "raptormark/tools/decode-oracle/third_party/qemu-decode"
)

// Decoder is an ordered set of decode tables, tried in sequence.
//
// AArch64 is not one decoder. QEMU dispatches
//
//	if (!disas_a64(s, insn) && !disas_sme(s, insn) && !disas_sve(s, insn))
//	        unallocated_encoding(s);
//
// (target/arm/tcg/translate-a64.c:11200 at the vendored pin), and the tables
// behind those three calls are allowed to overlap ONE ANOTHER -- only patterns
// within a single table are required to be mutually exclusive. Merging them
// into one list would therefore be wrong in a way that produces a confident
// answer from the wrong decoder, so they are kept separate and tried in QEMU's
// order.
//
// A consequence worth relying on: adding a later table can only turn a NoMatch
// into a match. It can never change an answer an earlier table already gave.
type Decoder struct {
	// Tables in dispatch order. Earlier wins.
	Tables []*Table
}

// Match returns the first table's answer, in dispatch order.
func (d *Decoder) Match(enc uint32) *Match {
	for _, t := range d.Tables {
		if m := t.Match(enc); m != nil {
			return m
		}
	}
	return nil
}

// Validate checks each table's own invariants.
//
// Deliberately per-table: a cross-table overlap is legal and expected, so
// checking for one would report the architecture as broken.
func (d *Decoder) Validate() []Problem {
	var out []Problem
	for _, t := range d.Tables {
		out = append(out, t.Validate()...)
	}
	return out
}

// Names returns the distinct pattern names across all tables, sorted.
func (d *Decoder) Names() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range d.Tables {
		for _, n := range t.Names() {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

var (
	once    sync.Once
	decoder *Decoder
	loadErr error
)

// AArch64 returns the aarch64 decoder: a64.decode then sve.decode, matching
// QEMU's dispatch order.
//
// SME is not vendored. It was not observed in any measured gap over the fused
// fixtures, and its absence shows up honestly as a NoMatch rather than as a
// wrong answer.
//
// Built once and shared. The tables are read-only after Parse, so sharing is
// safe, and the corpus differential calls Match a few million times.
func AArch64() (*Decoder, error) {
	once.Do(func() {
		a64, err := Parse("a64.decode", qemudecode.A64)
		if err != nil {
			loadErr = fmt.Errorf("a64.decode: %w", err)
			return
		}
		sve, err := Parse("sve.decode", qemudecode.SVE)
		if err != nil {
			loadErr = fmt.Errorf("sve.decode: %w", err)
			return
		}
		decoder = &Decoder{Tables: []*Table{a64, sve}}
	})
	return decoder, loadErr
}

// A64 returns just the base A64 table.
//
// Kept for tests that need to reason about one table in isolation -- notably
// Validate, which is only meaningful per-table. Callers decoding real
// instructions want AArch64.
func A64() (*Table, error) {
	d, err := AArch64()
	if err != nil {
		return nil, err
	}
	return d.Tables[0], nil
}

// SVE returns just the SVE table, on the same terms as A64.
func SVE() (*Table, error) {
	d, err := AArch64()
	if err != nil {
		return nil, err
	}
	return d.Tables[1], nil
}
