package fuse

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// lib builds a library object with a known span. Bases are what the planner
// assigns, so the input must not carry one.
func lib(path string, lo, size uint64) *Object {
	return &Object{Path: path, Name: path, LibIndex: 0, lo: lo, hi: lo + size}
}

func exe(path string, hi uint64) *Object {
	return &Object{Path: path, Name: path, LibIndex: -1, lo: 0, hi: hi}
}

// THE property this file exists for. Two programs with DIFFERENT object sets --
// different executables, a different number of libraries, in a different order
// -- must place their shared libraries at identical addresses. Dense per-image
// packing cannot do this, because a base depends on everything placed before it.
func TestSharedLibrariesGetTheSameBaseInEveryProgram(t *testing.T) {
	libc := func() *Object { return lib("/lib/libc.so.6", 0, 2<<20) }
	libz := func() *Object { return lib("/lib/libz.so.1", 0, 1<<20) }
	libssl := func() *Object { return lib("/lib/libssl.so.3", 0, 3<<20) }

	progA := Program{Objs: []*Object{exe("/bin/a", 0x30000), libc(), libz()}, ExeIsPIE: true}
	progB := Program{Objs: []*Object{exe("/bin/b", 0x900000), libssl(), libc()}, ExeIsPIE: true}

	l, err := PlanLayout([]Program{progA, progB}, Options{})
	if err != nil {
		t.Fatal(err)
	}

	// Apply the plan the way assignBases does, per program, and compare the
	// address libc actually lands at.
	at := func(p Program) map[string]uint64 {
		if err := assignBases(p.Objs, p.ExeIsPIE, Options{Layout: l}); err != nil {
			t.Fatal(err)
		}
		got := map[string]uint64{}
		for _, o := range p.Objs[1:] {
			got[o.Path] = o.Base + o.lo // the address its first byte occupies
		}
		return got
	}
	a, b := at(progA), at(progB)

	if a["/lib/libc.so.6"] != b["/lib/libc.so.6"] {
		t.Errorf("libc at %#x in program A but %#x in program B; the whole point is that these match",
			a["/lib/libc.so.6"], b["/lib/libc.so.6"])
	}
	if a["/lib/libc.so.6"] == 0 {
		t.Error("libc was placed at 0, so the comparison above is vacuous")
	}
}

// Libraries must sit above EVERY program's executable, not just the one being
// fused. A library overlapping another program's exe would be silently corrupt.
func TestLibrariesClearTheLargestExecutable(t *testing.T) {
	small := Program{Objs: []*Object{exe("/bin/small", 0x10000), lib("/lib/libc.so.6", 0, 1<<20)}, ExeIsPIE: true}
	huge := Program{Objs: []*Object{exe("/bin/huge", 0x4000000), lib("/lib/libc.so.6", 0, 1<<20)}, ExeIsPIE: true}

	l, err := PlanLayout([]Program{small, huge}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	base, ok := l.baseFor(lib("/lib/libc.so.6", 0, 1<<20))
	if !ok {
		t.Fatal("libc not planned")
	}
	hugeTop := uint64(defaultExeBase) + 0x4000000
	if base < hugeTop {
		t.Errorf("libc based at %#x, below the largest exe top %#x -- it would overlap /bin/huge",
			base, hugeTop)
	}
}

// Overflow must be reported so the caller can fall back to per-image packing.
// Emitting an image that runs past BRK_START_VMA would corrupt the guest heap.
func TestOverflowIsAnErrorNotATruncatedLayout(t *testing.T) {
	var objs []*Object
	objs = append(objs, exe("/bin/big", 0x10000))
	for i := 0; i < 40; i++ {
		objs = append(objs, lib("/lib/lib"+strconv.Itoa(i)+".so", 0, 8<<20)) // 320 MB total
	}
	_, err := PlanLayout([]Program{{Objs: objs, ExeIsPIE: true}}, Options{})
	if err == nil {
		t.Fatal("a 320 MB closure planned into a 156 MB region without error")
	}
	if !strings.Contains(err.Error(), "fall back") {
		t.Errorf("overflow error should tell the caller what to do, got: %v", err)
	}
}

// The path-as-identity assumption is verified, not trusted.
func TestDisagreeingContentAtOnePathIsRejected(t *testing.T) {
	a := Program{Objs: []*Object{exe("/bin/a", 0x1000), lib("/lib/libc.so.6", 0, 1<<20)}, ExeIsPIE: true}
	b := Program{Objs: []*Object{exe("/bin/b", 0x1000), lib("/lib/libc.so.6", 0, 2<<20)}, ExeIsPIE: true}
	if _, err := PlanLayout([]Program{a, b}, Options{}); err == nil {
		t.Fatal("two different libcs at one path were accepted; the layout would be wrong for one of them")
	}
}

// A nil Layout must leave the existing behaviour exactly as it was: this change
// is opt-in, and every current caller passes no layout.
func TestNilLayoutKeepsDensePacking(t *testing.T) {
	objs := []*Object{exe("/bin/a", 0x1000), lib("/lib/one.so", 0, 1<<20), lib("/lib/two.so", 0, 1<<20)}
	if err := assignBases(objs, true, Options{}); err != nil {
		t.Fatal(err)
	}
	// Dense packing puts the first library immediately above the exe, aligned.
	want := (uint64(defaultExeBase) + 0x1000 + libAlign - 1) &^ (libAlign - 1)
	if got := objs[1].Base + objs[1].lo; got != want {
		t.Errorf("first library at %#x, want %#x -- dense packing changed", got, want)
	}
}

// brkStartVMA is copied from Rust and the two must not drift. Reading the source
// is the only check available from Go, and a silent drift here would place
// libraries inside the guest heap.
func TestBrkStartMatchesTheRuntime(t *testing.T) {
	b, err := os.ReadFile("../../runtime/src/arena.rs")
	if err != nil {
		t.Skipf("runtime source not readable: %v", err)
	}
	m := regexp.MustCompile(`(?m)^pub const BRK_START_VMA: u64 = (0x[0-9A-Fa-f_]+);`).FindSubmatch(b)
	if m == nil {
		t.Fatal("BRK_START_VMA not found in runtime/src/arena.rs; the constant moved or was renamed")
	}
	got, err := strconv.ParseUint(strings.ReplaceAll(strings.TrimPrefix(string(m[1]), "0x"), "_", ""), 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	if got != brkStartVMA {
		t.Errorf("runtime BRK_START_VMA is %#x but fuse uses %#x", got, brkStartVMA)
	}
}

// Ranges must cover exactly what the plan assigned, because the partitioner uses
// them as the authority on where one library ends. A range that is too short
// leaves the library's tail unattributed and those functions fall back to the
// name-hashed common bucket; too long, and a partition can span two libraries and
// stop being reusable. So this checks the boundary against the base assignment
// itself rather than against a hand-written number.
func TestRangesMatchTheAssignedBases(t *testing.T) {
	objs := []*Object{exe("/bin/a", 0x1000), lib("/lib/one.so", 0x2000, 1<<20), lib("/lib/two.so", 0, 3<<20)}
	l, err := PlanLayout([]Program{{Objs: objs, ExeIsPIE: true}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rs := l.Ranges()
	if len(rs) != 2 {
		t.Fatalf("planned %d ranges for 2 libraries", len(rs))
	}
	for _, r := range rs {
		o := objs[1]
		if objs[2].Path == r.Path {
			o = objs[2]
		}
		base, ok := l.baseFor(o)
		if !ok {
			t.Fatalf("%s has a range but no base", r.Path)
		}
		// The library occupies [base+lo, base+hi): the same arithmetic emit uses.
		if want := base + o.lo; r.Start != want {
			t.Errorf("%s starts at %#x, want %#x", r.Path, r.Start, want)
		}
		if want := base + o.hi; r.End != want {
			t.Errorf("%s ends at %#x, want %#x", r.Path, r.End, want)
		}
	}
	if rs[0].Start >= rs[1].Start {
		t.Errorf("ranges are not sorted by address: %#x then %#x", rs[0].Start, rs[1].Start)
	}
	// The first range must begin exactly where sharing begins, or the two
	// boundaries disagree and a library looks program-specific.
	if rs[0].Start != l.SharedMin() {
		t.Errorf("first range starts at %#x but SharedMin is %#x", rs[0].Start, l.SharedMin())
	}
}

// The format is a wire protocol read by builder/ecv-split.cpp, so it is pinned
// here rather than left to whatever fmt produces.
func TestFormatLibRangesIsThePinnedWireFormat(t *testing.T) {
	got := FormatLibRanges([]LibRange{
		{Path: "/lib/one.so", Start: 0x600000, End: 0x700000},
		{Path: "/lib/two.so", Start: 0x800000, End: 0x8a0000},
	})
	const want = "0x600000:0x700000,0x800000:0x8a0000"
	if got != want {
		t.Errorf("FormatLibRanges = %q, want %q", got, want)
	}
	if FormatLibRanges(nil) != "" {
		t.Errorf("no ranges must format as the empty string, so the env var is simply unset")
	}
}

// plug builds an Options.Extra object: a dlopen'd plugin, with its own PT_LOAD
// alignment rather than libAlign.
//
// 64 KiB is not an arbitrary stand-in. Sampled over 2,114 real aarch64 shared
// objects on a Debian host, and over all 79 of postgres:17's extensions, EVERY
// one reports a maximum PT_LOAD p_align of exactly 0x10000.
func plug(path string, lo, size uint64) *Object {
	o := lib(path, lo, size)
	o.isPlugin = true
	o.maxAlign = 0x10000
	return o
}

// THE property the plugin band exists for, and the reason it is a separate band
// rather than a smaller libAlign: adding plugins must not move any LIBRARY.
//
// Lifted code embeds absolute guest addresses, so moving a library re-lifts it
// and invalidates every cached object and partition for it. Packing plugins is
// not worth that, so the band goes ABOVE the libraries and leaves them alone.
func TestPluginsDoNotMoveAnyLibrary(t *testing.T) {
	libs := func() []*Object {
		return []*Object{lib("/lib/libc.so.6", 0, 2<<20), lib("/lib/libz.so.1", 0, 1<<20)}
	}

	without := Program{Objs: append([]*Object{exe("/bin/pg", 0x30000)}, libs()...), ExeIsPIE: true}
	withObjs := append([]*Object{exe("/bin/pg", 0x30000)}, libs()...)
	for i := 0; i < 40; i++ {
		withObjs = append(withObjs, plug("/aaa/ext"+strconv.Itoa(i)+".so", 0, 66<<10))
	}
	with := Program{Objs: withObjs, ExeIsPIE: true}

	a, err := PlanLayout([]Program{without}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := PlanLayout([]Program{with}, Options{})
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"/lib/libc.so.6", "/lib/libz.so.1"} {
		x, okx := a.base[p]
		y, oky := b.base[p]
		if !okx || !oky {
			t.Fatalf("%s missing from a plan (%v/%v)", p, okx, oky)
		}
		if x != y {
			t.Errorf("%s moved from %#x to %#x when 40 plugins were added; "+
				"every cached object for it would miss", p, x, y)
		}
	}
	// Without this the comparison above passes when NOTHING was planned.
	if len(b.base) != len(a.base)+40 {
		t.Fatalf("expected 40 more planned objects, got %d vs %d", len(b.base), len(a.base))
	}
}

// Plugins pack at their OWN alignment, not at libAlign.
//
// The number is the point. postgres:17 ships 79 extensions with a median size of
// 66 KiB and 7.8 MiB of content between them. At libAlign (2 MiB) they need
// 158 MiB, which alone exceeds the 156 MiB fused region -- measured end to end,
// the closure asked for 0x1b820010 against a 0xa000000 ceiling and silently fell
// back to per-image packing.
func TestPluginBandPacksTighterThanLibAlign(t *testing.T) {
	const n = 79
	objs := []*Object{exe("/bin/pg", 0x30000)}
	for i := 0; i < n; i++ {
		objs = append(objs, plug("/aaa/ext"+strconv.Itoa(i)+".so", 0, 66<<10))
	}
	l, err := PlanLayout([]Program{{Objs: objs, ExeIsPIE: true}}, Options{})
	if err != nil {
		t.Fatalf("79 plugins should fit; they did not: %v", err)
	}

	band := l.top - l.pluginMin
	if ceiling := uint64(n) * libAlign; band >= ceiling {
		t.Errorf("plugin band is %d bytes, no better than %d at libAlign; "+
			"the band is not using the objects' own alignment", band, ceiling)
	}
	// Each plugin is 66 KiB and aligns to 64 KiB, so it occupies two 64 KiB
	// slots: 128 KiB apiece. Asserting an upper bound rather than equality --
	// but a bound tight enough that libAlign packing (2 MiB apiece) blows it.
	if want := uint64(n) * 2 * 0x10000; band > want {
		t.Errorf("plugin band %d bytes exceeds the %d a 64 KiB packing needs", band, want)
	}
	if band == 0 {
		t.Fatal("plugin band is empty, so the bounds above are vacuous")
	}

	// Every plugin must actually be IN the band, not left below it among the
	// libraries.
	for _, s := range l.Ranges() {
		if s.Start < l.pluginMin {
			t.Errorf("%s at %#x is below the plugin band floor %#x", s.Path, s.Start, l.pluginMin)
		}
	}
}
