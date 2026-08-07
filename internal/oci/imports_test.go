package oci

import (
	"slices"
	"strings"
	"testing"
)

// wasmHeader is `\0asm` plus version 1, which every module below starts with.
var wasmHeader = []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

func uleb(v uint64) []byte {
	var out []byte
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			c |= 0x80
		}
		out = append(out, c)
		if v == 0 {
			return out
		}
	}
}

func name(s string) []byte { return append(uleb(uint64(len(s))), s...) }

// section wraps a payload in an id + size header.
func section(id byte, payload []byte) []byte {
	out := []byte{id}
	out = append(out, uleb(uint64(len(payload)))...)
	return append(out, payload...)
}

// importSection builds section 2 from raw per-import descriptor bytes, so a test
// can hand it a shape the parser is supposed to REJECT as easily as one it
// accepts.
func importSection(entries ...[]byte) []byte {
	payload := uleb(uint64(len(entries)))
	for _, e := range entries {
		payload = append(payload, e...)
	}
	return section(2, payload)
}

func imp(mod, fld string, desc ...byte) []byte {
	out := append(name(mod), name(fld)...)
	return append(out, desc...)
}

func module(parts ...[]byte) []byte {
	out := slices.Clone(wasmHeader)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestModuleImportsReadsEveryKind(t *testing.T) {
	m := module(importSection(
		imp("wasi_snapshot_preview1", "sock_open", 0x00, 0x07),                // func, typeidx 7
		imp("env", "__indirect_function_table", 0x01, 0x70, 0x01, 0x02, 0x09), // table, reftype, limits 2..9
		imp("env", "memory", 0x02, 0x00, 0x01),                                // memory, limits min 1
		imp("env", "__stack_pointer", 0x03, 0x7f, 0x01),                       // global i32 mut
	))
	got, err := ModuleImports(m)
	if err != nil {
		t.Fatalf("ModuleImports: %v", err)
	}
	want := []Import{
		{"wasi_snapshot_preview1", "sock_open"},
		{"env", "__indirect_function_table"},
		{"env", "memory"},
		{"env", "__stack_pointer"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

// A module that needs nothing from its host is legal, and must not read as an
// error -- otherwise the guard cannot tell "imports nothing" from "unparseable".
func TestModuleWithNoImportSection(t *testing.T) {
	got, err := ModuleImports(module(section(1, []byte{0x00}))) // an empty type section
	if err != nil {
		t.Fatalf("ModuleImports: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}

// The section is found wherever it is, not only immediately after the header.
func TestImportSectionIsFoundAfterOtherSections(t *testing.T) {
	m := module(
		section(0, append(name("producers"), 0xaa, 0xbb)), // a custom section
		section(1, []byte{0x00}),
		importSection(imp("wasi_snapshot_preview1", "fd_write", 0x00, 0x00)),
	)
	got, err := ModuleImports(m)
	if err != nil || len(got) != 1 || got[0].Field != "fd_write" {
		t.Fatalf("got %v, %v", got, err)
	}
}

// ⚠️ THE PROPERTY THIS PARSER EXISTS FOR. Every caller asks "is the import set
// still what we expect", and a SHORT list is the answer that passes. So a
// descriptor the parser does not understand has to be an error: giving up
// quietly would report a prefix, and a prefix reads as a portability
// improvement.
func TestAnUnknownDescriptorIsAnErrorNotAnEarlyStop(t *testing.T) {
	m := module(importSection(
		imp("wasi_snapshot_preview1", "fd_write", 0x00, 0x00),
		imp("wasi_snapshot_preview1", "mystery", 0x09), // no such import kind
		imp("wasi_snapshot_preview1", "sock_bind", 0x00, 0x00),
	))
	got, err := ModuleImports(m)
	if err == nil {
		t.Fatalf("an unknown kind must not parse; got %v", got)
	}
	if !strings.Contains(err.Error(), "mystery") {
		t.Errorf("the error must name the import it choked on, got %v", err)
	}
}

// A limits flag beyond 0x01 means memory64 or shared memory, i.e. a proposal a
// raptormark module is not supposed to carry. Meeting one is a finding, so it
// is reported rather than skipped past.
func TestALimitsFlagBeyondWasm2IsRejected(t *testing.T) {
	m := module(importSection(imp("env", "memory", 0x02, 0x03, 0x01, 0x02)))
	_, err := ModuleImports(m)
	if err == nil || !strings.Contains(err.Error(), "proposal") {
		t.Errorf("want a proposal complaint, got %v", err)
	}
}

func TestNotAWasmModule(t *testing.T) {
	for _, b := range [][]byte{nil, []byte("hello"), append([]byte("\x00asm"), 0x09, 0, 0, 0)} {
		if _, err := ModuleImports(b); err == nil {
			t.Errorf("%q parsed as a module", b)
		}
	}
}

// A count that overstates how many imports follow must fail, not return what it
// managed to read.
func TestATruncatedImportSectionIsAnError(t *testing.T) {
	payload := append(uleb(5), imp("env", "one", 0x00, 0x00)...)
	if _, err := ModuleImports(module(section(2, payload))); err == nil {
		t.Error("a short import section must be an error")
	}
}

// Trailing bytes inside the section mean the parser and the producer disagree
// about a descriptor's length -- which is the silent way to mis-read the list.
func TestTrailingBytesInTheSectionAreAnError(t *testing.T) {
	payload := append(uleb(1), imp("env", "one", 0x00, 0x00)...)
	payload = append(payload, 0xff, 0xff)
	if _, err := ModuleImports(module(section(2, payload))); err == nil {
		t.Error("trailing bytes must be an error")
	}
}

func TestImportSetIsSorted(t *testing.T) {
	m := module(importSection(
		imp("wasi_snapshot_preview1", "sock_open", 0x00, 0x00),
		imp("wasi_snapshot_preview1", "fd_write", 0x00, 0x00),
		imp("env", "memory", 0x02, 0x00, 0x01),
	))
	got, err := ImportSet(m)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"env.memory",
		"wasi_snapshot_preview1.fd_write",
		"wasi_snapshot_preview1.sock_open",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func exportSection(entries ...[]byte) []byte {
	payload := uleb(uint64(len(entries)))
	for _, e := range entries {
		payload = append(payload, e...)
	}
	return section(7, payload)
}

func exp(name string, kind byte, idx uint64) []byte {
	out := append(uleb(uint64(len(name))), name...)
	return append(append(out, kind), uleb(idx)...)
}

func TestModuleExportsReadsEveryKind(t *testing.T) {
	m := module(exportSection(
		exp("ecv_program_0", 0x03, 4), // a GLOBAL, which is how a data symbol exports
		exp("__wasm_call_ctors", 0x00, 14),
		exp("memory", 0x02, 0),
		exp("__indirect_function_table", 0x01, 0),
	))
	got, err := ModuleExports(m)
	if err != nil {
		t.Fatal(err)
	}
	want := []Export{
		{"__indirect_function_table", "table"},
		{"__wasm_call_ctors", "func"},
		{"ecv_program_0", "global"},
		{"memory", "memory"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

// The KIND matters, not just the name: `ecv_program_0` is a global holding the
// descriptor's ADDRESS. A reader that reported it as a func would send an
// embedder looking for something to call.
func TestAnUnknownExportKindIsAnError(t *testing.T) {
	if _, err := ModuleExports(module(exportSection(exp("x", 0x09, 0)))); err == nil {
		t.Error("an unknown export kind must not parse")
	}
}

func TestAModuleWithNoExportsIsNotAnError(t *testing.T) {
	got, err := ModuleExports(module(section(1, []byte{0x00})))
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}

// dylink.0 MEM_INFO is the placement contract. Its absence is the test for
// "this is a flat module", so it must be reported as absent rather than as an
// error -- the two mean opposite things to a caller.
func TestSideModuleNeedsReadsMemInfo(t *testing.T) {
	memInfo := append([]byte{0x01}, uleb(4+1+1+1)...) // subsection 1, size
	memInfo = append(memInfo, uleb(443939)...)
	memInfo = append(memInfo, uleb(4)...) // log2(16)
	memInfo = append(memInfo, uleb(901)...)
	memInfo = append(memInfo, uleb(0)...)
	m := module(section(0, append(name("dylink.0"), memInfo...)))
	got, ok, err := SideModuleNeeds(m)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	want := SideNeeds{MemSize: 443939, MemAlignLog2: 4, TableSize: 901, TableAlignLog2: 0}
	if got != want {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestAFlatModuleReportsNoSideNeeds(t *testing.T) {
	_, ok, err := SideModuleNeeds(module(section(1, []byte{0x00})))
	if err != nil {
		t.Fatalf("a flat module must not be an error: %v", err)
	}
	if ok {
		t.Error("a module with no dylink.0 must report false")
	}
}
