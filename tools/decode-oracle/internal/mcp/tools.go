// Copyright 2026 The raptormark Authors
// SPDX-License-Identifier: LGPL-2.1-or-later

package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"raptormark/tools/decode-oracle/internal/decode"
)

// version is reported in serverInfo. Bump when the tool surface changes in a
// way a client could notice.
const version = "0.1.0"

// instructions is returned from initialize. It is the server's chance to tell
// a model the two things that otherwise cost it a wrong conclusion.
const instructions = `Names aarch64 instruction encodings that elfconv's lifter could not decode,
using QEMU's decodetree tables, and extracts their operand fields.

Two things to know before acting on a result:

  * A mnemonic is not a decodetree pattern. objdump prints ALIASES -- "cmp" for
    SUBS, "mov" for ORR and for SVE SEL -- so never match work against mnemonics.
    The raw encoding is the unit of truth. "st1" also splits across ST_mult and
    ST_single.
  * enc=0x00000000 is padding lifted as code, not an instruction. It dominates a
    raw report and is excluded from totals, but is reported separately because
    it doubles as a control: if it moves between two runs, function-boundary
    recovery moved and not just instruction coverage.

This tool NAMES encodings and extracts fields. It carries no instruction
semantics and cannot tell you whether an implementation would be correct.`

func builtinTools() []tool {
	return []tool{
		{
			Name:  "decode_encoding",
			Title: "Decode aarch64 encodings",
			Description: "Name one or more raw 32-bit aarch64 encodings and extract every operand " +
				"field, with the decodetree pattern, its fixedmask/fixedbits, and the independent " +
				"objdump rendering. This is the tool to reach for when looking at an " +
				"[ecv-undecoded] line from a translate log.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"encodings": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": `Hex encodings, with or without an 0x prefix, e.g. ["0x4c9f7000","4e020020"].`,
					},
				},
				"required": []string{"encodings"},
			},
			call: (*Server).decodeEncoding,
		},
		{
			Name:  "decode_report",
			Title: "Worklist from a translate log",
			Description: "Read elflift's [ecv-undecoded] records from a translate log and return a " +
				"worklist ranked by site count, grouped by decodetree pattern, with the padding " +
				"count reported separately as a control. Harvesting such a log needs " +
				"RAPTORMARK_TRANSLATE_VERBOSE=1 and a COLD object cache.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Path to the translate log.",
					},
					"top": map[string]any{
						"type":        "integer",
						"description": "Limit the per-encoding section to N entries; 0 or absent means all.",
					},
				},
				"required": []string{"path"},
			},
			call: (*Server).decodeReport,
		},
		{
			Name:  "decode_corpus",
			Title: "Decodetree vs objdump differential",
			Description: "Decode every executable word of a fused ELF with both the vendored " +
				"decodetree tables and objdump, and report where they disagree. This is the check " +
				"that says whether the pinned tables can be trusted at all; run it after " +
				"re-vendoring, against one glibc and one musl fixture.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Path to a fused ELF.",
					},
					"top": map[string]any{
						"type":        "integer",
						"description": "Limit the gap and correspondence tails to N entries; 0 or absent means all.",
					},
				},
				"required": []string{"path"},
			},
			call: (*Server).decodeCorpus,
		},
	}
}

func (s *Server) disasm() *decode.Disasm { return &decode.Disasm{Tool: s.Objdump} }

func (s *Server) decodeEncoding(args json.RawMessage) *toolResult {
	var a struct {
		Encodings []string `json:"encodings"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return errResult("invalid arguments: %v", err)
	}
	if len(a.Encodings) == 0 {
		return errResult("no encodings given; pass e.g. {\"encodings\":[\"0x4c9f7000\"]}")
	}

	encs := make([]uint32, 0, len(a.Encodings))
	for _, raw := range a.Encodings {
		e, err := decode.ParseEncoding(raw)
		if err != nil {
			// A bad argument is a tool execution error the model can fix by
			// retrying, not a protocol error.
			return errResult("%v", err)
		}
		encs = append(encs, e)
	}

	ls := decode.LookupEncodings(s.dec, s.disasm(), encs)
	var b strings.Builder
	decode.WriteLookups(&b, ls)
	return textResult(b.String(), map[string]any{"lookups": ls})
}

func (s *Server) decodeReport(args json.RawMessage) *toolResult {
	var a struct {
		Path string `json:"path"`
		Top  int    `json:"top"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return errResult("invalid arguments: %v", err)
	}
	if a.Path == "" {
		return errResult("no path given")
	}

	f, err := os.Open(a.Path)
	if err != nil {
		return errResult("%v", err)
	}
	defer f.Close()

	encs, err := decode.ParseLog(f)
	if err != nil {
		return errResult("%v", err)
	}
	if len(encs) == 0 {
		return errResult("%s: no [ecv-undecoded] lines found. The report needs "+
			"RAPTORMARK_TRANSLATE_VERBOSE=1 and a COLD object cache -- a cache hit means no "+
			"translate ran, so an empty result here says nothing about coverage", a.Path)
	}

	dis := s.disasm()
	var lines map[uint32]decode.Line
	if dis.Available() {
		keys := make([]uint32, 0, len(encs))
		for e := range encs {
			keys = append(keys, e)
		}
		if lines, err = dis.Decode(keys); err != nil {
			s.logf("%s unavailable (%v); continuing without mnemonics", s.Objdump, err)
			lines = nil
		}
	}

	rep := decode.Aggregate(a.Path, encs, s.dec, lines)
	dropped := 0
	if a.Top > 0 && len(rep.Detail) > a.Top {
		dropped = len(rep.Detail) - a.Top
		rep.Detail = rep.Detail[:a.Top]
	}

	var b strings.Builder
	if err := rep.Write(&b); err != nil {
		return errResult("%v", err)
	}
	if dropped > 0 {
		// Never let a truncation read as "that is everything".
		fmt.Fprintf(&b, "\n(%d further encodings omitted by top=%d)\n", dropped, a.Top)
	}
	return textResult(b.String(), rep)
}

func (s *Server) decodeCorpus(args json.RawMessage) *toolResult {
	var a struct {
		Path string `json:"path"`
		Top  int    `json:"top"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return errResult("invalid arguments: %v", err)
	}
	if a.Path == "" {
		return errResult("no path given")
	}

	dis := s.disasm()
	if !dis.Available() {
		return errResult("decode_corpus needs %q on PATH: it is the independent half of the "+
			"differential and there is nothing to compare against without it", s.Objdump)
	}
	res, err := decode.RunCorpus(a.Path, s.dec, dis)
	if err != nil {
		return errResult("%v", err)
	}

	var b strings.Builder
	if err := res.Write(&b, a.Top, a.Top); err != nil {
		return errResult("%v", err)
	}
	return textResult(b.String(), res)
}
