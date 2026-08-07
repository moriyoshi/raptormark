// Copyright 2026 The raptormark Authors
// SPDX-License-Identifier: LGPL-2.1-or-later

package decode

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ReportCmd is the decode-report command.
//
// Two modes, deliberately in one command because they answer two halves of the
// same question:
//
//   - Log turns elflift's `[ecv-undecoded]` output into a ranked worklist.
//     This is the day-to-day use.
//   - Corpus decodes a real fused binary with both the decode tables and
//     objdump and reports where they disagree. This is what says the worklist
//     can be trusted at all.
//
// ⚠️ Harvesting a log needs RAPTORMARK_TRANSLATE_VERBOSE=1 *and a cold object
// cache*. A cache hit means no translate ran, so the report is empty for a
// reason that has nothing to do with this tool.
//
// Plain fields rather than kong tags: this module is deliberately
// dependency-free (see go.mod), so cmd/decode-report wires these from stdlib
// flags. AGENTS.md's "keep new commands consistent with the kong layout" rule
// is about cmd/raptormark, which this is no longer part of.
type ReportCmd struct {
	// Log is a translate log to read [ecv-undecoded] lines from; "-" is stdin.
	Log string
	// Corpus is a fused ELF to run the decodetree/objdump differential over.
	Corpus string
	// Enc is a comma-separated list of raw hex encodings to name directly.
	// The same capability the MCP decode_encoding tool exposes -- both render
	// decode.LookupEncodings, so a wrong answer is wrong in both or neither.
	Enc string

	// Objdump is the disassembler used as the independent cross-check. It must
	// target aarch64.
	Objdump string
	// Top limits the per-encoding and tail sections; 0 means all.
	Top int
	// JSON emits JSON instead of text, for diffing two runs.
	JSON bool
}

func (c *ReportCmd) Run() error {
	n := 0
	for _, set := range []bool{c.Log != "", c.Corpus != "", c.Enc != ""} {
		if set {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("exactly one of -log, -corpus or -enc is required")
	}

	dec, err := AArch64()
	if err != nil {
		return fmt.Errorf("parsing the vendored decode tables: %w", err)
	}
	// A table that fails its own invariants would produce a confident, wrong
	// worklist. Refuse rather than report.
	if probs := dec.Validate(); len(probs) != 0 {
		for i, p := range probs {
			if i >= 10 {
				fmt.Fprintf(os.Stderr, "  ... and %d more\n", len(probs)-10)
				break
			}
			fmt.Fprintf(os.Stderr, "  %s\n", p)
		}
		return fmt.Errorf("a vendored decode table failed validation (%d problems); "+
			"re-vendor or fix the parser before trusting a report", len(probs))
	}

	dis := &Disasm{Tool: c.Objdump}

	if c.Enc != "" {
		var encs []uint32
		for _, raw := range strings.Split(c.Enc, ",") {
			if strings.TrimSpace(raw) == "" {
				continue
			}
			e, err := ParseEncoding(raw)
			if err != nil {
				return err
			}
			encs = append(encs, e)
		}
		if len(encs) == 0 {
			return fmt.Errorf("-enc: no encodings given")
		}
		ls := LookupEncodings(dec, dis, encs)
		if c.JSON {
			return writeJSON(ls)
		}
		var b strings.Builder
		WriteLookups(&b, ls)
		_, err := os.Stdout.WriteString(b.String())
		return err
	}

	if c.Corpus != "" {
		if !dis.Available() {
			return fmt.Errorf("-corpus needs %q on PATH: it is the independent half of the differential", c.Objdump)
		}
		res, err := RunCorpus(c.Corpus, dec, dis)
		if err != nil {
			return err
		}
		if c.JSON {
			return writeJSON(res)
		}
		return res.Write(os.Stdout, c.Top, c.Top)
	}

	src := c.Log
	in := os.Stdin
	if c.Log != "-" {
		f, err := os.Open(c.Log)
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	} else {
		src = "(stdin)"
	}

	encs, err := ParseLog(in)
	if err != nil {
		return err
	}
	if len(encs) == 0 {
		return fmt.Errorf("%s: no [ecv-undecoded] lines found. "+
			"The report needs RAPTORMARK_TRANSLATE_VERBOSE=1 and a COLD object cache "+
			"-- a cache hit means no translate ran", src)
	}

	// The disassembler is a nicety here rather than a requirement: it supplies
	// mnemonics so the report can be read against TODO.md's counts. Without it
	// the worklist is still correct.
	var lines map[uint32]Line
	if dis.Available() {
		keys := make([]uint32, 0, len(encs))
		for e := range encs {
			keys = append(keys, e)
		}
		if lines, err = dis.Decode(keys); err != nil {
			fmt.Fprintf(os.Stderr, "decode-report: %s unavailable (%v); continuing without mnemonics\n", c.Objdump, err)
			lines = nil
		}
	}

	rep := Aggregate(src, encs, dec, lines)
	if c.Top > 0 && len(rep.Detail) > c.Top {
		// Say what was dropped. A silently truncated list reads as "that is
		// everything", which is how a coverage gap survives a review.
		fmt.Fprintf(os.Stderr, "decode-report: showing the top %d of %d encodings (-top)\n",
			c.Top, len(rep.Detail))
		rep.Detail = rep.Detail[:c.Top]
	}
	if c.JSON {
		return writeJSON(rep)
	}
	return rep.Write(os.Stdout)
}

func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
