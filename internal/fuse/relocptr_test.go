package fuse

import "testing"

// The musl __stdout_write case, reduced. Its address appears ONLY as the
// initialiser of a FILE's `write` slot, so the only trace of it in the image is
// a relative relocation -- no symbol, no FDE, no adrp/add. Encodings are from
// the fused redis image.
func TestRelocTargetFuncsRecoversAStaticInitialiserPointer(t *testing.T) {
	const sec = uint64(0x1000)
	target := uint64(0x1478)
	insns := map[uint64][]uint32{
		target - 4: {0x17ffffe9},             // b -- ends the previous function
		target:     {0xaa0003e3, 0xa9be7bfd}, // mov x3, x0 ; stp x29, x30, [sp, #-32]!
		0x1600:     {0xa9bd7bfd},             // a known function, bounding it
	}
	known := []funcRange{{addr: 0x1600, size: 8}}
	got := relocTargetFuncs([]uint64{target}, buildExec(sec, 0x1000, insns), known)
	if len(got) != 1 || got[0].addr != target {
		t.Fatalf("recovered %v, want the relocated pointer at %#x", got, target)
	}
	if want := uint64(0x1600 - 0x1478); got[0].size != want {
		t.Errorf("size %d, want %d", got[0].size, want)
	}
}

// The rejecting direction. A relocation is strong evidence, but not strong
// enough to override a boundary that is already known, and not enough to
// declare a function start in the middle of straight-line code.
func TestRelocTargetFuncsRejects(t *testing.T) {
	const sec = uint64(0x1000)
	insns := map[uint64][]uint32{
		0x1400: {0xd65f03c0},             // ret
		0x1404: {0xa9bd7bfd, 0x910003fd}, // a plausible function start
		0x1500: {0x910003fd},             // mid-function: mov x29, sp
		0x1504: {0xa9bd7bfd},
	}
	ranges := buildExec(sec, 0x1000, insns)

	for name, tc := range map[string]struct {
		target uint64
		known  []funcRange
	}{
		// Data. The overwhelming majority of relative relocations point at
		// other data, and none of them may become a function.
		"outside every executable section": {0x9000, nil},
		// A jump-table entry, or any interior label: the enclosing function is
		// known, so the relocation must not split it.
		"inside a known function": {0x1404, []funcRange{{addr: 0x1400, size: 0x100}}},
		"exactly a known start":   {0x1404, []funcRange{{addr: 0x1404, size: 0x10}}},
		// Straight-line code: whatever precedes a function has to end one.
		"not preceded by a terminator": {0x1504, nil},
	} {
		if got := relocTargetFuncs([]uint64{tc.target}, ranges, tc.known); len(got) != 0 {
			t.Errorf("%s: recovered %v, want nothing", name, got)
		}
	}

	// Zero is what an unrelocated or absent slot reads as, and it is in no
	// section at all.
	if got := relocTargetFuncs([]uint64{0}, ranges, nil); len(got) != 0 {
		t.Errorf("a null pointer became a function: %v", got)
	}
}

// Alignment padding between two functions is not "straight-line code", so a
// function that a linker pushed onto the next 8- or 16-byte boundary must still
// be recoverable. Without this, whether a boundary is found depends on how much
// padding the linker happened to insert.
func TestRelocTargetFuncsAcceptsAfterPadding(t *testing.T) {
	const sec = uint64(0x1000)
	for name, prev := range map[string]uint32{
		"nop":       0xd503201f,
		"zero fill": 0,
	} {
		target := uint64(0x1408)
		insns := map[uint64][]uint32{
			target - 4: {prev},
			target:     {0xa9bd7bfd},
			0x1500:     {0xa9bd7bfd},
		}
		got := relocTargetFuncs([]uint64{target},
			buildExec(sec, 0x1000, insns), []funcRange{{addr: 0x1500, size: 8}})
		if len(got) != 1 || got[0].addr != target {
			t.Errorf("%s padding: recovered %v, want %#x", name, got, target)
		}
	}
}

// Two relocated starts must bound each other, not both run to the next known
// symbol. This is the half that actually fixed redis: __stdio_write was already
// recovered and claimed 540 bytes, which SWALLOWED __stdout_write -- and an
// indirect call into another function's interior is exactly the failure being
// removed, so recovering the second start without shrinking the first would
// have changed nothing.
func TestRelocTargetFuncsBoundEachOther(t *testing.T) {
	const sec = uint64(0x1000)
	a, b := uint64(0x1380), uint64(0x1478)
	insns := map[uint64][]uint32{
		a - 4:  {0xd65f03c0},
		a:      {0xa9ba7bfd},
		b - 4:  {0x17ffffe9},
		b:      {0xaa0003e3},
		0x1600: {0xa9bd7bfd},
	}
	got := relocTargetFuncs([]uint64{a, b},
		buildExec(sec, 0x1000, insns), []funcRange{{addr: 0x1600, size: 8}})
	if len(got) != 2 {
		t.Fatalf("recovered %v, want both %#x and %#x", got, a, b)
	}
	byAddr := map[uint64]uint64{}
	for _, fr := range got {
		byAddr[fr.addr] = fr.size
	}
	if want := b - a; byAddr[a] != want {
		t.Errorf("the first function claims %d bytes, want %d -- it must stop at %#x",
			byAddr[a], want, b)
	}
	if want := uint64(0x1600) - b; byAddr[b] != want {
		t.Errorf("the second function claims %d bytes, want %d", byAddr[b], want)
	}
}
