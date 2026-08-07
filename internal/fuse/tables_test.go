package fuse

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"testing"
)

// buildThreadDB lays out a fake libc image holding glibc's `_thread_db_*`
// descriptors at known addresses, and returns the object plus a symbol table
// naming them. Field descriptors are three uint32s -- width in BITS, element
// count, byte OFFSET -- and reading the first word instead of the third is the
// obvious mistake, so the fixture makes the two differ.
func buildThreadDB(t *testing.T) (*Object, map[string]uint64) {
	t.Helper()
	const base = 0x1000000
	img := make([]byte, 0x4000)
	syms := map[string]uint64{"_rtld_global": base + 0x3000}

	field := func(name string, at uint64, bits, offset uint32) {
		binary.LittleEndian.PutUint32(img[at:], bits)
		binary.LittleEndian.PutUint32(img[at+4:], 1)
		binary.LittleEndian.PutUint32(img[at+8:], offset)
		syms[name] = base + at
	}
	field("_thread_db_rtld_global__dl_stack_used", 0x100, 128, 2968)
	field("_thread_db_rtld_global__dl_stack_user", 0x110, 128, 2984)
	field("_thread_db_pthread_list", 0x120, 128, 192)
	field("_thread_db_pthread_tid", 0x130, 32, 208)
	// A scalar sizeof descriptor is a bare uint32, not a triple.
	binary.LittleEndian.PutUint32(img[0x140:], 1824)
	syms["_thread_db_sizeof_pthread"] = base + 0x140

	return buildObj(base, img), syms
}

func TestStacklistsDescriptorReadsThreadDB(t *testing.T) {
	obj, syms := buildThreadDB(t)
	b := stacklistsDescriptor([]*Object{obj}, syms)
	if len(b) != 128 {
		t.Fatalf("got %d bytes, want 128 (sixteen words; the runtime needs >= 72 for the tid "+
			"seed, >= 80 for _dl_minsigstacksize, >= 96 for the _dl_tls_static pair and "+
			"words 12..15 for the ld.so hook installers)", len(b))
	}
	word := func(i int) uint64 { return binary.LittleEndian.Uint64(b[i*8:]) }
	for _, tc := range []struct {
		i    int
		want uint64
		name string
	}{
		{0, 0x1003000, "_rtld_global"},
		{1, 2968, "_dl_stack_used offset"},
		{2, 2984, "_dl_stack_user offset"},
		{3, 3000, "_dl_stack_cache offset, derived from the stride"},
		{4, 1824, "sizeof(struct pthread)"},
		{5, 192, "offsetof(pthread, list)"},
		{6, 0, "rtld lock slot, not supplied"},
		{7, 0, "noop lock, not supplied"},
		{8, 208, "offsetof(pthread, tid)"},
		// Words 10 and 11 need `_rtld_global_ro` and `__pthread_get_minstack`,
		// neither of which this fixture has, so they must come back ZERO --
		// which is what the runtime reads as "leave those fields alone". A
		// non-zero here would mean the decoder invented an address.
		{10, 0, "_dl_tls_static_size, not decodable in this fixture"},
		{11, 0, "_dl_tls_static_align, not decodable in this fixture"},
		// Word 12 needs ld.so's own text to scan; this fixture has none, so it
		// must be ZERO. A non-zero value would mean the installer search
		// matched something in a synthetic object, which is the shape of a
		// false positive that would make the runtime CALL an arbitrary address.
		{12, 0, "ld.so hook installer, not identifiable in this fixture"},
		{13, 0, "second installer slot, empty"},
		{14, 0, "third installer slot, empty"},
		{15, 0, "fourth installer slot, empty"},
		// This fixture has no _rtld_global_ro and no accessors to decode, so the
		// tenth word must be an explicit zero -- which the runtime reads as
		// "leave the field alone" -- rather than a plausible-looking address.
		{9, 0, "_dl_minsigstacksize, not derivable here"},
	} {
		if got := word(tc.i); got != tc.want {
			t.Errorf("word %d (%s) = %d, want %d", tc.i, tc.name, got, tc.want)
		}
	}
}

// _dl_stack_cache is derived from the gap between the two list heads thread_db
// does describe. If that gap is not one list_t the three are no longer
// consecutive, and guessing would make the runtime write pointers into an
// unrelated struct member.
func TestStacklistsDescriptorRefusesAnUnexpectedStride(t *testing.T) {
	obj, syms := buildThreadDB(t)
	// Move _dl_stack_user so the gap is 24 rather than 16.
	binary.LittleEndian.PutUint32(obj.image[0x110+8:], 2992)
	if b := stacklistsDescriptor([]*Object{obj}, syms); b != nil {
		t.Errorf("emitted %d bytes for a %d-byte stride; expected a refusal", len(b), 2992-2968)
	}
}

// A closure without glibc must produce no section at all, not a table of zeros
// the runtime would act on.
func TestStacklistsDescriptorAbsentWithoutGlibc(t *testing.T) {
	obj, _ := buildThreadDB(t)
	if b := stacklistsDescriptor([]*Object{obj}, map[string]uint64{}); b != nil {
		t.Errorf("got %d bytes with no symbols, want none", len(b))
	}
	// _rtld_global present but the thread_db descriptors missing (a stripped
	// libc) must also refuse rather than emit partial geometry.
	if b := stacklistsDescriptor([]*Object{obj}, map[string]uint64{"_rtld_global": 0x1003000}); b != nil {
		t.Errorf("got %d bytes without thread_db descriptors, want none", len(b))
	}
}

func TestEarlyDescriptor(t *testing.T) {
	if b := earlyDescriptor(map[string]uint64{"__libc_early_init": 0x11405a0}); len(b) != 8 ||
		binary.LittleEndian.Uint64(b) != 0x11405a0 {
		t.Errorf("got %x, want the 8-byte VMA 0x11405a0", b)
	}
	if b := earlyDescriptor(map[string]uint64{}); b != nil {
		t.Errorf("got %d bytes with no __libc_early_init, want none", len(b))
	}
}

// readDlsyms decodes the export table the way dlsym_lookup in
// runtime/src/context.rs does -- offsets are from the START OF THE SECTION,
// which is the detail worth pinning.
func readDlsyms(t *testing.T, b []byte) map[string]uint64 {
	t.Helper()
	out := map[string]uint64{}
	if len(b) == 0 {
		return out
	}
	count := int(binary.LittleEndian.Uint32(b))
	for i := 0; i < count; i++ {
		e := 8 + i*16
		if e+12 > len(b) {
			t.Fatalf("entry %d runs past the section (%d bytes)", i, len(b))
		}
		vma := binary.LittleEndian.Uint64(b[e:])
		noff := int(binary.LittleEndian.Uint32(b[e+8:]))
		end := noff
		for end < len(b) && b[end] != 0 {
			end++
		}
		if end >= len(b) {
			t.Fatalf("entry %d has an unterminated name at %d", i, noff)
		}
		out[string(b[noff:end])] = vma
	}
	return out
}

func TestDlsymsDescriptor(t *testing.T) {
	syms := map[string]uint64{
		"SSL_new": 0x63c720,
		"memset":  0x109c800, // an ifunc; the value is its resolver
		"strlen":  0x10a4100,
	}
	ifuncs := map[string]bool{"memset": true}

	// Resolver evaluated at build time: the table must hold the implementation.
	got := readDlsyms(t, dlsymsDescriptor(syms, ifuncs, map[uint64]uint64{0x109c800: 0x10a3a80}))
	if got["memset"] != 0x10a3a80 {
		t.Errorf("memset = %#x, want the implementation 0x10a3a80, not the resolver", got["memset"])
	}
	if got["SSL_new"] != 0x63c720 || got["strlen"] != 0x10a4100 {
		t.Errorf("plain symbols wrong: %#x %#x", got["SSL_new"], got["strlen"])
	}

	// Resolver NOT evaluatable: the name must be absent, so dlsym reports
	// not-found rather than handing back a resolver that silently no-ops.
	got = readDlsyms(t, dlsymsDescriptor(syms, ifuncs, map[uint64]uint64{}))
	if _, present := got["memset"]; present {
		t.Errorf("unresolvable ifunc must be omitted, got %#x", got["memset"])
	}
	if len(got) != 2 {
		t.Errorf("got %d exports, want the 2 non-ifunc ones", len(got))
	}
}

func TestDlsymsDescriptorIsDeterministic(t *testing.T) {
	syms := map[string]uint64{"c": 3, "a": 1, "b": 2, "aa": 4}
	first := dlsymsDescriptor(syms, nil, nil)
	for i := 0; i < 8; i++ {
		if !bytes.Equal(dlsymsDescriptor(syms, nil, nil), first) {
			t.Fatal("output varies between runs; map iteration order is leaking into the image")
		}
	}
	// A prefix must not match a longer name: the reader compares to the NUL.
	got := readDlsyms(t, first)
	if got["a"] != 1 || got["aa"] != 4 {
		t.Errorf("prefix confusion: a=%d aa=%d", got["a"], got["aa"])
	}
}

func TestDlStubEncoding(t *testing.T) {
	b := dlStub(nrDlopen)
	for i, want := range []uint32{0xd281e008, 0xd4000001, 0xd65f03c0} {
		if got := binary.LittleEndian.Uint32(b[i*4:]); got != want {
			t.Errorf("word %d = %#x, want %#x", i, got, want)
		}
	}
	// movz x8,#imm places imm at bits 5..20 and Rd=8 in the low five bits.
	for _, nr := range []uint32{nrDlopen, nrDlsym, nrDlclose, nrDlerror} {
		w := binary.LittleEndian.Uint32(dlStub(nr))
		if got := (w >> 5) & 0xffff; got != nr {
			t.Errorf("stub for %#x encodes imm %#x", nr, got)
		}
		if w&0x1f != 8 {
			t.Errorf("stub for %#x targets x%d, want x8", nr, w&0x1f)
		}
	}
}

// A definition too small to hold three instructions must be left alone rather
// than have the stub spill into whatever follows it.
func TestPatchDLStubsSkipsUndersizedDefinitions(t *testing.T) {
	img := make([]byte, 0x2000)
	for i := range img {
		img[i] = 0xAA
	}
	obj := buildObj(0x400000, img)
	obj.symbols = []elf.Symbol{
		{Name: "dlopen", Value: 0x100, Size: dlStubLen, Section: 1,
			Info: uint8(elf.ST_INFO(elf.STB_GLOBAL, elf.STT_FUNC))},
		{Name: "dlsym", Value: 0x200, Size: dlStubLen - 1, Section: 1,
			Info: uint8(elf.ST_INFO(elf.STB_GLOBAL, elf.STT_FUNC))},
	}
	done := patchDLStubs([]*Object{obj})
	if len(done) != 1 || done[0] != "dlopen" {
		t.Fatalf("patched %v, want only dlopen", done)
	}
	if binary.LittleEndian.Uint32(img[0x100:]) != 0xd281e008 {
		t.Errorf("dlopen was not patched")
	}
	for i := 0x200; i < 0x200+dlStubLen; i++ {
		if img[i] != 0xAA {
			t.Fatalf("undersized dlsym was overwritten at %#x", i)
		}
	}
}

// muslPthreadCreatePrologue is the real prologue of `pthread_create` in Alpine
// 3.20's ld-musl-aarch64.so.1, taken verbatim from `objdump -d`. It is the whole
// input to the can_do_threads decode, so it is the test.
//
//	6247c: 910003fd  mov  x29, sp
//	62480: 90000313  adrp x19, c2000
//	62484: a9025bf5  stp  x21, x22, [sp, #32]
//	62488: 528004d5  mov  w21, #0x26        // ENOSYS, the value it returns
//	6248c: 3949e264  ldrb w4, [x19, #632]   // libc.can_do_threads
//	62490: f9003fe0  str  x0, [sp, #120]
//	624a4: 34001364  cbz  w4, 62710         // -> return ENOSYS
//	624a8: 9109e260  add  x0, x19, #0x278   // &__libc again, for libc.threaded
var muslPthreadCreatePrologue = map[uint64]uint32{
	0x6246c: 0x4f00041f, // movi v31.4s, #0x0
	0x62470: 0xa9ac7bfd, // stp  x29, x30, [sp, #-320]!
	0x62474: 0x910003fd, // mov  x29, sp
	0x62478: 0xa90153f3, // stp  x19, x20, [sp, #16]
	0x6247c: 0x910203f4, // add  x20, sp, #0x80
	0x62480: 0x90000313, // adrp x19, c2000
	0x62484: 0xa9025bf5, // stp  x21, x22, [sp, #32]
	0x62488: 0x528004d5, // mov  w21, #0x26
	0x6248c: 0x3949e264, // ldrb w4, [x19, #632]
	0x62490: 0xf9003fe0, // str  x0, [sp, #120]
	0x62494: 0x3d800a9f, // str  q31, [x20, #32]
	0x62498: 0xad007e9f, // stp  q31, q31, [x20]
	0x6249c: 0xf9001a9f, // str  xzr, [x20, #48]
	0x624a0: 0xa9068fe2, // stp  x2, x3, [sp, #104]
	0x624a4: 0x34001364, // cbz  w4, 62710
	0x624a8: 0x9109e260, // add  x0, x19, #0x278
	0x624ac: 0xa90363f7, // stp  x23, x24, [sp, #48]
	0x624b0: 0xaa0103f5, // mov  x21, x1
	0x624b4: 0xa9046bf9, // stp  x25, x26, [sp, #64]
	0x624b8: 0xd53bd057, // mrs  x23, tpidr_el0
	0x624bc: 0xa90573fb, // stp  x27, x28, [sp, #80]
	0x624c0: 0xd10322f7, // sub  x23, x23, #0xc8
	0x624c4: 0x39400400, // ldrb w0, [x0, #1]
	0x624c8: 0x340012e0, // cbz  w0, 62724
}

func readerFor(insns map[uint64]uint32) func(uint64) (uint32, bool) {
	return func(at uint64) (uint32, bool) {
		w, ok := insns[at]
		return w, ok
	}
}

// The address must come out at 0xc2278 -- page 0xc2000 plus 632 -- and both the
// ldrb displacement and the later `add` immediate say so independently.
func TestMuslLibcFlagDecodesARealPrologue(t *testing.T) {
	got, ok := decodeMuslLibcFlag(0x6246c, readerFor(muslPthreadCreatePrologue))
	if !ok {
		t.Fatal("failed to decode &libc.can_do_threads from the real prologue")
	}
	if want := uint64(0xc2278); got != want {
		t.Errorf("got %#x, want %#x (adrp page 0xc2000 + 632)", got, want)
	}
}

// Every refusal matters more than the success: a wrong address writes 1 into an
// arbitrary byte of guest data, which is silent.
func TestMuslLibcFlagRefuses(t *testing.T) {
	copyOf := func() map[uint64]uint32 {
		m := make(map[uint64]uint32, len(muslPthreadCreatePrologue))
		for k, v := range muslPthreadCreatePrologue {
			m[k] = v
		}
		return m
	}

	// The two readings disagree: the `add` names a different offset than the
	// `ldrb`. One of them is not &__libc, and there is no way to tell which.
	m := copyOf()
	m[0x624a8] = 0x9109e660 // add x0, x19, #0x279
	if _, ok := decodeMuslLibcFlag(0x6246c, readerFor(m)); ok {
		t.Error("accepted two disagreeing readings of &__libc")
	}

	// Only one reading present. gcc emits both because the function needs
	// `&__libc` a second time for libc.threaded; a build that does not is a
	// build this decode has not been validated against.
	m = copyOf()
	delete(m, 0x624a8)
	m[0x624a8] = 0xd503201f // nop
	if _, ok := decodeMuslLibcFlag(0x6246c, readerFor(m)); ok {
		t.Error("accepted a single unconfirmed reading")
	}

	// A different base register: the ldrb reads through something that is not
	// the adrp result, so the page is unrelated to the displacement.
	m = copyOf()
	m[0x6248c] = 0x3949e284 // ldrb w4, [x20, #632]
	if _, ok := decodeMuslLibcFlag(0x6246c, readerFor(m)); ok {
		t.Error("accepted an ldrb through a register the adrp did not define")
	}

	// No adrp at all -- glibc exports pthread_create too, and its prologue is
	// not this. Nothing may be emitted for it.
	m = copyOf()
	m[0x62480] = 0xd503201f // nop
	if _, ok := decodeMuslLibcFlag(0x6246c, readerFor(m)); ok {
		t.Error("accepted a prologue with no adrp")
	}

	// Unreadable code (a symbol pointing outside any mapped object).
	if _, ok := decodeMuslLibcFlag(0x6246c, func(uint64) (uint32, bool) { return 0, false }); ok {
		t.Error("accepted an unreadable prologue")
	}
}

// A closure with no musl -- or one whose pthread_create is not exported -- must
// produce a zero third word rather than an address.
func TestMuslTPDescriptorOmitsTheFlagWithoutPthreadCreate(t *testing.T) {
	if got := muslCanDoThreadsVMA(nil, map[string]uint64{}); got != 0 {
		t.Errorf("got %#x with no pthread_create, want 0", got)
	}
}

// The real bodies from Debian bookworm's aarch64 glibc 2.36, taken verbatim
// from `objdump -d`. They are the whole input to the _dl_minsigstacksize
// derivation, so they are the test.
//
// __getpagesize, at 0xe7900 (68 bytes). `assert (GLRO(dl_pagesize) != 0);
// return GLRO(dl_pagesize);` -- one assert, giving offsetof _dl_pagesize == 24
// and the GOT slot 0x1af000 + 3712 = 0x1afe80 that holds &_rtld_global_ro.
var glibcGetpagesize = map[uint64]uint32{
	0xe7900: 0xd503245f, // bti  c
	0xe7904: 0x90000640, // adrp x0, 1af000
	0xe7908: 0xf9474000, // ldr  x0, [x0, #3712]
	0xe790c: 0xf9400c00, // ldr  x0, [x0, #24]      <- _dl_pagesize
	0xe7910: 0xb4000040, // cbz  x0, e7918          <- the assert
	0xe7914: 0xd65f03c0, // ret
	0xe7918: 0xd503233f, // paciasp
	0xe791c: 0xa9bf7bfd, // stp  x29, x30, [sp, #-16]!
	0xe7920: 0xb00003e3, // adrp x3, 164000
	0xe7924: 0xb00003e1, // adrp x1, 164000
	0xe7928: 0x910003fd, // mov  x29, sp
	0xe792c: 0xb00003e0, // adrp x0, 164000
	0xe7930: 0xd503201f, // (padding, to fill the declared 68 bytes)
	0xe7934: 0xd503201f,
	0xe7938: 0xd503201f,
	0xe793c: 0xd503201f,
	0xe7940: 0xd503201f,
}

// __sysconf's three `assert (GLRO(dl_minsigstacksize) != 0)` sites, each a
// four-instruction quadruple, all naming offset 32. Both cbz and cbnz polarity
// appear, which is why the matcher accepts either. The surrounding
// instructions are kept because they are what a looser matcher would trip on.
var glibcSysconfAsserts = map[uint64]uint32{
	// _SC_SIGSTKSZ: minsigstacksize * 4.
	0xda2b8: 0xf2a00060, // movk x0, #0x3, lsl #16
	0xda2bc: 0x17ffffcd, // b    da1f0
	0xda2c0: 0xb00006a0, // adrp x0, 1af000
	0xda2c4: 0xf9474000, // ldr  x0, [x0, #3712]
	0xda2c8: 0xf9401001, // ldr  x1, [x0, #32]
	0xda2cc: 0xb4002e21, // cbz  x1, da890
	0xda2d0: 0xd37ef420, // lsl  x0, x1, #2
	// _SC_MINSIGSTKSZ: return it if non-zero.
	0xda3ec: 0x92800000, // mov  x0, #-1
	0xda3f0: 0x17ffff80, // b    da1f0
	0xda3f4: 0xb00006a0, // adrp x0, 1af000
	0xda3f8: 0xf9474000, // ldr  x0, [x0, #3712]
	0xda3fc: 0xf9401000, // ldr  x0, [x0, #32]
	0xda400: 0xb5ffef80, // cbnz x0, da1f0
	0xda404: 0xb00006a0, // adrp x0, 1af000
	// The _SC_SIGSTKSZ clamp against 128 KiB.
	0xda748: 0x17fffeaa, // b    da1f0
	0xda74c: 0xd503249f, // bti  j
	0xda750: 0xb00006a0, // adrp x0, 1af000
	0xda754: 0xf9474000, // ldr  x0, [x0, #3712]
	0xda758: 0xf9401000, // ldr  x0, [x0, #32]
	0xda75c: 0xb4000780, // cbz  x0, da84c
	0xda760: 0xf140801f, // cmp  x0, #0x20, lsl #12
}

// glroFixture wires the two real bodies into one object with the symbol table
// and GOT slot the derivation reads, and returns it with the symbol map.
func glroFixture() (*Object, map[string]uint64) {
	const rtldRO = 0x3fb68 // _rtld_global_ro's own address, from ld.so
	img := make([]byte, 0x200000)
	for at, w := range glibcGetpagesize {
		binary.LittleEndian.PutUint32(img[at:], w)
	}
	for at, w := range glibcSysconfAsserts {
		binary.LittleEndian.PutUint32(img[at:], w)
	}
	// The relocated GOT slot: adrp page 0x1af000 + 3712.
	binary.LittleEndian.PutUint64(img[0x1af000+3712:], rtldRO)

	o := buildObj(0, img)
	o.symbols = []elf.Symbol{
		{Name: "__getpagesize", Value: 0xe7900, Size: 68, Section: elf.SectionIndex(13)},
		{Name: "__sysconf", Value: 0xda1a0, Size: 1844, Section: elf.SectionIndex(13)},
	}
	return o, map[string]uint64{
		"_rtld_global_ro": rtldRO,
		"__getpagesize":   0xe7900,
		"__sysconf":       0xda1a0,
	}
}

// _dl_minsigstacksize must come out at _rtld_global_ro + 32: read out of
// sysconf's own assert, and confirmed to sit immediately after _dl_pagesize,
// whose offset __getpagesize gives as 24.
func TestMinsigstacksizeIsDerivedFromTwoAccessors(t *testing.T) {
	o, syms := glroFixture()
	got := minsigstacksizeVMA([]*Object{o}, syms)
	if want := uint64(0x3fb68 + 32); got != want {
		t.Fatalf("got %#x, want %#x (_rtld_global_ro + 32)", got, want)
	}
	// The two readings, separately, so a failure says which one moved.
	if pg := glroAsserts([]*Object{o}, syms, "__getpagesize", wordOf(o)); len(pg) != 1 || pg[0].field != 24 {
		t.Errorf("__getpagesize gave %v, want exactly one assert on offset 24", pg)
	}
	if sc := glroAsserts([]*Object{o}, syms, "__sysconf", wordOf(o)); len(sc) != 3 {
		t.Errorf("__sysconf gave %d asserts, want the 3 GLRO(dl_minsigstacksize) sites", len(sc))
	}
}

func wordOf(o *Object) func(uint64) (uint32, bool) {
	return func(at uint64) (uint32, bool) {
		w, err := readInsn([]*Object{o}, at)
		return w, err == nil
	}
}

// Every refusal, because a wrong offset writes 5120 into some other member of
// _rtld_global_ro and glibc then misbehaves somewhere else entirely.
func TestMinsigstacksizeRefuses(t *testing.T) {
	for name, breakIt := range map[string]func(*Object, map[string]uint64){
		// The GOT slot does not hold &_rtld_global_ro: the accessor being
		// decoded reads some other global, so its offset means nothing here.
		"the GOT slot names a different object": func(o *Object, _ map[string]uint64) {
			binary.LittleEndian.PutUint64(o.image[0x1af000+3712:], 0xdead000)
		},
		// sysconf asserts on two different fields, so which one is
		// _dl_minsigstacksize is no longer decidable.
		"sysconf asserts on two fields": func(o *Object, _ map[string]uint64) {
			binary.LittleEndian.PutUint32(o.image[0xda758:], 0xf9401400) // ldr x0,[x0,#40]
		},
		// The field is no longer adjacent to _dl_pagesize, so the struct is not
		// the one this derivation was validated against.
		"the fields are not adjacent": func(o *Object, _ map[string]uint64) {
			for _, at := range []uint64{0xda2c8, 0xda3fc, 0xda758} {
				w := binary.LittleEndian.Uint32(o.image[at:])
				binary.LittleEndian.PutUint32(o.image[at:], (w&^(0xfff<<10))|(6<<10)) // #48
			}
		},
		// sysconf reads only _dl_pagesize: there is no minsigstacksize assert to
		// find, and the adjacency alone must not be enough.
		"sysconf has no other asserted field": func(o *Object, _ map[string]uint64) {
			for _, at := range []uint64{0xda2c8, 0xda3fc, 0xda758} {
				w := binary.LittleEndian.Uint32(o.image[at:])
				binary.LittleEndian.PutUint32(o.image[at:], (w&^(0xfff<<10))|(3<<10)) // #24
			}
		},
		// No ld.so in the closure at all.
		"no _rtld_global_ro": func(_ *Object, syms map[string]uint64) {
			delete(syms, "_rtld_global_ro")
		},
		// A symbol with no declared size cannot bound the scan, and an unbounded
		// scan reports a neighbouring function's offsets as if they were these.
		"__sysconf has no size": func(o *Object, _ map[string]uint64) {
			for i := range o.symbols {
				if o.symbols[i].Name == "__sysconf" {
					o.symbols[i].Size = 0
				}
			}
		},
	} {
		o, syms := glroFixture()
		breakIt(o, syms)
		if got := minsigstacksizeVMA([]*Object{o}, syms); got != 0 {
			t.Errorf("%s: got %#x, want a refusal", name, got)
		}
	}
}

// The encodings are REAL, taken from `objdump -d` over pthread_create in
// .agents-workspace/fixtures/busybox-musl.fused. A mask derived by hand is
// wrong more often than not, so the decoder is checked against instructions
// that actually exist rather than against my reading of the manual.
func TestMuslLibcTLSOffsetDecodesPthreadCreate(t *testing.T) {
	// 0x6c2000 + 0x278 = 0x6c2278 = &__libc on that image.
	const libc = 0x6c2278
	insns := []uint32{
		0x4f000400, // movi v0.4s, #0
		0x90000313, // adrp x19, 6c2000
		0x3949e264, // ldrb w4, [x19, #632]     -- can_do_threads, +0
		0x9109e260, // add  x0, x19, #0x278     -- caller-saved: NOT followed
		0x39400400, // ldrb w0, [x0, #1]        -- threaded, +1
		0x9109e276, // add  x22, x19, #0x278    -- callee-saved: followed
		0xf9400ec0, // ldr  x0, [x22, #24]      -- tls_size
		0xf941cf42, // ldr  x2, [x26, #920]     -- a DIFFERENT base: must be ignored
		0xf9401ac0, // ldr  x0, [x22, #48]      -- page_size
	}
	at := uint64(0x6623ec)
	word := func(a uint64) (uint32, bool) {
		i := (a - at) / 4
		if a < at || i >= uint64(len(insns)) {
			return 0, false
		}
		return insns[i], true
	}
	if got := muslLibcTLSOffset(at, libc, word); got != 24 {
		t.Fatalf("tls_size offset %d, want 24", got)
	}

	// The x26 load is the one that must be ignored: attributing it to &__libc
	// would put 920 in the set and, without the exact-set requirement, could
	// hand the runtime a wrong offset instead of no offset.
	insns[7] = 0xf9400ec0 // make it ldr x0, [x22, #24] as well -- still {24,48}
	if got := muslLibcTLSOffset(at, libc, word); got != 24 {
		t.Errorf("a duplicate load changed the answer: %d", got)
	}

	// A THIRD field read off &__libc means this is not the layout we decoded,
	// so the answer must be "cannot confirm", not a guess.
	insns[7] = 0xf9402ac0 // ldr x0, [x22, #80]
	if got := muslLibcTLSOffset(at, libc, word); got != 0 {
		t.Errorf("an unexpected field at +80 still produced offset %d; it must fail closed", got)
	}

	// Only one of the two expected fields is not enough either.
	insns[7] = 0xf9400ec0
	insns[8] = 0x4f000400 // drop the page_size load
	if got := muslLibcTLSOffset(at, libc, word); got != 0 {
		t.Errorf("a single load produced offset %d; both fields must be present", got)
	}

	// An adrp for a DIFFERENT object must not seed the base set at all.
	insns[8] = 0xf9401ac0
	if got := muslLibcTLSOffset(at, libc+0x1000, word); got != 0 {
		t.Errorf("loads off an unrelated &__libc produced offset %d", got)
	}
}

// Real encodings, from `objdump -d` over __pthread_get_minstack in
// debian:trixie-slim's libc.so.6. The instruction that matters is the `ldp`:
// its imm7 is SCALED BY 8, and a decoder that forgot that would report offset
// 59 instead of 472 -- a plausible-looking number pointing at the wrong member
// of _rtld_global_ro.
func TestDecodeLdp64AgainstGlibc(t *testing.T) {
	// ldp x1, x2, [x0, #472]
	off, ok := decodeLdp64(0xa95d8801, 0)
	if !ok || off != 472 {
		t.Fatalf("decodeLdp64 = (%d, %v), want (472, true)", off, ok)
	}
	// The base register is part of the match: the same instruction off x3 is
	// not this one.
	if _, ok := decodeLdp64(0xa95d8801, 3); ok {
		t.Error("matched an ldp off the wrong base register")
	}
	// A STORE pair must not match a load decoder.
	if _, ok := decodeLdp64(0xa95d8801&^0x00400000, 0); ok {
		t.Error("matched stp as ldp")
	}
	// ...and neither must the plain ldr from the same function.
	if _, ok := decodeLdp64(0xf9400c00, 0); ok {
		t.Error("matched ldr as ldp")
	}
	// The neighbouring ldr IS the page-size read, at +24: 0xf9400c00 has
	// imm12=3, scaled by 8. This pins the cross-check the TLS decode uses.
	if off, ok := decodeLdr64Unsigned(0xf9400c00, 0); !ok || off != 24 {
		t.Errorf("decodeLdr64Unsigned = (%d, %v), want (24, true)", off, ok)
	}
}
