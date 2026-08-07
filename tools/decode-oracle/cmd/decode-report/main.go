// Copyright 2026 The raptormark Authors
// SPDX-License-Identifier: LGPL-2.1-or-later

// Command decode-report names the aarch64 encodings elflift could not decode.
//
// It reads elflift's `[ecv-undecoded]` report (elfconv patch 0057) and, for
// each encoding, reports the QEMU decodetree pattern that matches it together
// with every operand's bit position, width and value. The point is to remove
// the hand-derived-mask step that AGENTS.md records as "wrong more often than
// not".
//
// # Why this is a separate program
//
// It embeds QEMU's decodetree tables, which are LGPL-2.1-or-later, and is
// licensed the same way. raptormark's lifter lineage (third_party/elfconv,
// remill, LLVM) is Apache-2.0, which conflicts with GPLv2 and LGPLv2.1
// specifically -- not with the GPL family at large; LGPLv3 was written to be
// Apache-compatible, and the `-or-later` grant puts it within reach.
//
// The split is what keeps that choice open rather than forcing it: this tool is
// never built into, linked with, or shipped alongside the pipeline. It is a
// developer-only analysis tool that reads a log and prints a report, and
// nothing it touches reaches a translated object or module.wasm. See
// ../../go.mod and ../../README.md for the full reasoning.
//
// # Usage
//
//	decode-report -enc 0x4c9f7000          # name one encoding, with its fields
//	decode-report -log <translate log>     # the worklist
//	decode-report -corpus <fused ELF>      # the decodetree/objdump differential
//
// ⚠️ Harvesting a log needs RAPTORMARK_TRANSLATE_VERBOSE=1 *and a cold object
// cache*. A cache hit means no translate ran, so the report comes out empty for
// a reason that has nothing to do with this tool.
package main

import (
	"flag"
	"fmt"
	"os"

	"raptormark/tools/decode-oracle/internal/decode"
)

func main() {
	var c decode.ReportCmd

	flag.StringVar(&c.Log, "log", "", "translate log to read [ecv-undecoded] lines from; \"-\" reads stdin")
	flag.StringVar(&c.Corpus, "corpus", "", "fused ELF to run the decodetree/objdump differential over")
	flag.StringVar(&c.Enc, "enc", "", "comma-separated hex encodings to name directly, e.g. 0x4c9f7000,4e020020")
	flag.StringVar(&c.Objdump, "objdump", "objdump", "disassembler used as the independent cross-check; must target aarch64")
	flag.IntVar(&c.Top, "top", 0, "limit the per-encoding and tail sections to N entries; 0 means all")
	flag.BoolVar(&c.JSON, "json", false, "emit JSON instead of text, for diffing two runs")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "decode-report: name the aarch64 encodings elflift could not decode.\n\n")
		fmt.Fprintf(os.Stderr, "  decode-report -enc 0x4c9f7000        name encodings, with their fields\n")
		fmt.Fprintf(os.Stderr, "  decode-report -log <translate log>   ranked worklist, with fields\n")
		fmt.Fprintf(os.Stderr, "  decode-report -corpus <fused ELF>    decodetree vs objdump differential\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if err := c.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "decode-report: %s\n", err)
		// 2 for a bad invocation, 1 for a step that failed -- matching the
		// convention cmd/raptormark uses.
		if c.Log == "" && c.Corpus == "" && c.Enc == "" {
			flag.Usage()
			os.Exit(2)
		}
		os.Exit(1)
	}
}
