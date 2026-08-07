// Copyright 2026 The raptormark Authors
// SPDX-License-Identifier: LGPL-2.1-or-later

package decode

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Disasm names encodings with an external disassembler.
//
// It exists as a CROSS-CHECK, not as a source of truth, and the distinction is
// the whole reason this package does not simply shell out to objdump and stop.
// objdump prints ALIASES -- `cmp` for SUBS, `mov` for ORR, `tst` for ANDS, `lsl`
// for UBFM -- and .agents/docs/TODO.md records that taking those mnemonics at
// face value produced 83 false positives when matched against elfconv's stub
// decoders. A mnemonic is a rendering; the decodetree pattern is the decode.
//
// What the disassembler IS good for is an independent opinion on whether an
// encoding decodes at all, which is what the corpus differential uses it for,
// and a human-readable label beside each entry in a worklist.
type Disasm struct {
	// Tool is the objdump binary. Empty means "objdump" from PATH.
	Tool string
}

// Line is one disassembled encoding.
type Line struct {
	Enc uint32
	// Text is the full rendering, e.g. "st1 {v0.16b}, [x0], #16".
	Text string
	// Mnemonic is the first token of Text.
	Mnemonic string
	// Undefined is set when the disassembler could not decode the word at all
	// (objdump renders these as `.inst 0x... ; undefined`, and the all-zero
	// word as `udf #0`).
	Undefined bool
	// Failed is set when the disassembler CRASHED on this encoding rather than
	// declining it. binutils 2.42 aborts on an assertion in
	// aarch64-dis.c:get_sreg_qualifier_from_value for some words present in
	// .agents-workspace/fixtures/aptget-glibc.fused.
	//
	// It is distinct from Undefined on purpose: "objdump says this is not an
	// instruction" is evidence, and "objdump fell over" is the absence of
	// evidence. Folding the second into the first would let a binutils bug
	// silently inflate this package's apparent coverage.
	Failed bool
}

// objdumpLine matches "   1c:\t4c000c00 \tst4\t{v0.2d-v3.2d}, [x0]".
var objdumpLine = regexp.MustCompile(`^\s*[0-9a-f]+:\s+([0-9a-f]{8})\s+(.*)$`)

// Decode disassembles the given encodings.
//
// The encodings are written to one flat little-endian blob and disassembled in
// a single objdump run, so naming 474 distinct encodings costs one process
// rather than 474. Duplicates in encs are fine; the result is keyed by value.
func (d *Disasm) Decode(encs []uint32) (map[uint32]Line, error) {
	if len(encs) == 0 {
		return map[uint32]Line{}, nil
	}

	seen := map[uint32]bool{}
	var uniq []uint32
	for _, e := range encs {
		if !seen[e] {
			seen[e] = true
			uniq = append(uniq, e)
		}
	}

	dir, err := os.MkdirTemp("", "raptormark-disasm-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	out := make(map[uint32]Line, len(uniq))
	if err := d.decodeChunk(dir, uniq, out); err != nil {
		return nil, err
	}
	return out, nil
}

// decodeChunk disassembles encs, bisecting around a crash.
//
// binutils can abort on a specific word (see Line.Failed). One poisonous
// encoding must not cost the differential the other three million, so a failing
// run is split and retried until the offending encodings are isolated to
// singletons and marked. Cost is O(k log n) objdump runs for k bad encodings,
// and k is single digits in practice.
func (d *Disasm) decodeChunk(dir string, encs []uint32, out map[uint32]Line) error {
	if len(encs) == 0 {
		return nil
	}

	stdout, runErr, fatal := d.run(dir, encs)
	if fatal != nil {
		return fatal
	}

	if runErr == nil {
		d.scan(stdout, out)
		return nil
	}

	if len(encs) == 1 {
		// objdump could not survive this word. Record that, rather than
		// inferring anything about the encoding from the crash.
		out[encs[0]] = Line{Enc: encs[0], Text: "<disassembler crashed>", Failed: true}
		return nil
	}

	mid := len(encs) / 2
	if err := d.decodeChunk(dir, encs[:mid], out); err != nil {
		return err
	}
	return d.decodeChunk(dir, encs[mid:], out)
}

// run writes encs to a blob and disassembles it. The second return is the
// disassembler's own failure (recoverable by bisection); the third is a failure
// to even attempt it (not recoverable).
func (d *Disasm) run(dir string, encs []uint32) (*bytes.Buffer, error, error) {
	blob := make([]byte, 4*len(encs))
	for i, e := range encs {
		binary.LittleEndian.PutUint32(blob[4*i:], e)
	}
	path := filepath.Join(dir, "raw.bin")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		return nil, nil, err
	}

	tool := d.Tool
	if tool == "" {
		tool = "objdump"
	}
	// -b binary -m aarch64 disassembles a headerless blob, which is what lets
	// this name encodings that were never in an ELF.
	cmd := exec.Command(tool, "-D", "-b", "binary", "-m", "aarch64", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return &stdout, fmt.Errorf("%s: %w: %s", tool, err, strings.TrimSpace(stderr.String())), nil
		}
		// Could not start it at all: a missing binary is not something
		// bisection can help with.
		return nil, nil, fmt.Errorf("%s: %w", tool, err)
	}
	return &stdout, nil, nil
}

// scan parses objdump's listing into out.
func (d *Disasm) scan(stdout *bytes.Buffer, out map[uint32]Line) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		m := objdumpLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		v, err := strconv.ParseUint(m[1], 16, 32)
		if err != nil {
			continue
		}
		text := strings.Join(strings.Fields(strings.ReplaceAll(m[2], "\t", " ")), " ")
		l := Line{Enc: uint32(v), Text: text}
		if i := strings.IndexByte(text, ' '); i > 0 {
			l.Mnemonic = text[:i]
		} else {
			l.Mnemonic = text
		}
		// objdump's two ways of saying "I cannot decode this".
		l.Undefined = l.Mnemonic == ".inst" || l.Mnemonic == "udf" ||
			strings.HasSuffix(text, "; undefined")
		out[l.Enc] = l
	}
}

// Available reports whether the disassembler can be run at all, so callers can
// degrade to a decodetree-only report instead of failing.
func (d *Disasm) Available() bool {
	tool := d.Tool
	if tool == "" {
		tool = "objdump"
	}
	_, err := exec.LookPath(tool)
	return err == nil
}
