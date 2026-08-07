package fuse

import (
	"debug/elf"
	"encoding/binary"
	"testing"
)

// entries decodes `.ecv.funcs` back into {addr, size} pairs so a test asserts on
// what the lifter would read, not on the byte layout it happens to have.
func entries(t *testing.T, b []byte) [][2]uint64 {
	t.Helper()
	if len(b)%16 != 0 {
		t.Fatalf("table is %d bytes, not a multiple of the 16-byte entry", len(b))
	}
	out := make([][2]uint64, 0, len(b)/16)
	for i := 0; i < len(b); i += 16 {
		out = append(out, [2]uint64{
			binary.LittleEndian.Uint64(b[i:]),
			binary.LittleEndian.Uint64(b[i+8:]),
		})
	}
	return out
}

func fn(name string, value, size uint64, typ elf.SymType) elf.Symbol {
	return elf.Symbol{
		Name:    name,
		Value:   value,
		Size:    size,
		Info:    uint8(elf.ST_INFO(elf.STB_GLOBAL, typ)),
		Section: elf.SectionIndex(1),
	}
}

// The whole point of the table is that addresses are in the FUSED space: a
// library's symbols are recorded at link-time values, and the lifter reads the
// image after every object has been rebased.
func TestFuncTableRebasesAndSorts(t *testing.T) {
	objs := []*Object{
		// Deliberately out of address order: the executable is first in objs but
		// the library below it is mapped lower, so emitting in object order would
		// produce an unsorted table.
		{Base: 0x400000, symbols: []elf.Symbol{
			fn("main", 0x1000, 0x40, elf.STT_FUNC),
			fn("helper", 0x2000, 0x10, elf.STT_FUNC),
		}},
		{Base: 0x100000, symbols: []elf.Symbol{
			fn("libfn", 0x500, 0x80, elf.STT_FUNC),
		}},
	}

	got := entries(t, funcTable(objs))
	want := [][2]uint64{
		{0x100500, 0x80},
		{0x401000, 0x40},
		{0x402000, 0x10},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %#x", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = {%#x, %#x}, want {%#x, %#x}",
				i, got[i][0], got[i][1], want[i][0], want[i][1])
		}
	}
}

// An STT_GNU_IFUNC symbol's value is its RESOLVER, which is ordinary code the
// lifter has to lift. Dropping it would make the inventory an unusable keep-list
// -- the resolver would never be lifted and binding it would land on nothing.
func TestFuncTableKeepsIfuncResolversAndDropsNonCode(t *testing.T) {
	objs := []*Object{{Base: 0, symbols: []elf.Symbol{
		fn("resolver", 0x1000, 0x20, sttGNUIFunc),
		fn("plain", 0x2000, 0x20, elf.STT_FUNC),
		fn("a_data_object", 0x3000, 0x20, elf.STT_OBJECT),
		fn("", 0x4000, 0x20, elf.STT_FUNC), // unnamed
		{Name: "imported", Value: 0x5000, Size: 0x20,
			Info:    uint8(elf.ST_INFO(elf.STB_GLOBAL, elf.STT_FUNC)),
			Section: elf.SHN_UNDEF},
	}}}

	got := entries(t, funcTable(objs))
	want := []uint64{0x1000, 0x2000}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %#x", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i][0] != w {
			t.Errorf("entry %d addr = %#x, want %#x", i, got[i][0], w)
		}
	}
}

// Aliases are routine -- weak and strong names for one implementation share an
// address. Two entries at the same address would make the count wrong and, used
// as a keep-list, would ask the lifter to lift one function twice.
func TestFuncTableDedupesAliases(t *testing.T) {
	objs := []*Object{{Base: 0, symbols: []elf.Symbol{
		fn("memcpy", 0x1000, 0x40, elf.STT_FUNC),
		fn("__memcpy_generic", 0x1000, 0x40, elf.STT_FUNC),
	}}}

	if got := entries(t, funcTable(objs)); len(got) != 1 {
		t.Fatalf("aliases produced %d entries, want 1: %#x", len(got), got)
	}
}

// A size of 0 is recorded rather than dropped: the source object did not know
// the extent either, and the lifter's own fallback still applies. Dropping the
// symbol would instead hide a real function from the inventory.
func TestFuncTableKeepsSizelessFunctions(t *testing.T) {
	objs := []*Object{{Base: 0, symbols: []elf.Symbol{
		fn("nosize", 0x1000, 0, elf.STT_FUNC),
	}}}

	got := entries(t, funcTable(objs))
	if len(got) != 1 || got[0][0] != 0x1000 || got[0][1] != 0 {
		t.Fatalf("got %#x, want one entry {0x1000, 0}", got)
	}
}

// addTable skips an empty table, so an image with no functions must emit no
// section at all rather than a zero-length one.
func TestFuncTableEmptyWhenNoFunctions(t *testing.T) {
	objs := []*Object{{Base: 0, symbols: []elf.Symbol{
		fn("just_data", 0x1000, 0x20, elf.STT_OBJECT),
	}}}

	if got := funcTable(objs); len(got) != 0 {
		t.Fatalf("got %d bytes, want none", len(got))
	}
}
