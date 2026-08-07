package fuse

import "testing"

// The two selectors that split a closure's deferred fixups between the main
// image and each unit.
//
// They are tested here, apart from FuseWithUnits, because they are the parts
// that can be wrong WITHOUT anything downstream noticing. A misfiled ifunc
// fixup does not fail a build: it puts a `.ecv.irela` entry in an image whose
// memory does not contain that GOT slot, and `apply_ifuncs` then writes a
// pointer into whatever else occupies the address. The end-to-end behaviour of
// FuseWithUnits needs real ELFs (emit reads o.file.Sections) and is covered by
// `.agents-workspace/drivers/unitfuse`.

// span builds an object occupying [base+lo, base+hi).
func span(name string, base, lo, hi uint64) *Object {
	return &Object{Path: "/" + name, Name: name, LibIndex: 0, Base: base, lo: lo, hi: hi}
}

func TestIfuncsWithinSelectsBySlotAddress(t *testing.T) {
	a := span("a.so", 0x100000, 0, 0x1000) // [0x100000, 0x101000)
	b := span("b.so", 0x200000, 0, 0x1000) // [0x200000, 0x201000)

	all := []ifuncFixup{
		{slot: 0x100000, resolver: 1}, // a, first byte
		{slot: 0x100fff, resolver: 2}, // a, last byte
		{slot: 0x101000, resolver: 3}, // just past a: belongs to NEITHER
		{slot: 0x200800, resolver: 4}, // b
		{slot: 0x0ffff8, resolver: 5}, // just below a
	}

	got := ifuncsWithin([]*Object{a}, all)
	if len(got) != 2 {
		t.Fatalf("a should take exactly its own 2 slots, got %d: %v", len(got), got)
	}
	for _, f := range got {
		if f.slot < 0x100000 || f.slot >= 0x101000 {
			t.Errorf("slot %#x is outside a's span but was assigned to it", f.slot)
		}
	}
	// The boundaries are the point: half-open, so the byte AT hi is excluded.
	for _, f := range got {
		if f.slot == 0x101000 {
			t.Error("the slot at a's exclusive upper bound was included")
		}
	}

	gotB := ifuncsWithin([]*Object{b}, all)
	if len(gotB) != 1 || gotB[0].resolver != 4 {
		t.Errorf("b should take exactly slot 0x200800, got %v", gotB)
	}

	// Nothing may be assigned to two images, and nothing that belongs to an
	// image may be dropped. 5 fixtures, 2 of which belong to no object here.
	if n := len(got) + len(gotB); n != 3 {
		t.Errorf("a and b together took %d of 5 fixups; expected exactly 3", n)
	}
}

func TestTlsdescsOfSelectsByObjectIdentity(t *testing.T) {
	a := span("a.so", 0x100000, 0, 0x1000)
	b := span("b.so", 0x200000, 0, 0x1000)
	// Deliberately identical geometry to `a`: selection must be by IDENTITY, not
	// by address, or two objects fused from the same file would take each
	// other's descriptors.
	twin := span("a.so", 0x100000, 0, 0x1000)

	all := []tlsdescFixup{
		{o: a, off: 0x10, tpOff: 1},
		{o: b, off: 0x20, tpOff: 2},
		{o: twin, off: 0x30, tpOff: 3},
		{o: a, off: 0x40, tpOff: 4},
	}

	got := tlsdescsOf([]*Object{a}, all)
	if len(got) != 2 {
		t.Fatalf("a owns 2 of the 4 descriptors, got %d", len(got))
	}
	for _, f := range got {
		if f.o != a {
			t.Errorf("descriptor at off %#x belongs to %p, not to a (%p)", f.off, f.o, a)
		}
		if f.tpOff == 3 {
			t.Error("took the twin's descriptor: selection fell back to address, not identity")
		}
	}
	if len(tlsdescsOf([]*Object{twin}, all)) != 1 {
		t.Error("the twin owns exactly its own descriptor")
	}
	if len(tlsdescsOf(nil, all)) != 0 {
		t.Error("no objects should select no descriptors")
	}
}
