// Copyright 2026 The raptormark Authors
// SPDX-License-Identifier: LGPL-2.1-or-later

package decode

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// sampleLog is shaped like a real translate log: the report lines are
// interleaved with unrelated container output, because
// RAPTORMARK_TRANSLATE_VERBOSE=1 forwards every container's stderr onto one
// stream. A parser anchored on line start would find nothing here.
const sampleLog = `
translate-one: lifting /usr/lib/aarch64-linux-gnu/libcrypto.so.3
[ecv-undecoded] vma=0x4a1234 enc=0x4c9f7000 fn=0x4a1000
clang: warning: argument unused during compilation
[ecv-undecoded] vma=0x4a1238 enc=0x4c9f7000 fn=0x4a1000
[ecv-undecoded] vma=0x4b0000 enc=0x4c9f7000 fn=0x4b0000
[ecv-undecoded] vma=0x4a2000 enc=0x4e020020 fn=0x4a1000
[ecv-undecoded] vma=0x4a3000 enc=0x00000000 fn=0x4a3000
[ecv-undecoded] vma=0x4a3004 enc=0x00000000 fn=0x4a3000
2026-08-14T09:00:00Z [ecv-undecoded] vma=0x4a4000 enc=0xffffffff fn=0x4a4000
translate-one: done
`

func TestParseLogGroupsByEncoding(t *testing.T) {
	encs, err := ParseLog(strings.NewReader(sampleLog))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(encs), 4; got != want {
		t.Fatalf("got %d distinct encodings, want %d", got, want)
	}

	st1 := encs[0x4c9f7000]
	if st1 == nil {
		t.Fatal("0x4c9f7000 missing")
	}
	if st1.Sites != 3 {
		t.Errorf("sites = %d, want 3", st1.Sites)
	}
	// Two functions, not three: two of the three sites share fn=0x4a1000.
	// Collapsing this to the site count would hide "one function is the whole
	// problem", which is a different kind of finding.
	if st1.Funcs != 2 {
		t.Errorf("funcs = %d, want 2", st1.Funcs)
	}
	if st1.FirstVMA != 0x4a1234 {
		t.Errorf("first vma = %#x, want 0x4a1234", st1.FirstVMA)
	}
	// The timestamped line must still be found.
	if encs[0xffffffff] == nil {
		t.Error("the timestamp-prefixed line was not parsed")
	}
}

func TestAggregateSeparatesPaddingAndUnmatched(t *testing.T) {
	dec, err := AArch64()
	if err != nil {
		t.Fatal(err)
	}
	encs, err := ParseLog(strings.NewReader(sampleLog))
	if err != nil {
		t.Fatal(err)
	}
	rep := Aggregate("test", encs, dec, nil)

	// Padding is excluded from the totals but kept as a control.
	if rep.PaddingSites != 2 {
		t.Errorf("padding sites = %d, want 2", rep.PaddingSites)
	}
	if rep.Sites != 5 {
		t.Errorf("sites = %d, want 5 (3 st1 + 1 tbl + 1 undefined)", rep.Sites)
	}
	if rep.Encodings != 3 {
		t.Errorf("encodings = %d, want 3", rep.Encodings)
	}
	// 0xffffffff decodes to nothing.
	if rep.Unmatched != 1 {
		t.Errorf("unmatched = %d, want 1", rep.Unmatched)
	}

	// Ranked by sites: ST_mult (3) before TBL_TBX (1) and <unmatched> (1).
	if len(rep.Families) != 3 {
		t.Fatalf("got %d families, want 3: %+v", len(rep.Families), rep.Families)
	}
	if rep.Families[0].Pattern != "ST_mult" || rep.Families[0].Sites != 3 {
		t.Errorf("top family = %+v, want ST_mult with 3 sites", rep.Families[0])
	}

	var b bytes.Buffer
	if err := rep.Write(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"ST_mult", "TBL_TBX", "<unmatched>",
		"2 padding sites",
		"no decodetree pattern matches this encoding",
		"rpt=1", "selem=1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
}

func TestAggregateIsDeterministic(t *testing.T) {
	// Go randomises map iteration, and the report is meant to be diffed
	// between two lifts. Rendering it twice must give identical bytes.
	dec, err := AArch64()
	if err != nil {
		t.Fatal(err)
	}
	render := func() string {
		encs, err := ParseLog(strings.NewReader(sampleLog))
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if err := Aggregate("test", encs, dec, nil).Write(&b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	first := render()
	for i := 0; i < 10; i++ {
		if got := render(); got != first {
			t.Fatalf("report is not deterministic across runs")
		}
	}
}

func TestComma(t *testing.T) {
	for _, c := range []struct {
		in   int
		want string
	}{
		{0, "0"}, {999, "999"}, {1000, "1,000"}, {2805, "2,805"},
		{8159, "8,159"}, {1234567, "1,234,567"}, {-1000, "-1,000"},
	} {
		if got := comma(c.in); got != c.want {
			t.Errorf("comma(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Disassembler bridge and the corpus differential.
// ---------------------------------------------------------------------------

func TestDisasmNamesRealEncodings(t *testing.T) {
	d := &Disasm{}
	if !d.Available() {
		t.Skip("objdump not on PATH")
	}
	lines, err := d.Decode([]uint32{
		0x4c9f7000, 0x4e020020, 0x6f615420, 0x00000000, 0xffffffff, 0x8b020020,
		0x4c9f7000, // a duplicate: must not disturb the result
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		enc       uint32
		mnemonic  string
		undefined bool
	}{
		{0x4c9f7000, "st1", false},
		{0x4e020020, "tbl", false},
		{0x6f615420, "sli", false},
		{0x8b020020, "add", false},
		// objdump's two ways of declining a word. Both must be recognised, or
		// the corpus differential counts undecodable padding as an instruction
		// objdump knew and this package did not.
		{0x00000000, "udf", true},
		{0xffffffff, ".inst", true},
	} {
		l, ok := lines[c.enc]
		if !ok {
			t.Errorf("%#08x: no line", c.enc)
			continue
		}
		if l.Mnemonic != c.mnemonic {
			t.Errorf("%#08x: mnemonic = %q, want %q", c.enc, l.Mnemonic, c.mnemonic)
		}
		if l.Undefined != c.undefined {
			t.Errorf("%#08x: undefined = %v, want %v (%q)", c.enc, l.Undefined, c.undefined, l.Text)
		}
		if l.Failed {
			t.Errorf("%#08x: reported as a disassembler crash", c.enc)
		}
	}
}

// TestDisasmSurvivesACrashingWord guards the bisection.
//
// binutils 2.42 aborts on an assertion in aarch64-dis.c for some words that
// occur in aptget-glibc.fused. Before bisection, one such word cost the whole
// 1.6-million-word differential. This asserts the recovery WITHOUT depending on
// binutils still having the bug: it uses a fake tool that crashes on demand.
func TestDisasmSurvivesACrashingWord(t *testing.T) {
	// A shell stand-in for objdump: it reads the blob, and aborts if the
	// poisoned word is present; otherwise it prints a plausible listing.
	dir := t.TempDir()
	tool := dir + "/fake-objdump"
	script := `#!/bin/sh
# args: -D -b binary -m aarch64 <path>; the blob is the LAST one.
for f; do :; done
if od -An -tx4 -v "$f" | tr -s ' ' '\n' | grep -qx deadbeef; then
  echo "fake-objdump: assertion failed" >&2
  exit 134
fi
i=0
od -An -tx4 -v "$f" | tr -s ' ' '\n' | grep -v '^$' | while read w; do
  printf '%8x:\t%s \tfake\top\n' "$i" "$w"
  i=$((i+4))
done
`
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	d := &Disasm{Tool: tool}
	encs := []uint32{0x11111111, 0xdeadbeef, 0x22222222, 0x33333333, 0x44444444}
	lines, err := d.Decode(encs)
	if err != nil {
		t.Fatalf("Decode gave up entirely: %v", err)
	}
	if got, want := len(lines), len(encs); got != want {
		t.Fatalf("got %d lines, want %d", got, want)
	}
	if l := lines[0xdeadbeef]; !l.Failed {
		t.Errorf("the poisoned word was not marked Failed: %+v", l)
	}
	for _, e := range []uint32{0x11111111, 0x22222222, 0x33333333, 0x44444444} {
		l := lines[e]
		if l.Failed {
			t.Errorf("%#08x was lost to the neighbouring crash", e)
		}
		if l.Mnemonic != "fake" {
			t.Errorf("%#08x: mnemonic = %q, want %q", e, l.Mnemonic, "fake")
		}
	}
}

// TestCorpusAgreesWithObjdump is the differential that decides whether the
// vendored pin can be trusted.
//
// Env-gated because the fixtures live under .agents-workspace/, which is
// gitignored and not present on a clean checkout. It skips cleanly rather than
// failing, so `go test ./...` stays Docker-free, network-free and fixture-free.
//
// The path must be ABSOLUTE: `go test` runs each package in its own source
// directory, so a repo-relative path resolves against internal/decode/.
//
//	RAPTORMARK_DECODE_CORPUS=$PWD/.agents-workspace/fixtures/postgres-glibc.fused \
//	  go test ./internal/decode/ -run TestCorpusAgreesWithObjdump -v
//
// Measured 2026-08-14 at QEMU v11.1.0, binutils 2.42, aarch64 host, with both
// a64.decode and sve.decode vendored:
//
//	busybox-musl     268,891 words   100.0000%      0 gaps
//	bash-glibc       527,547 words   100.0000%      0 gaps
//	aptget-glibc   1,613,201 words    99.9985%     24 gaps, all SME
//	postgres-glibc 4,899,857 words    99.9990%     50 gaps, all SME
//
// With a64.decode ALONE the same fixtures read 100.0000% / 99.9619% / 99.9830%
// / 99.9690%, and every one of those 1,979 gaps was SVE -- glibc's SVE ifunc
// variants of the string and memory routines. Vendoring sve.decode closed them.
//
// The residual SME is not chased. Several of its examples are repeated-byte
// words (0x80808000, 0xe0e000e0, 0xc000c0c0), which is the signature of data
// lifted as code rather than of real SME.
//
// The floor is set at 99.9% rather than at the measured value: the point is to
// catch a pin or a parser that has lost whole families, not to freeze a number
// that legitimately moves with binutils.
func TestCorpusAgreesWithObjdump(t *testing.T) {
	path := os.Getenv("RAPTORMARK_DECODE_CORPUS")
	if path == "" {
		t.Skip("set RAPTORMARK_DECODE_CORPUS=<fused ELF> to run the differential")
	}
	dis := &Disasm{}
	if !dis.Available() {
		t.Skip("objdump not on PATH")
	}
	dec, err := AArch64()
	if err != nil {
		t.Fatal(err)
	}
	res, err := RunCorpus(path, dec, dis)
	if err != nil {
		t.Fatal(err)
	}

	var b bytes.Buffer
	if err := res.Write(&b, 20, 0); err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + b.String())

	if got := res.Coverage(); got < 0.999 {
		t.Errorf("coverage %.4f%% is below the 99.9%% floor; the pin is likely too old "+
			"or the parser has lost patterns", 100*got)
	}
	if res.BothDecoded == 0 {
		t.Error("nothing decoded at all -- is this an aarch64 binary?")
	}
}
