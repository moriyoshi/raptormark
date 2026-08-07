package fuse

import (
	"encoding/binary"
	"testing"
)

func TestInstructionPredicates(t *testing.T) {
	// Encodings taken from an objdump of the fused nginx image.
	if !isADRP(0x90000003) {
		t.Error("adrp x3, . not recognised")
	}
	if !isADDImm(0x91371063) {
		t.Error("add x3, x3, #0xdc4 not recognised")
	}
	if isADDImm(0xd1371063) {
		t.Error("sub must not be taken for add")
	}
	if got := adrpTarget(0x90000003, 0x141fe2c); got != 0x141f000 {
		t.Errorf("adrp target = %#x, want 0x141f000", got)
	}
	// A negative ADRP displacement must sign-extend.
	if got := adrpTarget(0x90ffffe0, 0x100000); got == 0x100000 {
		t.Error("negative adrp displacement did not move the page")
	}

	for _, w := range []uint32{
		0xa9bd7bfd, // stp x29, x30, [sp, #-0x30]!
		0xa9b57bfd, // stp x29, x30, [sp, #-0xb0]!
		0xd10083ff, // sub sp, sp, #0x20
		0xd503233f, // paciasp
		0xd503245f, // bti c
	} {
		if !isPrologue(w) {
			t.Errorf("%#x should be a prologue", w)
		}
	}
	for _, w := range []uint32{
		0xa94153f3, // ldp x19, x20, [sp, #0x10]  -- an epilogue
		0x91371063, // add
		0xaa1403e0, // mov
	} {
		if isPrologue(w) {
			t.Errorf("%#x must not be a prologue", w)
		}
	}
	for _, w := range []uint32{
		0xd65f03c0, // ret
		0x17ffce12, // b (backward)
		0xd61f0220, // br x17
	} {
		if !isTerminator(w) {
			t.Errorf("%#x should be a terminator", w)
		}
	}
	if isTerminator(0x940132cc) {
		t.Error("bl must not be a terminator -- it returns")
	}
}

// buildExec lays out one executable span holding insns at the given addresses.
func buildExec(secAddr uint64, size int, insns map[uint64][]uint32) []execRange {
	data := make([]byte, size)
	for at, ws := range insns {
		for i, w := range ws {
			binary.LittleEndian.PutUint32(data[at-secAddr+uint64(i)*4:], w)
		}
	}
	return []execRange{{addr: secAddr, size: uint64(size), data: data}}
}

// The musl case, reduced: a static function reached only through a pointer
// materialised by adrp/add. No symbol, no FDE, no BL -- if this is not
// recovered, elflift never lifts it and the guest dies at _ecv_unreached.
func TestCodePointerFuncsRecoversLaunderedPointer(t *testing.T) {
	const sec = uint64(0x1000)
	target := uint64(0x1dc4)
	insns := map[uint64][]uint32{
		// The hidden function: preceded by a ret, starts with a prologue.
		target - 4: {0xd65f03c0},
		target:     {0xa9bd7bfd, 0x910003fd},
		// A known function after it, giving the size bound.
		0x1dfc: {0xa9bd7bfd},
		// The caller materialising the pointer.
		0x1e2c: {0x90000003, 0x91371063}, // adrp x3, 0x1000 ; add x3, x3, #0xdc4
	}
	known := []funcRange{{addr: 0x1dfc, size: 4}}
	got := codePointerFuncs(buildExec(sec, 0x1000, insns), known)
	if len(got) != 1 {
		t.Fatalf("recovered %v, want exactly the laundered target %#x", got, target)
	}
	if got[0].addr != target {
		t.Errorf("recovered %#x, want %#x", got[0].addr, target)
	}
	// Sized by the distance to the next boundary, which is the most that can
	// honestly be claimed.
	if want := uint64(0x1dfc - 0x1dc4); got[0].size != want {
		t.Errorf("size %d, want %d", got[0].size, want)
	}
}

// The dangerous direction: a computed address that is NOT a function. Jump
// tables and switch bases live in .text and are formed the same way, and
// declaring one a function makes elflift disassemble data as code.
func TestCodePointerFuncsRejectsNonFunctions(t *testing.T) {
	const sec = uint64(0x1000)
	for name, setup := range map[string]map[uint64][]uint32{
		"no prologue at the target": {
			0x1dc0: {0xd65f03c0},
			0x1dc4: {0xaa1403e0}, // mov -- data or mid-function
			0x1e2c: {0x90000003, 0x91371063},
		},
		"not preceded by a terminator": {
			0x1dc0: {0x910003fd}, // mov x29, sp -- we are mid-function
			0x1dc4: {0xa9bd7bfd},
			0x1e2c: {0x90000003, 0x91371063},
		},
		"register clobbered between adrp and add": {
			0x1dc0: {0xd65f03c0},
			0x1dc4: {0xa9bd7bfd},
			0x1e2c: {0x90000003, 0xaa1403e3, 0x91371063}, // mov x3, x20 kills it
		},
	} {
		if got := codePointerFuncs(buildExec(sec, 0x1000, setup), nil); len(got) != 0 {
			t.Errorf("%s: recovered %v, want nothing", name, got)
		}
	}
}

// musl's qsort passes a two-instruction trampoline as its comparison wrapper.
// It has no symbol, no FDE and no prologue, so the prologue test alone rejects
// it -- and nginx then dies at the first qsort with "not in the lifted function
// table". Encodings are from the fused nginx image.
func TestCodePointerFuncsRecoversTrampoline(t *testing.T) {
	const sec = uint64(0x1000)
	target := uint64(0x1ad4)
	insns := map[uint64][]uint32{
		target - 4: {0xd65f03c0},             // ret, ending the previous function
		target:     {0xaa0203f0, 0xd61f0200}, // mov x16, x2 ; br x16
		0x1adc:     {0xa9bd7bfd},             // the next real function, bounding it
		// qsort itself, materialising the trampoline's address.
		0x1b00: {0x90000003, 0x912b5063}, // adrp x3, 0x1000 ; add x3, x3, #0xad4
	}
	got := codePointerFuncs(buildExec(sec, 0x1000, insns), []funcRange{{addr: 0x1adc, size: 4}})
	if len(got) != 1 || got[0].addr != target {
		t.Fatalf("recovered %v, want the trampoline at %#x", got, target)
	}
	if want := uint64(0x1adc - 0x1ad4); got[0].size != want {
		t.Errorf("size %d, want %d", got[0].size, want)
	}
}

// A trampoline must be sized by its OWN branch, not by the distance to the next
// known boundary. This is the at_quick_exit case from the fused nginx image: the
// function following the trampoline has no symbol and no FDE, so the next known
// boundary is 184 bytes away, and sizing by distance handed the trampoline the
// whole of its unnamed neighbour -- a boundary declared in the middle of a real
// function, which is precisely what this file's filter exists to prevent.
func TestTrampolineIsSizedByItsOwnBranch(t *testing.T) {
	const sec = uint64(0x1000)
	target := uint64(0x15f4)
	insns := map[uint64][]uint32{
		target - 4: {0xd65f03c0},             // ret
		target:     {0xaa0003f0, 0xd61f0200}, // mov x16, x0 ; br x16
		// An unnamed function immediately after the trampoline. Nothing marks
		// it, so it cannot bound anything.
		0x15fc: {0xa9bb7bfd, 0x910003fd},
		// The next boundary that IS known, far away.
		0x16b0: {0xa9bd7bfd},
		// The caller materialising the trampoline's address.
		0x1700: {0x90000000, 0x9117d000}, // adrp x0, 0x1000 ; add x0, x0, #0x5f4
	}
	got := codePointerFuncs(buildExec(sec, 0x1000, insns), []funcRange{{addr: 0x16b0, size: 4}})
	if len(got) != 1 || got[0].addr != target {
		t.Fatalf("recovered %v, want the trampoline at %#x", got, target)
	}
	if got[0].size != 8 {
		t.Errorf("size %d, want 8 -- sizing by the next boundary would give %d and swallow "+
			"the unnamed function at %#x", got[0].size, 0x16b0-target, 0x15fc)
	}
}

// The relaxation must stay narrow: a computed address whose word is neither a
// prologue nor an indirect branch is still data as far as we know.
func TestTrampolinePredicateIsNarrow(t *testing.T) {
	word := func(m map[uint64]uint32) func(uint64) (uint32, bool) {
		return func(a uint64) (uint32, bool) { w, ok := m[a]; return w, ok }
	}
	if got := trampolineLen(0, word(map[uint64]uint32{0: 0xd61f0200})); got != 4 {
		t.Errorf("a bare br x16 is a 4-byte trampoline, got %d", got)
	}
	if got := trampolineLen(0, word(map[uint64]uint32{0: 0xaa0203f0, 4: 0xd61f0200})); got != 8 {
		t.Errorf("mov then br is an 8-byte trampoline, got %d", got)
	}
	if trampolineLen(0, word(map[uint64]uint32{0: 0x91371063, 4: 0xd61f0200})) != 0 {
		t.Error("an add before the branch is not a register move")
	}
	// A long run of moves is a function body, not a thunk; the scan gives up.
	movs := map[uint64]uint32{}
	for i := uint64(0); i < 8; i++ {
		movs[i*4] = 0xaa0203f0
	}
	if trampolineLen(0, word(movs)) != 0 {
		t.Error("an unbounded run of movs must not count as a trampoline")
	}
	if trampolineLen(0, word(map[uint64]uint32{})) != 0 {
		t.Error("an unreadable address is not a trampoline")
	}
}

// musl's `__setxid` hands the static `do_setxid` to `__synccall`. The callback
// pushes no frame -- it starts `mov x3, x0` -- so the prologue test rejects it and
// it has neither a symbol nor an FDE. nginx reaches it only when a master drops
// privileges for its workers, and without this the first `setgid` kills master
// mode with "vma 0x1469a00 not in the lifted function table". Encodings are from
// the fused nginx image.
func TestCodePointerFuncsRecoversFramelessCallback(t *testing.T) {
	const sec = uint64(0x1000)
	target := uint64(0x1a00)
	insns := map[uint64][]uint32{
		0x19fc: {0x1400001e},             // b -- ends the previous function
		target: {0xaa0003e3, 0xb9401000}, // mov x3, x0 ; ldr w0, [x0, #16]
		0x1abc: {0xa9bd7bfd},             // the next known function, bounding it
		// __setxid materialising the callback and passing it to __synccall.
		0x1a90: {0x90000000, 0x91280000, 0x97ffed26}, // adrp x0 ; add x0,x0,#0xa00 ; bl
	}
	got := codePointerFuncs(buildExec(sec, 0x1000, insns), []funcRange{{addr: 0x1abc, size: 4}})
	if len(got) != 1 || got[0].addr != target {
		t.Fatalf("recovered %v, want the frameless callback at %#x", got, target)
	}
}

// The use-based test is the only thing separating a frameless code pointer from
// data in .text, so it has to hold in the rejecting direction too.
func TestPassedToCallIsNarrow(t *testing.T) {
	word := func(m map[uint64]uint32) func(uint64) (uint32, bool) {
		return func(a uint64) (uint32, bool) { w, ok := m[a]; return w, ok }
	}
	// The accepting shape: `add x0, ...` at 0, `bl` at 4.
	if !passedToCall(0, 0, word(map[uint64]uint32{4: 0x97ffed26})) {
		t.Error("a value passed to bl in x0 should be accepted")
	}
	// Called directly.
	if !passedToCall(0, 16, word(map[uint64]uint32{4: 0xd63f0200})) { // blr x16
		t.Error("blr on the register itself should be accepted")
	}
	for name, m := range map[string]map[uint64]uint32{
		// A jump-table base: dereferenced, never called.
		"used as a load base": {4: 0x38614841}, // ldrb w1, [x2, w1, uxtw] with Rn=2
		// Overwritten before any call.
		"clobbered first": {4: 0xaa1403e2, 8: 0x97ffed26},
		// The block ends without a call.
		"branch before the call": {4: 0xd65f03c0},
		// Nothing readable there.
		"unreadable": {},
	} {
		rd := uint32(2)
		if name == "clobbered first" {
			rd = 2
		}
		if passedToCall(0, rd, word(m)) {
			t.Errorf("%s: must not count as a call argument", name)
		}
	}
	// Too far away: a value called nine instructions later is not something this
	// scan should be guessing about.
	if passedToCall(0, 0, word(map[uint64]uint32{
		4: 0xd503201f, 8: 0xd503201f, 12: 0xd503201f, 16: 0xd503201f, 20: 0x97ffed26,
	})) {
		t.Error("a call past the window must not be accepted")
	}
	// A pointer in a non-argument register at a `bl` is not an argument.
	if passedToCall(0, 19, word(map[uint64]uint32{4: 0x97ffed26})) {
		t.Error("x19 at a bl is not an argument register")
	}
}

// TestStoreEncodings pins every mask in is64BitStore against a real encoding.
// Each comment is the mnemonic that assembled to it (aarch64-linux-gnu-as), and
// the LDR/LDP rejections are the point: a store and a load differ in one bit,
// and a mask that confuses them inverts the meaning of storedAsPointer from
// "written as a value" to "read through as an address".
func TestStoreEncodings(t *testing.T) {
	for _, tc := range []struct {
		w           uint32
		rt, rt2, rn uint32
		what        string
	}{
		{0xf9002a63, 3, 32, 19, "str x3, [x19, #80]"},
		{0xf9000263, 3, 32, 19, "str x3, [x19]"},
		{0xf81f8263, 3, 32, 19, "stur x3, [x19, #-8]"},
		{0xf8008e63, 3, 32, 19, "str x3, [x19, #8]!"},
		{0xf8008663, 3, 32, 19, "str x3, [x19], #8"},
		{0xa9011263, 3, 4, 19, "stp x3, x4, [x19, #16]"},
		{0xa9bf1263, 3, 4, 19, "stp x3, x4, [x19, #-16]!"},
		{0xa8811263, 3, 4, 19, "stp x3, x4, [x19], #16"},
	} {
		rt, rt2, rn, ok := is64BitStore(tc.w)
		if !ok {
			t.Errorf("%s (%#x) not recognised as a store", tc.what, tc.w)
			continue
		}
		if rt != tc.rt || rt2 != tc.rt2 || rn != tc.rn {
			t.Errorf("%s: got rt=%d rt2=%d rn=%d, want %d/%d/%d",
				tc.what, rt, rt2, rn, tc.rt, tc.rt2, tc.rn)
		}
	}
	for _, tc := range []struct {
		w    uint32
		what string
	}{
		{0xf9402a63, "ldr x3, [x19, #80]"},
		{0xa9411263, "ldp x3, x4, [x19, #16]"},
		{0xf8406263, "ldur x3, [x19, #6]"},
		{0x91371063, "add x3, x3, #0xdc4"},
		{0xd65f03c0, "ret"},
	} {
		if _, _, _, ok := is64BitStore(tc.w); ok {
			t.Errorf("%s (%#x) must not be taken for a store", tc.what, tc.w)
		}
	}
}

// storedAsPointer is the third and loosest acceptance rule, so its rejecting
// direction is what keeps it from marking data as code.
func TestStoredAsPointerIsNarrow(t *testing.T) {
	word := func(m map[uint64]uint32) func(uint64) (uint32, bool) {
		return func(a uint64) (uint32, bool) { w, ok := m[a]; return w, ok }
	}
	// The accepting shape, from musl's __fdopen: add x3 at 0, str x3,[x19,#80].
	if !storedAsPointer(0, 3, word(map[uint64]uint32{4: 0xf9002a63})) {
		t.Error("a value stored as the Rt of a str should be accepted")
	}
	// The same value as the second register of an stp.
	if !storedAsPointer(0, 4, word(map[uint64]uint32{4: 0xa9011263})) {
		t.Error("a value stored as the Rt2 of an stp should be accepted")
	}
	for name, tc := range map[string]struct {
		rd uint32
		m  map[uint64]uint32
	}{
		// The discrimination that matters: the register is the store's BASE, so
		// the value is an address being written THROUGH -- a data pointer.
		"stored through as a base": {19, map[uint64]uint32{4: 0xf9002a63}},
		// Loaded through: a jump-table base or a literal pool.
		"loaded through as a base": {19, map[uint64]uint32{4: 0xf9402a63}},
		// Overwritten before any store.
		"clobbered first": {3, map[uint64]uint32{4: 0xaa1403e3, 8: 0xf9002a63}},
		// The block ends first.
		"ret before the store": {3, map[uint64]uint32{4: 0xd65f03c0}},
		// Past the window.
		"stored too late": {3, map[uint64]uint32{
			4: 0xd503201f, 8: 0xd503201f, 12: 0xd503201f, 16: 0xd503201f, 20: 0xf9002a63,
		}},
		"unreadable": {3, map[uint64]uint32{}},
	} {
		if storedAsPointer(0, tc.rd, word(tc.m)) {
			t.Errorf("%s: must not count as a stored code pointer", name)
		}
	}
}

// The musl __stdio_seek case end to end. It is frameless, it is not a
// trampoline (a `b` to a label is not a `br`), and its address is stored rather
// than called -- so it is recovered by storedAsPointer or not at all.
//
// The second assertion is the half that is easy to forget: recovering the start
// must also SHRINK the candidate before it. __stdio_seek sat inside a
// previously-recovered function's claimed range, and a boundary in the middle of
// a claimed function is what made the indirect call fail in the first place.
func TestCodePointerFuncsRecoversAStoredFramelessPointer(t *testing.T) {
	const sec = uint64(0x1000)
	prev, target := uint64(0x1294), uint64(0x1378)
	insns := map[uint64][]uint32{
		// An earlier frameless function, recovered because its address is
		// passed to a call; it would otherwise claim everything up to 0x1380.
		prev - 4: {0xd65f03c0}, // ret
		prev:     {0xaa0003e3}, // mov x3, x0  -- frameless
		0x1200: {0x90000000, 0x910a5000, // adrp x0 ; add x0, x0, #0x294
			0x94000001}, // bl -- passedToCall accepts prev
		// __stdio_seek: preceded by an unconditional branch, frameless.
		target - 4: {0x17ffffe9},                         // b (terminator)
		target:     {0xb9407800, 0x1400568c},             // ldr w0,[x0,#120] ; b lseek
		0x1240:     {0x90000003, 0x910de063, 0xf9002a63}, // adrp x3 ; add x3,x3,#0x378 ; str x3,[x19,#80]
		// A known symbol after both, bounding the last one.
		0x1380: {0xa9ba7bfd},
	}
	known := []funcRange{{addr: 0x1380, size: 0x21c}}
	got := codePointerFuncs(buildExec(sec, 0x1000, insns), known)

	byAddr := map[uint64]uint64{}
	for _, fr := range got {
		byAddr[fr.addr] = fr.size
	}
	if _, ok := byAddr[target]; !ok {
		t.Fatalf("did not recover the stored frameless pointer %#x; got %v", target, got)
	}
	if size, ok := byAddr[prev]; !ok {
		t.Errorf("lost the passed-to-call recovery at %#x; got %v", prev, got)
	} else if want := target - prev; size != want {
		t.Errorf("the earlier function still claims %d bytes, want %d -- it must stop "+
			"at the newly recovered start, or the indirect call to %#x still lands "+
			"inside another function", size, want, target)
	}
}

// An address already described by a symbol or FDE must not be re-reported.
func TestCodePointerFuncsSkipsKnownBoundaries(t *testing.T) {
	const sec = uint64(0x1000)
	insns := map[uint64][]uint32{
		0x1dc0: {0xd65f03c0},
		0x1dc4: {0xa9bd7bfd},
		0x1e2c: {0x90000003, 0x91371063},
	}
	ranges := buildExec(sec, 0x1000, insns)
	if got := codePointerFuncs(ranges, []funcRange{{addr: 0x1dc4, size: 0x38}}); len(got) != 0 {
		t.Errorf("re-reported a known start: %v", got)
	}
	// Also when it merely falls INSIDE a known range.
	if got := codePointerFuncs(ranges, []funcRange{{addr: 0x1d00, size: 0x200}}); len(got) != 0 {
		t.Errorf("re-reported an address inside a known function: %v", got)
	}
}
