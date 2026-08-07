// Copyright 2026 The raptormark Authors
// SPDX-License-Identifier: LGPL-2.1-or-later

// Command decode-mcp serves the decode oracle over MCP's stdio transport.
//
// The oracle's real consumer is a coding agent working through elfconv's
// instruction-coverage debt, not a human at a prompt. Over MCP it is a typed
// tool with a schema rather than a binary to shell out to and a text format to
// re-parse, and the tool descriptions carry the two caveats that otherwise cost
// a wrong conclusion -- that objdump prints aliases, and that `enc=0x00000000`
// is padding rather than an instruction.
//
// Register it with a client as, from the repo root:
//
//	{"mcpServers": {"decode-oracle": {
//	  "command": "go",
//	  "args": ["run", "./cmd/decode-mcp"],
//	  "cwd": "tools/decode-oracle"
//	}}}
//
// ⚠️ stdout belongs to the protocol. The MCP spec requires that a stdio server
// write nothing to stdout that is not a valid message, so this binary prints
// diagnostics to stderr only, and the server never calls the decode package's
// os.Stdout-writing entry points. Do not add a Println here.
package main

import (
	"flag"
	"fmt"
	"os"

	"raptormark/tools/decode-oracle/internal/mcp"
)

func main() {
	objdump := flag.String("objdump", "objdump",
		"disassembler used as the independent cross-check; must target aarch64")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr,
			"decode-mcp: serve the decode oracle over the MCP stdio transport.\n\n"+
				"Not meant to be run by hand -- an MCP client launches it as a subprocess and\n"+
				"speaks newline-delimited JSON-RPC over stdin/stdout. For interactive use, see\n"+
				"decode-report.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	// Tables are parsed and validated here so a bad pin fails the launch, where
	// a client reports it, rather than answering every tool call confidently
	// and wrongly.
	s, err := mcp.New(os.Stdin, os.Stdout, os.Stderr, *objdump)
	if err != nil {
		fmt.Fprintf(os.Stderr, "decode-mcp: %s\n", err)
		os.Exit(1)
	}
	if err := s.Serve(); err != nil {
		fmt.Fprintf(os.Stderr, "decode-mcp: %s\n", err)
		os.Exit(1)
	}
}
