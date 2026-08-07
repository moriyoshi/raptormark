package fuse

import (
	"encoding/binary"
	"testing"
)

// The TLSDESC resolver stub must be exactly `ldr x0, [x0, #8]` then `ret`. The
// bytes are asserted by DECODING them rather than by comparing to the same
// constant the code uses, which would only prove the constant equals itself.
//
// An instruction encoding written by hand is wrong more often than not in this
// tree, and this one is load-bearing in a way that fails quietly: a wrong
// immediate still produces a valid image that lifts and links, and only misreads
// thread-local storage at run time.
func TestTLSDescStubEncoding(t *testing.T) {
	if len(tlsdescStub) != 8 {
		t.Fatalf("stub is %d bytes, want 8", len(tlsdescStub))
	}
	ldr := binary.LittleEndian.Uint32(tlsdescStub[0:])
	ret := binary.LittleEndian.Uint32(tlsdescStub[4:])

	// LDR (immediate, unsigned offset), 64-bit: size=0b11, V=0, opc=0b01,
	// i.e. the top 10 bits are 1111100101, then imm12, Rn, Rt.
	if got := ldr >> 22; got != 0x3e5 {
		t.Errorf("first word %#x is not a 64-bit LDR (unsigned offset): top10=%#x, want %#x",
			ldr, got, 0x3e5)
	}
	// The immediate is scaled by the access size, so #8 encodes as 1.
	if imm := (ldr >> 10) & 0xfff; imm != 1 {
		t.Errorf("LDR immediate encodes #%d, want #8 (imm12=1, got %d)", imm*8, imm)
	}
	if rn := (ldr >> 5) & 0x1f; rn != 0 {
		t.Errorf("LDR base is x%d, want x0 (the descriptor address)", rn)
	}
	if rt := ldr & 0x1f; rt != 0 {
		t.Errorf("LDR destination is x%d, want x0 (the returned offset)", rt)
	}
	if ret != 0xd65f03c0 {
		t.Errorf("second word %#x, want RET (x30) %#x", ret, 0xd65f03c0)
	}
}

// A descriptor is two words and their order is the whole contract: the guest
// loads word 0 and branches to it, so getting them backwards jumps to a small
// integer.
func TestApplyTLSDescFixupsWritesResolverThenOffset(t *testing.T) {
	const base = 0x1000
	o := &Object{
		Name:  "libtest.so",
		lo:    base,
		hi:    base + 0x100,
		image: make([]byte, 0x100),
	}
	const stub = 0x7423000
	fixups := []tlsdescFixup{
		{o: o, off: base + 0x10, tpOff: 0x1c8},
		{o: o, off: base + 0x20, tpOff: 0x50},
	}
	if err := applyTLSDescFixups(fixups, stub); err != nil {
		t.Fatalf("applyTLSDescFixups: %v", err)
	}
	for _, want := range []struct {
		off   uint64
		res   uint64
		tpOff uint64
	}{
		{0x10, stub, 0x1c8},
		{0x20, stub, 0x50},
	} {
		gotRes := binary.LittleEndian.Uint64(o.image[want.off:])
		gotArg := binary.LittleEndian.Uint64(o.image[want.off+8:])
		if gotRes != want.res {
			t.Errorf("descriptor at %#x: resolver word = %#x, want %#x", want.off, gotRes, want.res)
		}
		if gotArg != want.tpOff {
			t.Errorf("descriptor at %#x: argument word = %#x, want %#x", want.off, gotArg, want.tpOff)
		}
	}
	// Nothing outside the two descriptors may be touched.
	for i, b := range o.image {
		off := uint64(i)
		in := (off >= 0x10 && off < 0x20) || (off >= 0x20 && off < 0x30)
		if !in && b != 0 {
			t.Fatalf("byte %#x = %#x, want untouched", off, b)
		}
	}
}

// A descriptor that would spill past the object's image must be reported, not
// silently written into whatever follows.
func TestApplyTLSDescFixupsRejectsOutOfRange(t *testing.T) {
	const base = 0x1000
	o := &Object{
		Name:  "libtest.so",
		lo:    base,
		hi:    base + 0x20,
		image: make([]byte, 0x20),
	}
	// Room for the resolver word but not the argument that follows it.
	err := applyTLSDescFixups([]tlsdescFixup{{o: o, off: base + 0x18, tpOff: 1}}, 0x7423000)
	if err == nil {
		t.Fatal("writing a descriptor that overruns the image succeeded, want an error")
	}
}
