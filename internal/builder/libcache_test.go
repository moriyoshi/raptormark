package builder

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestParseLibRanges(t *testing.T) {
	got := parseLibRanges("0x60e0d0:0x61d140,0x827200:0x93bec4")
	want := []libRange{{0x60e0d0, 0x61d140}, {0x827200, 0x93bec4}}
	if len(got) != len(want) {
		t.Fatalf("got %d ranges, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("range %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	// Sorted, because exeRangeSpec takes the first range's start as the boundary
	// and libKey hashes from it.
	if out := parseLibRanges("0x900:0xa00,0x100:0x200"); out[0].start != 0x100 {
		t.Errorf("ranges must come back sorted, got %+v", out)
	}
	// A malformed spec disables the cache rather than half-configuring it.
	for _, bad := range []string{"", "0x100", "0x100:", "zz:0x200", "0x200:0x100", "0x100:0x200,oops"} {
		if out := parseLibRanges(bad); out != nil {
			t.Errorf("parseLibRanges(%q) = %+v, want nil", bad, out)
		}
	}
}

func TestExeRangeSpecIsEverythingBelowTheFirstLibrary(t *testing.T) {
	// PlanLayout places every library above the highest executable top, so one
	// span from 0 covers the executable and anything synthesised alongside it.
	// Starting at the executable's own base instead would drop such a function
	// out of BOTH halves.
	if got, want := exeRangeSpec([]libRange{{0x60e0d0, 0x61d140}}), "0x0:0x60e0d0"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// writeTestELF builds a minimal ELF64 with the given (name, addr, bytes)
// allocated sections, which is enough for libKey: it walks Sections and reads
// SHF_ALLOC ones.
func writeTestELF(t *testing.T, path string, secs []testSec) {
	t.Helper()
	const ehsize = 64
	shentsize := 64
	// Section 0 is the null entry; then one .shstrtab; then the payload sections.
	names := []byte{0}
	off := map[string]uint32{}
	for _, s := range secs {
		off[s.name] = uint32(len(names))
		names = append(names, []byte(s.name)...)
		names = append(names, 0)
	}
	shstrOff := uint32(len(names))
	names = append(names, []byte(".shstrtab")...)
	names = append(names, 0)

	var body bytes.Buffer
	dataOff := map[string]uint64{}
	body.Write(make([]byte, ehsize)) // header written last
	for _, s := range secs {
		dataOff[s.name] = uint64(body.Len())
		body.Write(s.data)
	}
	strOff := uint64(body.Len())
	body.Write(names)
	shoff := uint64(body.Len())

	sh := func(name uint32, typ elf.SectionType, flags elf.SectionFlag, addr, offset, size uint64) []byte {
		b := make([]byte, shentsize)
		binary.LittleEndian.PutUint32(b[0:], name)
		binary.LittleEndian.PutUint32(b[4:], uint32(typ))
		binary.LittleEndian.PutUint64(b[8:], uint64(flags))
		binary.LittleEndian.PutUint64(b[16:], addr)
		binary.LittleEndian.PutUint64(b[24:], offset)
		binary.LittleEndian.PutUint64(b[32:], size)
		binary.LittleEndian.PutUint64(b[56:], 1) // addralign
		return b
	}
	var shdrs bytes.Buffer
	shdrs.Write(make([]byte, shentsize)) // SHT_NULL
	for _, s := range secs {
		flags := elf.SHF_ALLOC | elf.SHF_EXECINSTR
		if s.data1 {
			// Allocated but NOT executable: per-program data, which must not
			// reach the key even when it sits above the library base.
			flags = elf.SHF_ALLOC
		}
		shdrs.Write(sh(off[s.name], elf.SHT_PROGBITS, flags,
			s.addr, dataOff[s.name], uint64(len(s.data))))
	}
	shdrs.Write(sh(shstrOff, elf.SHT_STRTAB, 0, 0, strOff, uint64(len(names))))
	body.Write(shdrs.Bytes())

	raw := body.Bytes()
	copy(raw[0:], []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0})
	binary.LittleEndian.PutUint16(raw[16:], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(raw[18:], uint16(elf.EM_AARCH64))
	binary.LittleEndian.PutUint32(raw[20:], 1)
	binary.LittleEndian.PutUint64(raw[40:], shoff)
	binary.LittleEndian.PutUint16(raw[58:], uint16(shentsize))
	binary.LittleEndian.PutUint16(raw[60:], uint16(len(secs)+2))
	binary.LittleEndian.PutUint16(raw[62:], uint16(len(secs)+1))
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := elf.Open(path); err != nil {
		t.Fatalf("the fixture is not a readable ELF: %v", err)
	}
}

type testSec = struct {
	name  string
	addr  uint64
	data  []byte
	data1 bool // allocated but not executable
}

// TestLibKeyDependsOnTheLibrariesAndNotTheProgram is the property the whole
// cache rests on: two programs of one closure share library code at identical
// addresses while their executables differ, so their keys must MATCH — and any
// change to the libraries, their placement, or the lifter must not.
func TestLibKeyDependsOnTheLibrariesAndNotTheProgram(t *testing.T) {
	dir := t.TempDir()
	ranges := []libRange{{0x600000, 0x700000}}
	const spec = "0x600000:0x700000"

	libs := []testSec{{".text.l0", 0x600000, []byte("library code here"), false}}
	progA := append([]testSec{{".text", 0x400000, []byte("program A"), false}}, libs...)
	progB := append([]testSec{{".text", 0x400000, []byte("program B, quite different"), false}}, libs...)

	key := func(name string, secs []testSec, lifter, rspec string) string {
		p := filepath.Join(dir, name)
		writeTestELF(t, p, secs)
		k, err := libKey(p, lifter, rspec, ranges)
		if err != nil {
			t.Fatalf("libKey(%s): %v", name, err)
		}
		return k
	}

	a := key("a", progA, "lifter-1", spec)
	b := key("b", progB, "lifter-1", spec)
	if a != b {
		t.Errorf("two programs sharing libraries must share a key:\n  %s\n  %s", a, b)
	}

	// Each of these MUST move the key. A cached half from any of them would be
	// silently wrong rather than merely stale.
	changedBytes := key("c", []testSec{
		{".text", 0x400000, []byte("program A"), false},
		{".text.l0", 0x600000, []byte("library code CHANGED"), false},
	}, "lifter-1", spec)
	if changedBytes == a {
		t.Error("changing library BYTES must change the key")
	}

	movedLib := key("d", []testSec{
		{".text", 0x400000, []byte("program A"), false},
		{".text.l0", 0x680000, []byte("library code here"), false},
	}, "lifter-1", spec)
	if movedLib == a {
		t.Error("moving a library to a different ADDRESS must change the key; " +
			"the lifted IR embeds guest addresses in names and bodies")
	}

	if newLifter := key("e", progA, "lifter-2", spec); newLifter == a {
		t.Error("a different LIFTER must change the key")
	}
	if newSpec := key("f", progA, "lifter-1", "0x600000:0x700001"); newSpec == a {
		t.Error("a different RANGE SPEC must change the key")
	}
}

// The executable half must not reach the key at all — that is what lets two
// different programs hit. Guarded separately from the equality above so a
// regression says which property broke.
func TestLibKeyIgnoresSectionsBelowTheFirstLibrary(t *testing.T) {
	dir := t.TempDir()
	ranges := []libRange{{0x600000, 0x700000}}
	mk := func(name string, exe []byte) string {
		p := filepath.Join(dir, name)
		writeTestELF(t, p, []testSec{
			{".text", 0x400000, exe, false},
			{".text.l0", 0x600000, []byte("shared library code"), false},
		})
		k, err := libKey(p, "lifter", "0x600000:0x700000", ranges)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	if mk("x", []byte("tiny")) != mk("y", bytes.Repeat([]byte("much larger program"), 64)) {
		t.Error("executable content must not reach the library key")
	}
}

// TestLibKeyIgnoresPerProgramDataAboveTheLibraryBase is the regression for the
// bug that made the first real closure run miss on both programs.
//
// The fused layout puts per-program material ABOVE the first library base --
// `.got.l0`, and the `.ecv.*` metadata sections, which differ per program by
// construction (measured on `.ecv.funcs`, since removed: 62,240 bytes for echo
// against 62,400 for cat -- the property is the per-program difference, which
// `.ecv.dlsyms` still exhibits). A key over every ALLOCATED section from that base swept all of it in, so
// two programs of one closure keyed differently and both re-lifted their
// libraries. None of it reaches the merged module: it is data, and the library
// half's copy is dropped as a duplicate at merge time.
func TestLibKeyIgnoresPerProgramDataAboveTheLibraryBase(t *testing.T) {
	dir := t.TempDir()
	ranges := []libRange{{0x600000, 0x700000}}
	const spec = "0x600000:0x700000"

	mk := func(name string, meta []byte) string {
		p := filepath.Join(dir, name)
		writeTestELF(t, p, []testSec{
			{".text", 0x400000, []byte("program"), false},
			{".text.l0", 0x600000, []byte("shared library code"), false},
			// Above the library base, allocated, NOT executable, per-program.
			{".ecv.dlsyms", 0x780000, meta, true},
		})
		k, err := libKey(p, "lifter", spec, ranges)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	if mk("m1", []byte("echo inventory")) != mk("m2", []byte("cat inventory, a different length")) {
		t.Error("per-program data above the library base must not reach the key; " +
			"it is dropped as a duplicate at merge time and never reaches the module")
	}
}

// TestLibKeyIsPerLibraryNotPerSet covers libKey's per-library capability, which
// is implemented and correct but NOT currently wired: liftSplit keys the whole
// library set, because per-library merging fails its closure gate (-5 of 121
// against 37). Kept because the key is the part that was never in doubt, and the
// next attempt should not have to rebuild it.
//
// Two programs of a closure rarely link the same library SET. Keyed on the set,
// they share nothing: /bin/echo takes libc while /bin/bash takes libc and
// libtinfo, so echo+bash cached two halves, hit never, and paid the split lift
// for no return. Keyed per library, the library they DO share must key alike
// regardless of what else either one links.
func TestLibKeyIsPerLibraryNotPerSet(t *testing.T) {
	dir := t.TempDir()
	libc := testSec{".text.l0", 0x600000, []byte("libc code, shared by both"), false}
	tinfo := testSec{".text.l1", 0x700000, []byte("libtinfo, only one links it"), false}

	keyOf := func(name string, secs []testSec, r libRange) string {
		p := filepath.Join(dir, name)
		writeTestELF(t, p, secs)
		spec := "0x600000:0x700000"
		if r.start != 0x600000 {
			spec = "0x700000:0x800000"
		}
		k, err := libKey(p, "lifter", spec, []libRange{r})
		if err != nil {
			t.Fatalf("libKey(%s): %v", name, err)
		}
		return k
	}

	// One program links libc only; the other links libc AND libtinfo.
	small := []testSec{{".text", 0x400000, []byte("echo"), false}, libc}
	big := []testSec{{".text", 0x400000, []byte("bash, larger"), false}, libc, tinfo}

	libcRange := libRange{0x600000, 0x700000}
	if a, b := keyOf("small", small, libcRange), keyOf("big", big, libcRange); a != b {
		t.Errorf("the SHARED library must key alike whatever else a program links:\n  %s\n  %s", a, b)
	}

	// And the library only one of them links must key differently from libc,
	// or the two would collide in the cache and serve each other's code.
	if libcKey, tinfoKey := keyOf("big2", big, libcRange), keyOf("big3", big, libRange{0x700000, 0x800000}); libcKey == tinfoKey {
		t.Error("two different libraries must not share a cache key")
	}
}

// TestLibKeyReportsUnlinkedRangesDistinctly guards the asymmetry that cost a
// closure run.
//
// The range list is CLOSURE-wide, so it names every library any program in the
// closure links and each program links a subset. A range the program has no code
// in must be distinguishable from a real failure: treated as an error it made
// /bin/echo abandon the split lift entirely while /bin/bash kept it, and two
// programs built by different pipelines produce different partitions — reuse went
// to -5 of 121 with nothing wrong in either module.
func TestLibKeyReportsUnlinkedRangesDistinctly(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "prog")
	writeTestELF(t, p, []testSec{
		{".text", 0x400000, []byte("program"), false},
		{".text.l0", 0x600000, []byte("the one library it links"), false},
	})

	// A library it does link: a real key.
	if _, err := libKey(p, "lifter", "0x600000:0x700000", []libRange{{0x600000, 0x700000}}); err != nil {
		t.Fatalf("a linked library must key: %v", err)
	}

	// A library it does not link: the sentinel, so the caller can skip rather
	// than fall back to a whole-image lift.
	_, err := libKey(p, "lifter", "0x900000:0xa00000", []libRange{{0x900000, 0xa00000}})
	if !errors.Is(err, errNoCodeInRange) {
		t.Errorf("an unlinked library range must report errNoCodeInRange, got %v", err)
	}
}

// TestLibKeyIgnoresThePerProgramSlotIndex guards the trap that made per-library
// caching share nothing between /bin/echo and /bin/bash.
//
// The fuser names a library's sections `.text.l<N>`, where N is that PROGRAM's
// own library slot — so one library is `.text.l0` in echo and `.text.l1` in
// bash, purely because bash links an extra library that takes slot 0. Identical
// code, identical address, different name. Hashing the name split the key and
// the cache held 5 modules for 2+3 libraries, i.e. no sharing at all.
func TestLibKeyIgnoresThePerProgramSlotIndex(t *testing.T) {
	dir := t.TempDir()
	code := []byte("identical library code at an identical address")
	r := libRange{0x600000, 0x700000}
	const spec = "0x600000:0x700000"

	mk := func(file, secName string, extra []testSec) string {
		p := filepath.Join(dir, file)
		writeTestELF(t, p, append([]testSec{
			{".text", 0x400000, []byte("program"), false},
			{secName, 0x600000, code, false},
		}, extra...))
		k, err := libKey(p, "lifter", spec, []libRange{r})
		if err != nil {
			t.Fatal(err)
		}
		return k
	}

	// The same library, occupying a different slot because the second program
	// links one more library below it.
	slot0 := mk("prog_a", ".text.l0", nil)
	slot1 := mk("prog_b", ".text.l1", []testSec{{".text.l0", 0x800000, []byte("the extra library"), false}})
	if slot0 != slot1 {
		t.Errorf("the per-program slot index must not reach the key:\n  %s\n  %s", slot0, slot1)
	}
}
