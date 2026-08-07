package fuse

import (
	"debug/elf"
	"encoding/binary"
	"testing"
)

// buildObj wraps a byte image as a single fused object at base.
func buildObj(base uint64, image []byte) *Object {
	return &Object{Name: "test", LibIndex: 0, Base: base, lo: 0, hi: uint64(len(image)), image: image}
}

func putInsns(img []byte, off uint64, insns []uint32) {
	for i, w := range insns {
		binary.LittleEndian.PutUint32(img[off+uint64(i)*4:], w)
	}
}

// These are the real encodings of glibc's aarch64 memchr and strlen ifunc
// resolvers, taken verbatim from `objdump -d libc.so.6`. They are the whole
// reason this evaluator exists, so they are the test.
var (
	memchrResolver = []uint32{
		0x90000861, // adrp x1, 0x1a0000
		0x90000040, // adrp x0, 0x9c000
		0x911a0000, // add  x0, x0, #0x680
		0xf9473c21, // ldr  x1, [x1, #3704]
		0xf9403821, // ldr  x1, [x1, #112]
		0xd358fc22, // lsr  x2, x1, #24
		0xf101405f, // cmp  x2, #0x50
		0x54000040, // b.eq +8
		0xd65f03c0, // ret
		0xf27c2c3f, // tst  x1, #0xfff0
		0x90000041, // adrp x1, 0x9c000
		0x911e0021, // add  x1, x1, #0x780
		0x9a811000, // csel x0, x0, x1, ne
		0xd65f03c0, // ret
	}
	strlenResolver = []uint32{
		0xb0000862, // adrp x2, 0x1a0000
		0xb0000041, // adrp x1, 0x9c000
		0xb0000040, // adrp x0, 0x9c000
		0x91240021, // add  x1, x1, #0x900
		0xf9473c42, // ldr  x2, [x2, #3704]
		0x91220000, // add  x0, x0, #0x880
		0xf9412042, // ldr  x2, [x2, #576]
		0xf26e005f, // tst  x2, #0x40000
		0x9a811000, // csel x0, x0, x1, ne
		0xd65f03c0, // ret
	}
)

// resolverImage lays a resolver out at its real address inside a 2 MiB image,
// with a cpu_features pointer in the GOT slot the resolver reads. The struct
// itself is left zeroed, exactly as it is in a real image before the loader
// runs.
func resolverImage(t *testing.T, at uint64, insns []uint32, cpuFeatures uint64) *Object {
	t.Helper()
	img := make([]byte, 0x200000)
	putInsns(img, at, insns)
	// Both resolvers read the cpu_features pointer from 0x1a0000 + 3704.
	binary.LittleEndian.PutUint64(img[0x1a0000+3704:], cpuFeatures)
	return buildObj(0, img)
}

func TestResolveIfuncPicksBaselineForMemchr(t *testing.T) {
	// cpu_features is zeroed, as in an unrelocated image: (x1>>24) != 0x50, so
	// the resolver returns early with the baseline candidate.
	o := resolverImage(t, 0x945b0, memchrResolver, 0x150000)
	got, err := resolveIfunc([]*Object{o}, 0x945b0)
	if err != nil {
		t.Fatalf("resolveIfunc: %v", err)
	}
	if got != 0x9c680 {
		t.Errorf("memchr resolved to %#x, want %#x (the baseline implementation)", got, 0x9c680)
	}
}

func TestResolveIfuncPicksBaselineForStrlen(t *testing.T) {
	// tst against zeroed features sets Z, so csel takes the `ne == false` arm.
	o := resolverImage(t, 0x935a0, strlenResolver, 0x150000)
	got, err := resolveIfunc([]*Object{o}, 0x935a0)
	if err != nil {
		t.Fatalf("resolveIfunc: %v", err)
	}
	if got != 0x9c900 {
		t.Errorf("strlen resolved to %#x, want %#x (the baseline implementation)", got, 0x9c900)
	}
}

func TestResolveIfuncTakesTheFeatureArm(t *testing.T) {
	// Prove the evaluator is really interpreting rather than always returning
	// the first candidate: set the feature bit strlen tests and the other arm
	// must be selected.
	o := resolverImage(t, 0x935a0, strlenResolver, 0x150000)
	binary.LittleEndian.PutUint64(o.image[0x150000+576:], 0x40000)
	got, err := resolveIfunc([]*Object{o}, 0x935a0)
	if err != nil {
		t.Fatalf("resolveIfunc: %v", err)
	}
	if got != 0x9c880 {
		t.Errorf("with the feature bit set, strlen resolved to %#x, want %#x", got, 0x9c880)
	}
}

func TestResolveIfuncHonoursObjectBase(t *testing.T) {
	// A library is fused at a base; the resolver's adrp is PC-relative, so the
	// selected address must come back in fused space.
	const base = 0x600000
	img := make([]byte, 0x200000)
	putInsns(img, 0x935a0, strlenResolver)
	binary.LittleEndian.PutUint64(img[0x1a0000+3704:], base+0x150000)
	o := buildObj(base, img)
	got, err := resolveIfunc([]*Object{o}, base+0x935a0)
	if err != nil {
		t.Fatalf("resolveIfunc: %v", err)
	}
	if got != base+0x9c900 {
		t.Errorf("resolved to %#x, want %#x", got, base+0x9c900)
	}
}

func TestResolveIfuncRejectsUnknownInstructions(t *testing.T) {
	// Guessing would be worse than failing: an unhandled instruction must be a
	// hard error naming the word, not a silently wrong implementation choice.
	img := make([]byte, 0x1000)
	putInsns(img, 0, []uint32{0xdeadbeef})
	if _, err := resolveIfunc([]*Object{buildObj(0, img)}, 0); err == nil {
		t.Error("expected an error for an unsupported instruction")
	}
}

func TestResolveIfuncRejectsRunaway(t *testing.T) {
	// An infinite loop must terminate rather than hang the build.
	img := make([]byte, 0x1000)
	putInsns(img, 0, []uint32{0x54000000}) // b.eq +0 with Z set... falls through
	putInsns(img, 4, []uint32{0x17ffffff}) // unconditional b -4 (unsupported -> error)
	if _, err := resolveIfunc([]*Object{buildObj(0, img)}, 0); err == nil {
		t.Error("expected an error rather than looping forever")
	}
}

func TestDecodeBitMasksMatchesRealEncodings(t *testing.T) {
	// The two logical immediates the resolvers actually use.
	for _, c := range []struct {
		name          string
		n, imms, immr uint32
		want          uint64
	}{
		{"0xfff0 (memchr tst)", 1, 11, 60, 0xfff0},
		{"0x40000 (strlen tst)", 1, 0, 46, 0x40000},
	} {
		got, ok := decodeBitMasks(c.n, c.imms, c.immr)
		if !ok || got != c.want {
			t.Errorf("%s: decodeBitMasks = %#x (ok=%v), want %#x", c.name, got, ok, c.want)
		}
	}
}

// Every ifunc resolver in a BTI-enabled build — which is every resolver in
// Debian's libc — begins with `bti c`. Accepting only bare NOP made those
// resolvers unevaluatable and the whole image unfuseable, with
// "unsupported instruction 0xd503245f". The rest of the HINT space (pointer-auth
// prologues) has to pass for the same reason.
func TestResolveIfuncSkipsHintSpace(t *testing.T) {
	hints := map[string]uint32{
		"nop":     0xd503201f,
		"bti":     0xd503241f,
		"bti c":   0xd503245f,
		"bti j":   0xd503249f,
		"bti jc":  0xd50324df,
		"paciasp": 0xd503233f,
		"autiasp": 0xd50323bf,
	}
	for name, hint := range hints {
		obj := resolverImage(t, 0x1000, []uint32{
			hint,
			0x90000000, // adrp x0, . -> 0x1000 (page of the adrp at 0x1004)
			0xd65f03c0, // ret
		}, 0)
		got, err := resolveIfunc([]*Object{obj}, 0x1000)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != 0x1000 {
			t.Errorf("%s: resolved to %#x, want 0x1000", name, got)
		}
	}
}

// The HINT mask must not swallow unrelated encodings: a real unsupported
// instruction still has to be reported rather than silently skipped.
func TestResolveIfuncHintMaskIsNarrow(t *testing.T) {
	obj := resolverImage(t, 0x1000, []uint32{
		0xd503201e, // not a HINT (Rt field != 11111)
		0xd65f03c0,
	}, 0)
	if _, err := resolveIfunc([]*Object{obj}, 0x1000); err == nil {
		t.Error("expected an error for a non-HINT instruction in the HINT range")
	}
}

// An STT_GNU_IFUNC symbol's VALUE is a resolver, not an implementation. Binding
// the value straight into a GOT slot makes every cross-object call to that name
// invoke the resolver -- which returns a function pointer and touches nothing
// else. That is how `memset` became a silent no-op in fused images: OpenSSL's
// CRYPTO_zalloc stopped zeroing, and an uninitialised RCU counter deadlocked
// the guest a long way downstream. Static binaries were unaffected (their
// IRELATIVE relocs are resolved by the runtime), so nothing caught it.
func TestGlobalSymbolsFlagsIfuncDefinitions(t *testing.T) {
	img := make([]byte, 0x200000)
	// A resolver that returns 0x9999, at link-time 0x1000.
	putInsns(img, 0x1000, []uint32{
		0xd503245f, // bti c
		0x90000000, // adrp x0, . (page of 0x1004 -> 0x1000)
		0x91080000, // add x0, x0, #0x200
		0xd65f03c0, // ret
	})
	lib := buildObj(0x400000, img)
	lib.symbols = []elf.Symbol{
		{Name: "memset", Value: 0x1000, Section: elf.SectionIndex(1),
			Info: uint8(elf.ST_INFO(elf.STB_GLOBAL, sttGNUIFunc))},
		{Name: "plain_fn", Value: 0x2000, Section: elf.SectionIndex(1),
			Info: uint8(elf.ST_INFO(elf.STB_GLOBAL, elf.STT_FUNC))},
	}

	syms, ifuncs := globalSymbols([]*Object{lib})
	if !ifuncs["memset"] {
		t.Error("memset (STT_GNU_IFUNC) was not flagged as an ifunc")
	}
	if ifuncs["plain_fn"] {
		t.Error("a plain STT_FUNC symbol must not be flagged as an ifunc")
	}
	// The table still holds the resolver address; relocate() is what replaces it.
	if got, want := syms["memset"], uint64(0x400000+0x1000); got != want {
		t.Errorf("symbol table has memset=%#x, want the resolver at %#x", got, want)
	}
	// And the resolver must be evaluatable, or relocate() would fail the fuse.
	if _, err := resolveIfunc([]*Object{lib}, 0x400000+0x1000); err != nil {
		t.Errorf("resolver not evaluatable: %v", err)
	}
}

// Some glibc resolvers cannot be evaluated at fuse time -- gettimeofday's and
// memchr's both dereference the `__ifunc_arg_t` the kernel passes, which does
// not exist until there is a real call frame. Those are handed to the runtime
// through `.ecv.irela` instead of failing the fuse, so the table's byte layout
// is a contract with apply_ifuncs in runtime/src/context.rs.
func TestIrelaDescriptorLayout(t *testing.T) {
	if got := irelaDescriptor(nil); len(got) != 0 {
		t.Fatalf("no fixups must produce no section, got %d bytes", len(got))
	}
	b := irelaDescriptor([]ifuncFixup{
		{slot: 0xdff2b0, resolver: 0x109c800},
		{slot: 0x50c990, resolver: 0x109bee0},
	})
	if len(b) != 32 {
		t.Fatalf("got %d bytes, want 16 per entry", len(b))
	}
	for i, want := range []uint64{0xdff2b0, 0x109c800, 0x50c990, 0x109bee0} {
		if got := binary.LittleEndian.Uint64(b[i*8:]); got != want {
			t.Errorf("word %d = %#x, want %#x", i, got, want)
		}
	}
}

// Deferral hands the runtime a slot address and nothing else, so it is only
// sound where the relocation writes a bare 8-byte pointer. Anything with an
// addend to preserve must keep failing loudly rather than being silently
// clobbered at load time.
func TestIsPointerSlot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		typ    elf.R_AARCH64
		addend int64
		want   bool
	}{
		{"GOT slot", elf.R_AARCH64_GLOB_DAT, 0, true},
		{"PLT slot", elf.R_AARCH64_JUMP_SLOT, 0, true},
		{"plain pointer", elf.R_AARCH64_ABS64, 0, true},
		{"interior pointer", elf.R_AARCH64_ABS64, 8, false},
		{"not a pointer", elf.R_AARCH64_ABS32, 0, false},
		{"relative", elf.R_AARCH64_RELATIVE, 0, false},
	} {
		if got := isPointerSlot(tc.typ, tc.addend); got != tc.want {
			t.Errorf("%s: isPointerSlot(%v, %d) = %v, want %v", tc.name, tc.typ, tc.addend, got, tc.want)
		}
	}
}

// RELR packs relative relocations as an address followed by bitmaps of the
// words after it. Getting the cursor advance wrong (63 words per bitmap,
// whether or not their bits are set) silently misplaces every later
// relocation, so the encoding is pinned here directly.
func TestWalkRELRAddressesAndBitmaps(t *testing.T) {
	// entry 0: address 0x1000        -> relocates 0x1000, cursor -> 0x1008
	// entry 1: bitmap, bits 1 and 2  -> relocates 0x1008 and 0x1010,
	//                                   then cursor += 63*8 -> 0x1200
	// entry 2: bitmap, bit 1         -> relocates 0x1200 == 0x1000 + 64*8
	relr := make([]byte, 24)
	binary.LittleEndian.PutUint64(relr[0:], 0x1000)
	binary.LittleEndian.PutUint64(relr[8:], 1|(1<<1)|(1<<2))
	binary.LittleEndian.PutUint64(relr[16:], 1|(1<<1))

	var got []uint64
	if err := walkRELR(relr, func(v uint64) error { got = append(got, v); return nil }); err != nil {
		t.Fatalf("walkRELR: %v", err)
	}
	want := []uint64{0x1000, 0x1008, 0x1010, 0x1000 + 64*8}
	if len(got) != len(want) {
		t.Fatalf("relocated %d sites %#x, want %d %#x", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("site %d = %#x, want %#x", i, got[i], want[i])
		}
	}
}

// The addend is implicit: whatever is already stored at the site. Rebasing must
// add the object base to that value, not overwrite it.
func TestApplyRELRRebasesTheStoredValue(t *testing.T) {
	const base = 0x400000
	img := make([]byte, 0x2000)
	binary.LittleEndian.PutUint64(img[0x1000:], 0x7e1c0) // the vtable slot that broke fclose
	obj := buildObj(base, img)
	apply := func(v uint64) error {
		cur, err := obj.read64(v)
		if err != nil {
			return err
		}
		return obj.write64(v, cur+obj.Base)
	}
	relr := make([]byte, 8)
	binary.LittleEndian.PutUint64(relr, 0x1000)
	if err := walkRELR(relr, apply); err != nil {
		t.Fatal(err)
	}
	if got, want := binary.LittleEndian.Uint64(img[0x1000:]), uint64(base+0x7e1c0); got != want {
		t.Errorf("got %#x, want %#x", got, want)
	}
}
