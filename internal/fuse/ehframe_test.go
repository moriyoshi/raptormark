package fuse

import (
	"debug/elf"
	"encoding/binary"
	"os"
	"testing"
)

// ehFrameBuilder assembles a `.eh_frame` the way a real toolchain does, so the
// parser is exercised against the encoding it will actually meet rather than a
// convenient one.
type ehFrameBuilder struct {
	buf     []byte
	secAddr uint64
}

func (b *ehFrameBuilder) entry(id uint32, body func(off int) []byte) int {
	start := len(b.buf)
	b.buf = append(b.buf, 0, 0, 0, 0) // length, patched below
	idOff := len(b.buf)
	b.buf = binary.LittleEndian.AppendUint32(b.buf, id)
	b.buf = append(b.buf, body(idOff)...)
	binary.LittleEndian.PutUint32(b.buf[start:], uint32(len(b.buf)-start-4))
	return start
}

// cie writes the aarch64-typical CIE: version 1, augmentation "zR", FDE
// pointers encoded pcrel|sdata4.
func (b *ehFrameBuilder) cie(fdeEnc byte) int {
	return b.entry(0, func(int) []byte {
		var e []byte
		e = append(e, 1)                // version
		e = append(e, 'z', 'R', 0)      // augmentation
		e = append(e, 1)                // code alignment factor (ULEB)
		e = append(e, 0x78)             // data alignment factor (SLEB, -8)
		e = append(e, 30)               // return address register
		e = append(e, 1)                // augmentation data length (ULEB)
		e = append(e, fdeEnc)           // 'R': FDE pointer encoding
		e = append(e, 0x0c, 0x1f, 0x00) // DW_CFA_def_cfa sp, 0 — ignored
		return e
	})
}

// fde writes an FDE for [pc, pc+size) referring back to the CIE at cieOff.
func (b *ehFrameBuilder) fde(cieOff int, pc, size uint64) {
	b.entry(0, func(idOff int) []byte {
		binary.LittleEndian.PutUint32(b.buf[idOff:], uint32(idOff-cieOff))
		var e []byte
		// pcrel|sdata4: stored value is the delta from the field's own address.
		field := b.secAddr + uint64(len(b.buf))
		e = binary.LittleEndian.AppendUint32(e, uint32(int32(pc-field)))
		e = binary.LittleEndian.AppendUint32(e, uint32(size))
		e = append(e, 0x0e, 0x10) // DW_CFA_def_cfa_offset 16 — ignored
		return e
	})
}

func TestParseEhFramePCRelSData4(t *testing.T) {
	b := &ehFrameBuilder{secAddr: 0x8000}
	cie := b.cie(pePCRel | peSData4)
	b.fde(cie, 0x1000, 0x40)
	b.fde(cie, 0x2000, 0x118)
	b.buf = append(b.buf, 0, 0, 0, 0) // terminator

	got := parseEhFrame(b.buf, b.secAddr)
	want := []funcRange{{0x1000, 0x40}, {0x2000, 0x118}}
	if len(got) != len(want) {
		t.Fatalf("got %d ranges, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("range %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// An absolute encoding must not be silently treated as pc-relative: getting
// this wrong yields addresses that look plausible and are wrong everywhere.
func TestParseEhFrameAbsolute(t *testing.T) {
	b := &ehFrameBuilder{secAddr: 0x8000}
	cie := b.entry(0, func(int) []byte {
		return []byte{1, 'z', 'R', 0, 1, 0x78, 30, 1, peAbs | peUData8}
	})
	b.entry(0, func(idOff int) []byte {
		binary.LittleEndian.PutUint32(b.buf[idOff:], uint32(idOff-cie))
		var e []byte
		e = binary.LittleEndian.AppendUint64(e, 0x4000)
		e = binary.LittleEndian.AppendUint64(e, 0x20)
		return e
	})
	got := parseEhFrame(b.buf, b.secAddr)
	if len(got) != 1 || got[0] != (funcRange{0x4000, 0x20}) {
		t.Fatalf("got %+v, want [{0x4000 0x20}]", got)
	}
}

// A truncated section must yield what parsed cleanly, not panic and not invent
// entries — .eh_frame is attacker-controlled input in the general case.
func TestParseEhFrameTruncated(t *testing.T) {
	b := &ehFrameBuilder{secAddr: 0x8000}
	cie := b.cie(pePCRel | peSData4)
	b.fde(cie, 0x1000, 0x40)
	full := append([]byte(nil), b.buf...)

	for cut := len(full) - 1; cut > 0; cut-- {
		got := parseEhFrame(full[:cut], b.secAddr) // must not panic
		for _, r := range got {
			if r.addr != 0x1000 || r.size != 0x40 {
				t.Fatalf("cut=%d invented %+v", cut, r)
			}
		}
	}
}

func TestParseEhFrameEmpty(t *testing.T) {
	if got := parseEhFrame(nil, 0); got != nil {
		t.Errorf("nil data: got %+v, want nil", got)
	}
	if got := parseEhFrame([]byte{0, 0, 0, 0}, 0); got != nil {
		t.Errorf("terminator only: got %+v, want nil", got)
	}
}

func TestCovered(t *testing.T) {
	sorted := []funcRange{{0x100, 0x10}, {0x200, 0x40}, {0x400, 0x4}}
	cases := []struct {
		addr uint64
		want bool
	}{
		{0x0ff, false},
		{0x100, true},  // first byte
		{0x10f, true},  // last byte
		{0x110, false}, // one past the end
		{0x220, true},
		{0x240, false},
		{0x403, true},
		{0x404, false},
		{0x9999, false},
	}
	for _, c := range cases {
		if got := covered(sorted, c.addr); got != c.want {
			t.Errorf("covered(%#x) = %v, want %v", c.addr, got, c.want)
		}
	}
	if covered(nil, 0x100) {
		t.Error("covered(nil) must be false")
	}
}

// Validates the parser against a real stripped distro library when one is
// available. Skips otherwise, so the suite stays hermetic.
func TestEhFrameAgainstRealLibrary(t *testing.T) {
	path := os.Getenv("RAPTORMARK_EHFRAME_LIB")
	if path == "" {
		t.Skip("set RAPTORMARK_EHFRAME_LIB to a real aarch64 .so to run")
	}
	f, err := elf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	ranges := ehFrameFuncs(f)
	if len(ranges) == 0 {
		t.Fatal("no FDEs recovered")
	}

	var execBytes uint64
	for _, s := range f.Sections {
		if s.Flags&elf.SHF_EXECINSTR != 0 && s.Type == elf.SHT_PROGBITS {
			execBytes += s.Size
		}
	}
	var covered uint64
	for _, r := range ranges {
		covered += r.size
		// Every FDE must name an address inside some executable section.
		in := false
		for _, s := range f.Sections {
			if s.Flags&elf.SHF_EXECINSTR != 0 && r.addr >= s.Addr && r.addr < s.Addr+s.Size {
				in = true
				break
			}
		}
		if !in {
			t.Fatalf("FDE at %#x is outside every executable section", r.addr)
		}
	}
	pct := 100 * float64(covered) / float64(execBytes)
	t.Logf("%s: %d FDEs covering %d/%d exec bytes (%.1f%%)", path, len(ranges), covered, execBytes, pct)
	if pct < 50 {
		t.Errorf("coverage %.1f%% is implausibly low for a C library", pct)
	}
}
