package fuse

import (
	"debug/elf"
	"encoding/binary"
	"sort"
)

// Function-boundary recovery from computed code pointers.
//
// This is the third source of boundaries, after the symbol table and
// `.eh_frame` (ehframe.go). It exists because `.eh_frame` recovery is
// effectively a glibc technique: Debian's libc carries full unwind tables,
// but Alpine's musl ships 868 bytes of `.eh_frame` for the whole library. On a
// musl closure the first two sources between them miss every internal static
// function, and elflift cannot lift what it cannot find.
//
// The case that forced this: musl calls its own `libc_start_main_stage2`
// through a pointer laundered by an inline-asm barrier,
//
//	libc_start_main_stage2_ptr = libc_start_main_stage2;
//	__asm__("" : "+r"(ptr) : : "memory");
//	ptr(main, argc, argv);
//
// specifically so the compiler cannot turn it back into a direct call. The
// function is static (no symbol), has no FDE, and is the target of no `BL`
// anywhere in the image -- but `__libc_start_main` computes its address with an
// `adrp`/`add` pair. Without it nginx died at `_ecv_unreached 0x141fdc4` before
// reaching main.
//
// The risk is the opposite failure, and it is a real one this project has
// already hit: marking data as a function start makes elflift disassemble data
// as code. Jump tables and switch bases are also computed with `adrp`/`add`, so
// a candidate is only accepted when all of the following hold:
//
//   - the computed address lands in an executable section of this object
//   - no symbol, FDE or init stub already starts or contains it
//   - the word there is a recognisable prologue, OR the address is a bare
//     indirect-branch trampoline (see trampolineLen -- musl's `qsort` passes one
//     as a function pointer, and two instructions have no frame to push), OR the
//     value is handed straight to a call without being dereferenced (see
//     passedToCall -- musl's `do_setxid` is frameless, so only its USE identifies
//     it as code), OR the value is stored to memory without being dereferenced
//     (see storedAsPointer -- musl's `__stdio_seek` is frameless AND is never
//     called at the site that takes its address; it is written into a FILE)
//   - the word BEFORE it ends a function (ret, unconditional branch, or brk)
//
// Measured, that filter keeps 62 of 2,340 raw candidates on the nginx closure
// and 0 of 2,019 on the OpenSSL one -- it adds nothing where `.eh_frame`
// already covers the library, which is what makes it safe to run everywhere.

// aarch64 instruction predicates. Each is a (mask, value) test on one word.
func isADRP(w uint32) bool { return w&0x9f000000 == 0x90000000 }

// isADDImm matches `add Xd, Xn, #imm12` with no shift -- the second half of the
// standard address-materialisation pair.
func isADDImm(w uint32) bool { return w&0xff800000 == 0x91000000 }

// isMovReg matches `mov Xd, Xm` (ORR Xd, XZR, Xm with no shift) -- the only
// instruction a branch trampoline needs before its indirect jump.
func isMovReg(w uint32) bool { return w&0xffe0ffe0 == 0xaa0003e0 }

// isTrampoline recognises a function that is nothing but an indirect branch,
// optionally preceded by register moves that set up its target or arguments.
//
// This exists because musl's `qsort` passes one as a function pointer:
//
//	qsort:      mov x4, x3 ; adrp x3, . ; add x3, x3, #0xad4 ; b qsort_r
//	0x...ad4:   mov x16, x2 ; br x16
//
// The thunk at `#0xad4` is a real call target -- `qsort_r` invokes it for every
// comparison -- but it has no symbol, no FDE, and no PROLOGUE, because two
// instructions have no frame to push. The prologue test alone therefore rejects
// it, and nginx dies at the first `qsort` with the target "not in the lifted
// function table".
//
// The shape is specific enough to be safe on its own: an indirect branch within
// a few words, reached only through register moves. Data does not look like
// this, and the caller still requires the candidate to be a computed address
// preceded by a function terminator.
// The returned length is the trampoline's exact size in bytes, through the
// branch, or 0 if this is not one. The exact size matters as much as the
// recognition: a trampoline is SELF-DELIMITING, so unlike a prologue-started
// function it must not be sized by the distance to the next known boundary. The
// first version did that and gave `at_quick_exit`'s trampoline 184 bytes,
// swallowing the entire unnamed function that followed it -- a boundary in the
// middle of a real function, which is the exact failure this file's filter
// exists to avoid.
func trampolineLen(at uint64, wordAt func(uint64) (uint32, bool)) uint64 {
	for i := uint64(0); i < 4; i++ {
		w, ok := wordAt(at + i*4)
		if !ok {
			return 0
		}
		if w&0xfffffc1f == 0xd61f0000 { // br Xn
			return (i + 1) * 4
		}
		if !isMovReg(w) {
			return 0
		}
	}
	return 0
}

// passedToCall reports whether the value just materialised into register `rd` is
// handed to a call without ever being dereferenced.
//
// This is the discriminator for a FRAMELESS function pointer -- one whose entry
// is neither a prologue nor a trampoline, so neither test above recognises it.
// musl's `__setxid` is the case that forced it:
//
//	adrp x0, .        ; add x0, x0, #0xa00   -> do_setxid
//	bl   __synccall
//
// `do_setxid` is static, has no FDE, and begins `mov x3, x0` -- it pushes no
// frame, so `isPrologue` rejects it. nginx only reaches it when a master drops
// privileges for its workers, which is why single-process nginx never saw this
// and master mode dies at once with "vma 0x1469a00 not in the lifted function
// table".
//
// The rule is about USE, and it separates the two things an `adrp`/`add` in
// executable memory can be forming:
//
//   - a code pointer is PASSED -- to an argument register at a `bl`, or straight
//     into a `blr`
//   - a data pointer is DEREFERENCED -- a jump-table base is `ldrb [x2, w1]`, a
//     literal pool is `ldr [x3]`; both name the register as a memory base
//
// So: scan forward a few instructions; any read or write of `rd` before the call
// rejects, reaching the call accepts. The window is deliberately short. A value
// that survives eight instructions of unrelated work before being called is not
// something this scan should be guessing about.
func passedToCall(at uint64, rd uint32, wordAt func(uint64) (uint32, bool)) bool {
	for i := uint64(1); i <= 4; i++ {
		w, ok := wordAt(at + i*4)
		if !ok {
			return false
		}
		switch {
		case w&0xfc000000 == 0x94000000: // bl
			return rd <= 7 // an argument register at the call
		case w&0xfffffc1f == 0xd63f0000: // blr Xn
			return (w>>5)&0x1f == rd || rd <= 7
		case isTerminator(w):
			return false // left the block without calling
		case (w>>5)&0x1f == rd, w&0x1f == rd:
			return false // dereferenced, or overwritten, before any call
		}
	}
	return false
}

// is64BitStore reports whether `w` stores a 64-bit general register to memory,
// and returns the stored register(s) and the base register.
//
// Every encoding here was taken from an `aarch64-linux-gnu-as` assembly of the
// corresponding mnemonic and is pinned in TestStoreEncodings; a mask derived by
// hand would silently classify LDR as STR (they differ in one bit) and turn the
// rule below into "the value was loaded through", which is the exact opposite
// test.
func is64BitStore(w uint32) (rt, rt2, rn uint32, ok bool) {
	rt, rt2, rn = w&0x1f, 32, (w>>5)&0x1f
	switch {
	case w&0xffc00000 == 0xf9000000: // str  Xt, [Xn, #imm]     f9002a63
	case w&0xffe00c00 == 0xf8000000: // stur Xt, [Xn, #simm]    f81f8263
	case w&0xffe00c00 == 0xf8000c00: // str  Xt, [Xn, #imm]!    f8008e63
	case w&0xffe00c00 == 0xf8000400: // str  Xt, [Xn], #imm     f8008663
	case w&0xffc00000 == 0xa9000000, // stp  Xt,Xt2,[Xn,#imm]   a9011263
		w&0xffc00000 == 0xa9800000, // stp  Xt,Xt2,[Xn,#imm]!  a9bf1263
		w&0xffc00000 == 0xa8800000: // stp  Xt,Xt2,[Xn],#imm   a8811263
		rt2 = (w >> 10) & 0x1f
	default:
		return 0, 0, 0, false
	}
	return rt, rt2, rn, true
}

// storedAsPointer reports whether the value just materialised into `rd` is
// STORED to memory without ever being dereferenced.
//
// It is `passedToCall`'s sibling, and it exists for the same reason: a frameless
// function whose entry no prologue test can recognise. musl's `__stdio_seek` is
//
//	a53378:  ldr w0, [x0, #120]
//	a5337c:  b   lseek
//
// -- two instructions, no frame, no symbol (Alpine's libc is stripped), no FDE
// (musl ships 916 bytes of .eh_frame for the whole library), and no BL anywhere.
// It is not a trampoline either: `b` to a fixed label is not `br`. Its address is
// materialised in `__fdopen` and STORED into the FILE:
//
//	adrp x3, . ; add x3, x3, #0x378 ; str x3, [x19, #80]
//
// so `passedToCall` rejects it too -- the register is stored, not called. redis
// died on the first `fflush` of a seekable stream with "vma 0xa53378 not in the
// lifted function table".
//
// The discrimination is the same one passedToCall makes, on the same axis of
// USE: a code pointer is stored as a VALUE (it is the `Rt` of the store), a data
// pointer is dereferenced (it is the `Rn`). A store that names `rd` as its base
// register therefore rejects, exactly as a load would.
func storedAsPointer(at uint64, rd uint32, wordAt func(uint64) (uint32, bool)) bool {
	for i := uint64(1); i <= 4; i++ {
		w, ok := wordAt(at + i*4)
		if !ok {
			return false
		}
		if rt, rt2, rn, isStore := is64BitStore(w); isStore && rn != rd && (rt == rd || rt2 == rd) {
			return true
		}
		switch {
		case isTerminator(w):
			return false // left the block without storing
		case (w>>5)&0x1f == rd, w&0x1f == rd:
			return false // dereferenced as a base, or overwritten
		}
	}
	return false
}

// isPrologue recognises the instruction a function is overwhelmingly likely to
// start with on aarch64.
func isPrologue(w uint32) bool {
	switch {
	case w&0xffc003e0 == 0xa98003e0: // stp Rt,Rt2,[sp,#-imm]!  (Rn=31)
		return true
	case w&0xff8003ff == 0xd10003ff: // sub sp, sp, #imm
		return true
	case w == 0xd503233f, w == 0xd503245f: // paciasp, bti c
		return true
	}
	return false
}

// isTerminator recognises an instruction that ends a function, so the word
// after it can plausibly begin a new one.
func isTerminator(w uint32) bool {
	switch {
	case w == 0xd65f03c0: // ret
		return true
	case w&0xfc000000 == 0x14000000: // b (unconditional, includes tail calls)
		return true
	case w&0xfffffc1f == 0xd61f0000: // br Xn
		return true
	case w&0xffe0001f == 0xd4200000: // brk #imm
		return true
	}
	return false
}

// adrpTarget decodes the page address an ADRP forms at pc.
func adrpTarget(w uint32, pc uint64) uint64 {
	imm := int64((w>>5)&0x7ffff)<<2 | int64((w>>29)&3)
	if imm >= 1<<20 {
		imm -= 1 << 21
	}
	return (pc &^ 0xfff) + uint64(imm<<12)
}

// execRange is one executable span of an object, in link-time addresses.
type execRange struct {
	addr, size uint64
	data       []byte
}

func execRanges(o *Object) []execRange {
	var out []execRange
	for _, sec := range o.file.Sections {
		if sec.Type == elf.SHT_NOBITS || sec.Addr == 0 || sec.Flags&elf.SHF_EXECINSTR == 0 {
			continue
		}
		d, err := sec.Data()
		if err != nil {
			continue
		}
		out = append(out, execRange{addr: sec.Addr, size: sec.Size, data: d})
	}
	return out
}

func (e execRange) word(addr uint64) (uint32, bool) {
	if addr < e.addr || addr+4 > e.addr+uint64(len(e.data)) {
		return 0, false
	}
	return binary.LittleEndian.Uint32(e.data[addr-e.addr:]), true
}

// codePointerFuncs returns function starts recovered from computed code
// pointers, sized by the distance to the next known boundary. `ranges` are the
// object's executable spans and `known` every boundary already established for
// it, both in link-time addresses.
func codePointerFuncs(ranges []execRange, known []funcRange) []funcRange {
	if len(ranges) == 0 {
		return nil
	}
	wordAt := func(addr uint64) (uint32, bool) {
		for _, r := range ranges {
			if w, ok := r.word(addr); ok {
				return w, true
			}
		}
		return 0, false
	}

	sorted := append([]funcRange(nil), known...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].addr < sorted[j].addr })

	// addr -> exact size, or 0 when only the next boundary can bound it.
	found := map[uint64]uint64{}
	for _, r := range ranges {
		// Last ADRP seen per destination register. Any other write to a
		// register invalidates it, which keeps the pairing local and cheap.
		var page [32]uint64
		var live [32]bool
		for off := uint64(0); off+4 <= uint64(len(r.data)); off += 4 {
			w := binary.LittleEndian.Uint32(r.data[off:])
			pc := r.addr + off
			switch {
			case isADRP(w):
				rd := w & 0x1f
				page[rd], live[rd] = adrpTarget(w, pc), true
			case isADDImm(w):
				rn, rd := (w>>5)&0x1f, w&0x1f
				if live[rn] {
					v := page[rn] + uint64((w>>10)&0xfff)
					if ok, exact := acceptCodePointer(v, pc, rd, sorted, wordAt); ok {
						found[v] = exact
					}
				}
				if rd != rn {
					live[rd] = false
				}
			default:
				live[w&0x1f] = false
			}
		}
	}
	if len(found) == 0 {
		return nil
	}

	starts := make([]uint64, 0, len(found))
	for a := range found {
		starts = append(starts, a)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })

	// Size each recovered function by the distance to the next boundary of any
	// kind -- the next symbol, FDE or recovered start, or the end of its
	// section. That is the most a disassembler can honestly claim, and it never
	// overlaps something already known.
	out := make([]funcRange, 0, len(starts))
	for i, a := range starts {
		end := uint64(0)
		for _, r := range ranges {
			if a >= r.addr && a < r.addr+r.size {
				end = r.addr + r.size
				break
			}
		}
		if j := sort.Search(len(sorted), func(k int) bool { return sorted[k].addr > a }); j < len(sorted) && sorted[j].addr < end {
			end = sorted[j].addr
		}
		if i+1 < len(starts) && starts[i+1] < end {
			end = starts[i+1]
		}
		// A self-delimiting recovery knows its own extent; only take the
		// next-boundary bound if it is tighter (a boundary inside the
		// trampoline would mean we misread it).
		if exact := found[a]; exact != 0 && a+exact < end {
			end = a + exact
		}
		if end > a {
			out = append(out, funcRange{addr: a, size: end - a})
		}
	}
	return out
}

// acceptCodePointer applies the filter described at the top of this file. The
// second result is the recovered function's exact size when that is knowable
// (a trampoline), or 0 when only the next boundary can bound it.
// `addPC` and `rd` locate the `add` that formed the value, so the use-based test
// (passedToCall) can look at what happens to the register next.
func acceptCodePointer(v, addPC uint64, rd uint32, sorted []funcRange,
	wordAt func(uint64) (uint32, bool)) (bool, uint64) {
	w, ok := wordAt(v)
	if !ok {
		return false, 0 // not in an executable section
	}
	if covered(sorted, v) {
		return false, 0 // already inside a known function
	}
	if i := sort.Search(len(sorted), func(k int) bool { return sorted[k].addr >= v }); i < len(sorted) && sorted[i].addr == v {
		return false, 0 // already a known start
	}
	exact := trampolineLen(v, wordAt)
	if !isPrologue(w) && exact == 0 &&
		!passedToCall(addPC, rd, wordAt) && !storedAsPointer(addPC, rd, wordAt) {
		return false, 0
	}
	if prev, ok := wordAt(v - 4); !ok || !isTerminator(prev) {
		return false, 0
	}
	return true, exact
}
