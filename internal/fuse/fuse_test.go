package fuse

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestSectionSuffix(t *testing.T) {
	// The executable's sections keep plain names; each library gets .lN. This is
	// the naming the poc's fused module shows (.got.l0, .data.l0).
	for _, c := range []struct {
		idx  int
		want string
	}{{-1, ""}, {0, ".l0"}, {1, ".l1"}, {12, ".l12"}} {
		if got := (&Object{LibIndex: c.idx}).suffix(); got != c.want {
			t.Errorf("LibIndex %d: suffix = %q, want %q", c.idx, got, c.want)
		}
	}
}

func TestLayoutTLSPacksObjectsAboveTheTCB(t *testing.T) {
	objs := []*Object{
		{Name: "exe"}, // no TLS
		{Name: "libc", hasTLS: true, tlsMemsz: 100, tlsAlignment: 16},
		{Name: "ld", hasTLS: true, tlsMemsz: 8, tlsAlignment: 8},
	}
	layoutTLS(objs)

	// Variant I: the TCB sits at the thread pointer and static blocks follow.
	if objs[1].tlsOffset != tcbSize {
		t.Errorf("first TLS object at %d, want %d", objs[1].tlsOffset, tcbSize)
	}
	// 16+100 = 116, rounded up to the next multiple of 8.
	if objs[2].tlsOffset != 120 {
		t.Errorf("second TLS object at %d, want 120", objs[2].tlsOffset)
	}
	// Objects without TLS must not consume a slot.
	if objs[0].tlsOffset != 0 {
		t.Errorf("non-TLS object was assigned offset %d", objs[0].tlsOffset)
	}
}

func TestLayoutTLSRespectsAlignment(t *testing.T) {
	objs := []*Object{
		{Name: "a", hasTLS: true, tlsMemsz: 1, tlsAlignment: 8},
		{Name: "b", hasTLS: true, tlsMemsz: 1, tlsAlignment: 64},
	}
	layoutTLS(objs)
	if objs[1].tlsOffset%64 != 0 {
		t.Errorf("offset %d violates 64-byte alignment", objs[1].tlsOffset)
	}
	if objs[1].tlsOffset < objs[0].tlsOffset+objs[0].tlsMemsz {
		t.Errorf("TLS blocks overlap: %d then %d", objs[0].tlsOffset, objs[1].tlsOffset)
	}
}

func TestLayoutPlacesLibrariesAboveTheExecutable(t *testing.T) {
	// A non-PIE executable keeps its absolute addresses; libraries are placed
	// above it on libAlign boundaries, giving the poc's 0x400000 / 0x600000
	// shape.
	exe := &Object{Name: "exe", LibIndex: -1, lo: 0x400000, hi: 0x401000}
	lib := &Object{Name: "lib", LibIndex: 0, lo: 0, hi: 0x1000}
	objs := []*Object{exe, lib}
	if err := assignBases(objs, false, Options{}); err != nil {
		t.Fatal(err)
	}
	if exe.Base != 0 {
		t.Errorf("non-PIE executable rebased to %#x, want 0", exe.Base)
	}
	if lib.Base < exe.hi {
		t.Errorf("library base %#x overlaps executable ending at %#x", lib.Base, exe.hi)
	}
	if lib.Base%libAlign != 0 {
		t.Errorf("library base %#x is not %#x-aligned", lib.Base, libAlign)
	}
}

// `.ecv.tlsalign` is what a fused musl image needs to create a thread at all:
// musl's `struct tls_module` carries `align`, `libc.tls_align` is the maximum
// over the modules, and `__copy_tls` sizes a new thread's TLS block from both.
// The alignment cannot be recovered from the other four fields, so a missing or
// mis-ordered entry here is a wrong TLS block rather than a missing one.
//
// The pairing with `.ecv.tls` is the whole contract -- entry i of one describes
// the same module as entry i of the other -- so this asserts the count and the
// order together, not just the values.
func TestTLSAlignTablePairsWithTheDescriptor(t *testing.T) {
	objs := []*Object{
		{Name: "exe", LibIndex: -1, Base: 0x400000},
		{Name: "libnotls.so", LibIndex: 0, Base: 0x600000},
		{Name: "libc.so", LibIndex: 1, Base: 0x800000,
			hasTLS: true, tlsVaddr: 0x1080, tlsFilesz: 0x28, tlsMemsz: 0x90, tlsAlignment: 16},
		{Name: "libx.so", LibIndex: 2, Base: 0xa00000,
			hasTLS: true, tlsVaddr: 0x2000, tlsFilesz: 8, tlsMemsz: 8, tlsAlignment: 8},
	}
	layoutTLS(objs)

	desc := tlsDescriptor(objs)
	al := tlsAlignTable(objs)
	if len(al)*4 != len(desc) {
		t.Fatalf("%d bytes of alignment for %d bytes of descriptor: one 8-byte entry must pair with each 32-byte entry",
			len(al), len(desc))
	}
	// Order, not just multiset: entry i of each table must be the same module.
	// libc aligns to 16 and libx to 8; swapping them would keep the byte count
	// and the set of values, and give every guest thread a misaligned block.
	for i, want := range []uint64{16, 8} {
		if got := binary.LittleEndian.Uint64(al[i*8:]); got != want {
			t.Errorf("module %d alignment %d, want %d", i, got, want)
		}
		// ...and the module it pairs with is the one whose OFFSET honours it.
		off := binary.LittleEndian.Uint64(desc[i*32+24:])
		if off%want != 0 {
			t.Errorf("module %d offset %d is not %d-aligned, so the tables disagree about which module this is",
				i, off, want)
		}
	}

	// A module with no declared alignment must report 1, never 0: musl rounds
	// with `align-1` masks, and a zero would underflow into an all-ones mask.
	objs[3].tlsAlignment = 0
	al = tlsAlignTable(objs)
	if got := binary.LittleEndian.Uint64(al[8:]); got != 1 {
		t.Errorf("unaligned module reported %d, want 1", got)
	}

	// An image with no thread-locals emits nothing, exactly as `.ecv.tls` does.
	none := []*Object{{Name: "exe", LibIndex: -1, Base: 0x400000}}
	if got := tlsAlignTable(none); len(got) != 0 {
		t.Errorf("a TLS-free image produced %d bytes of alignment table", len(got))
	}
}

// The runtime initialises static TLS from the `.ecv.tls` table (see
// runtime/src/context.rs setup_tls). Without it a fused image gets no TLS
// initialisation at all -- the merged image advertises no PT_TLS, so the
// single-phdr fallback finds nothing either, and every __thread variable reads
// whatever the arena held. That is silent wrong data, which is why it survived
// unnoticed: the guest does not crash, it just believes uninitialised values.
func TestTLSDescriptorLayout(t *testing.T) {
	objs := []*Object{
		{Name: "exe", LibIndex: -1, Base: 0x400000},
		{Name: "libnotls.so", LibIndex: 0, Base: 0x600000},
		{Name: "libc.so", LibIndex: 1, Base: 0x800000,
			hasTLS: true, tlsVaddr: 0x1080, tlsFilesz: 0x28, tlsMemsz: 0x90, tlsAlignment: 16},
		{Name: "libx.so", LibIndex: 2, Base: 0xa00000,
			hasTLS: true, tlsVaddr: 0x2000, tlsFilesz: 8, tlsMemsz: 8, tlsAlignment: 8},
	}
	layoutTLS(objs)

	// Blocks start above the TCB and honour each module's alignment.
	if got := objs[2].tlsOffset; got != tcbSize {
		t.Errorf("first TLS module at offset %d, want %d (just above the TCB)", got, tcbSize)
	}
	if objs[3].tlsOffset < objs[2].tlsOffset+objs[2].tlsMemsz {
		t.Errorf("TLS modules overlap: %d then %d", objs[2].tlsOffset, objs[3].tlsOffset)
	}
	if objs[3].tlsOffset%objs[3].tlsAlignment != 0 {
		t.Errorf("TLS module at %d violates alignment %d", objs[3].tlsOffset, objs[3].tlsAlignment)
	}

	desc := tlsDescriptor(objs)
	if len(desc) != 2*32 {
		t.Fatalf("descriptor is %d bytes, want %d (one 32-byte entry per TLS module)", len(desc), 2*32)
	}
	// Entry 0 must describe libc's template at its FUSED address, not its
	// link-time one -- the runtime copies straight out of the arena.
	get := func(entry, field int) uint64 {
		return binary.LittleEndian.Uint64(desc[entry*32+field*8:])
	}
	for _, c := range []struct {
		entry, field int
		want         uint64
		what         string
	}{
		{0, 0, 0x800000 + 0x1080, "template VMA (fused)"},
		{0, 1, 0x28, "filesz"},
		{0, 2, 0x90, "memsz"},
		{0, 3, objs[2].tlsOffset, "tp offset"},
		{1, 0, 0xa00000 + 0x2000, "second module template VMA"},
		{1, 3, objs[3].tlsOffset, "second module tp offset"},
	} {
		if got := get(c.entry, c.field); got != c.want {
			t.Errorf("entry %d %s = %#x, want %#x", c.entry, c.what, got, c.want)
		}
	}
}

func TestTLSDescriptorEmptyWithoutTLS(t *testing.T) {
	objs := []*Object{{Name: "exe", LibIndex: -1}, {Name: "lib", LibIndex: 0}}
	layoutTLS(objs)
	if d := tlsDescriptor(objs); len(d) != 0 {
		t.Errorf("descriptor should be empty when nothing uses TLS, got %d bytes", len(d))
	}
}

// findLib must treat a name containing a separator as a PATH, because that is
// how Options.Extra names a dlopen'd plugin: those live outside the library
// search path (postgres keeps them in its own $libdir), so searching the dirs
// for them could only ever fail.
func TestFindLibTakesAPathAsAPath(t *testing.T) {
	dir := t.TempDir()
	plugin := filepath.Join(dir, "dict_snowball.so")
	if err := os.WriteFile(plugin, []byte("\x7fELF"), 0o644); err != nil {
		t.Fatal(err)
	}

	// An absolute path resolves to itself, with no search directories at all --
	// the case that matters, since a plugin's directory is not on the path.
	got, err := findLib(plugin, nil)
	if err != nil {
		t.Fatalf("findLib(%q, nil) failed: %v", plugin, err)
	}
	if got != plugin {
		t.Errorf("findLib returned %q, want %q", got, plugin)
	}

	// A missing path is an error rather than a silent fallthrough to the search
	// dirs, which would otherwise turn a typo into "cannot find <soname> in
	// [...]" and point at the wrong thing.
	if _, err := findLib(filepath.Join(dir, "absent.so"), []string{dir}); err == nil {
		t.Error("a missing path resolved successfully")
	}

	// A bare soname still searches the directories.
	got, err = findLib("dict_snowball.so", []string{dir})
	if err != nil {
		t.Fatalf("findLib by soname failed: %v", err)
	}
	if got != plugin {
		t.Errorf("findLib by soname returned %q, want %q", got, plugin)
	}
}

// A dlopen'd plugin that cannot be satisfied must leave NO trace. The rollback
// is the whole mechanism: `walk` mutates the dedup maps and appends objects as
// it goes, so a failure partway through a plugin's subtree would otherwise leave
// the plugin itself fused with its dependency missing -- and a half-present
// module is worse than an absent one, because `dlopen` is intercepted and
// answers with a sentinel handle, so it loads "successfully" and then has every
// symbol resolve to NULL.
//
// python:3-slim is the case: `_tkinter` DT_NEEDEDs `libtk8.6.so`, which the
// image does not ship.
func TestAFailedPluginLeavesNoTrace(t *testing.T) {
	dir := t.TempDir()
	// A file that exists and is NOT a loadable ELF, so `walk` gets past the
	// path lookup and fails inside `open` -- i.e. after it has already mutated
	// the state. A merely-absent path would fail before touching anything and
	// would not exercise the rollback at all.
	plugin := filepath.Join(dir, "plugin.so")
	if err := os.WriteFile(plugin, []byte("\x7fELF not really an object"), 0o644); err != nil {
		t.Fatal(err)
	}

	exe := &Object{Name: "exe", LibIndex: -1}
	st := &loadState{
		objs:        []*Object{exe},
		seen:        map[string]bool{},
		seenPath:    map[string]bool{},
		seenSoname:  map[string]bool{},
		libraryPath: []string{dir},
	}
	before := st.snapshot()

	if err := st.walk([]string{plugin}); err == nil {
		t.Fatal("a non-ELF plugin walked successfully; this test proves nothing")
	}
	// Guard against a vacuous pass: the walk must actually have got far enough
	// to dirty the state, or the restore below is not being tested.
	if !st.seen[plugin] {
		t.Fatal("the walk failed before touching any state, so the rollback is untested")
	}

	*st = before
	if len(st.objs) != 1 || st.objs[0] != exe {
		t.Errorf("objs is %d entries after rollback, want just the executable", len(st.objs))
	}
	if len(st.seen) != 0 || len(st.seenPath) != 0 || len(st.seenSoname) != 0 {
		t.Errorf("dedup state survived the rollback: seen=%v path=%v soname=%v",
			st.seen, st.seenPath, st.seenSoname)
	}
}

// The snapshot must COPY the maps, not alias them. Aliasing would make the
// rollback a no-op in exactly the case it exists for, and every assertion above
// would still pass if `walk` happened to fail before its first map write.
func TestSnapshotDoesNotAliasTheDedupMaps(t *testing.T) {
	st := &loadState{
		objs:       []*Object{{Name: "exe", LibIndex: -1}},
		seen:       map[string]bool{"libc.so.6": true},
		seenPath:   map[string]bool{},
		seenSoname: map[string]bool{},
	}
	before := st.snapshot()
	st.seen["plugin.so"] = true
	st.seenPath["/x/plugin.so"] = true
	st.seenSoname["plugin.so"] = true
	st.objs = append(st.objs, &Object{Name: "plugin", LibIndex: 0})

	if before.seen["plugin.so"] || before.seenPath["/x/plugin.so"] || before.seenSoname["plugin.so"] {
		t.Error("the snapshot aliases the live maps, so a rollback restores nothing")
	}
	// NOTE: this checks the snapshot's LENGTH, not that the backing array was
	// copied, and it cannot distinguish the two -- aliasing the array is
	// harmless here precisely because the only mutation `walk` performs is an
	// append, which cannot be seen through a shorter header. Verified by
	// neutralization: replacing the copy with a plain assignment does NOT fail
	// this test, and does not break the rollback either. The defensive copy
	// stays because a future non-append mutation would make it matter.
	if len(before.objs) != 1 {
		t.Errorf("the snapshot did not preserve the object count: %d entries", len(before.objs))
	}
	if !before.seen["libc.so.6"] {
		t.Error("the snapshot dropped state that was there before it was taken")
	}
}
