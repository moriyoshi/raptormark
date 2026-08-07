package fuse

import (
	"debug/elf"
	"encoding/binary"
	"sort"
)

// The ecvisor bring-up tables. `.ecv.init` and `.ecv.dlsyms` apply to any libc;
// `.ecv.early` and `.ecv.stacklists` describe glibc internals and are simply
// absent for a musl closure.
//
// A real dynamic program is not ready to run once its relocations are applied.
// ld.so also calls `__libc_early_init`, initialises `_rtld_global`'s thread
// bookkeeping, and runs every object's constructors -- and the fused image
// enters at the executable's `_start`, bypassing all of it. The runtime
// performs those steps itself (apply_early_init, apply_stacklists,
// apply_init_array in runtime/src/context.rs) from tables the prelinker leaves
// in allocatable sections.
//
// Each is a no-op when its inputs are absent, so a musl or static closure
// simply gets no section.
type bringupTables struct {
	early      []byte // .ecv.early:      __libc_early_init's VMA
	initArray  []byte // .ecv.init:       constructor VMAs, in loader order
	stacklists []byte // .ecv.stacklists: _rtld_global thread-list geometry
	dlsyms     []byte // .ecv.dlsyms:     export table for the intercepted dlsym
	muslTP     []byte // .ecv.musltp:     musl thread-pointer layout (tid seed)
}

func buildTables(objs []*Object, syms map[string]uint64, ifuncSyms map[string]bool, resolved map[uint64]uint64) bringupTables {
	return bringupTables{
		early:      earlyDescriptor(syms),
		initArray:  initDescriptor(objs),
		stacklists: stacklistsDescriptor(objs, syms),
		dlsyms:     dlsymsDescriptor(syms, ifuncSyms, resolved),
		muslTP:     muslTPDescriptor(objs, syms),
	}
}

// earlyDescriptor names `__libc_early_init`, which ld.so invokes for libc
// before any constructor (`_dl_call_libc_early_init`). It runs `__ctype_init`,
// which populates the main thread's ctype TSD pointers; without it every
// `isalpha`/`tolower` in the guest reads through a null locale table.
func earlyDescriptor(syms map[string]uint64) []byte {
	vma, ok := syms["__libc_early_init"]
	if !ok || vma == 0 {
		return nil
	}
	return binary.LittleEndian.AppendUint64(nil, vma)
}

// initDescriptor collects constructor VMAs in the order `_dl_init` would call
// them: the executable's DT_PREINIT_ARRAY first, then each object's DT_INIT
// followed by its DT_INIT_ARRAY, dependencies before dependents, with the
// executable last.
//
// `objs` is [executable, then the DT_NEEDED closure breadth-first], so walking
// the libraries in reverse puts the deepest dependencies first -- the same
// ordering property ld.so's sort guarantees, arrived at more cheaply. The
// interpreter is skipped: it is fused so its symbols resolve, never entered.
//
// The array contents are read AFTER relocation, so the pointers they hold are
// already fused VMAs rather than link-time ones.
func initDescriptor(objs []*Object) []byte {
	var out []byte
	add := func(vma uint64) {
		// glibc treats 0 and -1 as empty slots.
		if vma == 0 || vma == ^uint64(0) {
			return
		}
		out = binary.LittleEndian.AppendUint64(out, vma)
	}
	exe := objs[0]
	for _, v := range dynArray(objs, exe, elf.DT_PREINIT_ARRAY, elf.DT_PREINIT_ARRAYSZ) {
		add(v)
	}
	ctorsOf := func(o *Object) {
		if o.isInterp {
			return
		}
		if v, ok := dynFirst(o, elf.DT_INIT); ok {
			add(o.addr(v))
		}
		for _, v := range dynArray(objs, o, elf.DT_INIT_ARRAY, elf.DT_INIT_ARRAYSZ) {
			add(v)
		}
	}
	for i := len(objs) - 1; i >= 1; i-- {
		ctorsOf(objs[i])
	}
	ctorsOf(exe)
	return out
}

// dynFirst reads a single-valued dynamic tag.
func dynFirst(o *Object, tag elf.DynTag) (uint64, bool) {
	vals, err := o.file.DynValue(tag)
	if err != nil || len(vals) == 0 {
		return 0, false
	}
	return vals[0], true
}

// dynArray reads a DT_*_ARRAY / DT_*_ARRAYSZ pair out of the relocated image
// and returns the pointers it holds.
func dynArray(objs []*Object, o *Object, arrayTag, sizeTag elf.DynTag) []uint64 {
	base, ok := dynFirst(o, arrayTag)
	if !ok || base == 0 {
		return nil
	}
	size, ok := dynFirst(o, sizeTag)
	if !ok {
		return nil
	}
	var out []uint64
	for off := uint64(0); off+8 <= size; off += 8 {
		v, err := readMem(objs, o.addr(base+off))
		if err != nil {
			// A truncated array is not worth failing the fuse over; the
			// constructors that were found still run.
			break
		}
		out = append(out, v)
	}
	return out
}

// stacklistsDescriptor describes `_rtld_global`'s thread bookkeeping so the
// runtime can perform the part of ld.so's pthread bring-up
// (`__tls_init_tp`) that is not reachable as an exported function.
//
// Nine words, matching apply_stacklists:
//
//	0  _rtld_global's VMA
//	1  offsetof _dl_stack_used     within _rtld_global
//	2  offsetof _dl_stack_user
//	3  offsetof _dl_stack_cache
//	4  sizeof(struct pthread)
//	5  offsetof(struct pthread, list)
//	6  rtld recursive-lock fn-ptr slot   (0 = not supplied)
//	7  ld.so's no-op lock function        (0 = not supplied)
//	8  offsetof(struct pthread, tid)
//
// Words 6 and 7 are left zero: they need `struct rtld_global`'s lock members,
// which thread_db does not describe, and the runtime already guards on both
// being non-zero. Everything else comes from glibc's own `_thread_db_*`
// descriptors, so it tracks the library's real layout rather than a
// version-specific guess. Word 8 is the one OpenSSL depends on -- with tid left
// zero, `pthread_rwlock_rdlock` sees `__cur_writer == tid == 0` and returns
// EDEADLK on every fresh read lock.
func stacklistsDescriptor(objs []*Object, syms map[string]uint64) []byte {
	rtld, ok := syms["_rtld_global"]
	if !ok || rtld == 0 {
		return nil
	}
	used, ok1 := threadDBOffset(objs, syms, "_thread_db_rtld_global__dl_stack_used")
	user, ok2 := threadDBOffset(objs, syms, "_thread_db_rtld_global__dl_stack_user")
	szPthread, ok3 := threadDBValue(objs, syms, "_thread_db_sizeof_pthread")
	listOff, ok4 := threadDBOffset(objs, syms, "_thread_db_pthread_list")
	tidOff, ok5 := threadDBOffset(objs, syms, "_thread_db_pthread_tid")
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
		return nil
	}
	// _dl_stack_cache follows _dl_stack_user, and thread_db describes neither
	// its offset nor the struct. The three are consecutive `list_t`s, so the
	// stride between the two it does describe gives the third -- but only if
	// that stride really is one list_t (two pointers). If glibc ever reorders
	// them the assumption is wrong in a way that would corrupt memory, so
	// refuse rather than guess.
	stride := user - used
	if stride != 16 {
		return nil
	}
	cache := user + stride

	out := make([]byte, 0, 80)
	for _, w := range []uint64{rtld, used, user, cache, szPthread, listOff, 0, 0, tidOff} {
		out = binary.LittleEndian.AppendUint64(out, w)
	}
	// Word 9, added later and optional: the fused address of
	// `_rtld_global_ro._dl_minsigstacksize`, or 0 when it cannot be derived.
	// It rides in this section because both are "fields ld.so would have
	// written", not because it needs thread_db; a glibc that stopped shipping
	// thread_db descriptors would lose this too, which is a trade for keeping
	// one glibc bring-up table rather than two.
	out = binary.LittleEndian.AppendUint64(out, minsigstacksizeVMA(objs, syms))
	// Words 10 and 11, optional like word 9: the fused addresses of
	// `_rtld_global_ro._dl_tls_static_size` and `._dl_tls_static_align`. Zero
	// when they cannot be decoded, which the runtime reads as "leave them
	// alone"; a shorter section behaves the same way.
	tlsSize, tlsAlign := glroTLSStaticVMAs(objs, syms)
	out = binary.LittleEndian.AppendUint64(out, tlsSize)
	out = binary.LittleEndian.AppendUint64(out, tlsAlign)
	// Words 12..15: ld.so's hook installers, to be CALLED at bring-up rather
	// than read, zero-padded. There is more than one -- the malloc family and
	// the lock pair have separate installers -- and a missing one is not a
	// degraded mode: the first indirect call through an uninstalled slot is
	// fatal.
	inits := rtldHookInitVMAs(objs, syms)
	for i := 0; i < maxRtldHookInits; i++ {
		var v uint64
		if i < len(inits) {
			v = inits[i]
		}
		out = binary.LittleEndian.AppendUint64(out, v)
	}
	return out
}

// minsigstacksizeVMA locates `_rtld_global_ro._dl_minsigstacksize`, which
// `sysconf(_SC_MINSIGSTKSZ)` asserts is non-zero before returning it.
//
// ld.so sets it from `AT_MINSIGSTKSZ`, with a fallback to the architecture's
// `MINSIGSTKSZ` if the auxv did not carry one. A fused image enters the
// executable's `_start` and never runs ld.so, so the field stays zero and the
// assert aborts. python:3-slim dies there, after its ifuncs resolve and all
// twelve of its constructors run.
//
// ❌ Supplying `AT_MINSIGSTKSZ` in the initial stack does NOT fix it, and this
// was tried: `_dl_aux_init` runs only on glibc's static path, and the shared
// path parses the auxv in `dl_main` -- which is ld.so, which is what we skip.
// The field has to be written directly.
//
// The offset is DERIVED, twice, because a wrong one writes 5120 into some other
// member of `_rtld_global_ro`:
//
//   - `__getpagesize` is `assert (GLRO(dl_pagesize) != 0); return
//     GLRO(dl_pagesize);` and compiles to exactly the four instructions
//     `glroAsserts` matches. It gives `offsetof(rtld_global_ro, _dl_pagesize)`
//     and, through the GOT slot it loads, an independent reading of
//     `&_rtld_global_ro` that must equal the symbol's own address.
//   - `__sysconf` contains the same four-instruction shape once per
//     `GLRO(dl_minsigstacksize)` assert -- three times in Debian's glibc 2.36,
//     for `_SC_MINSIGSTKSZ` and the two halves of `_SC_SIGSTKSZ` -- and every
//     one of them must name the same offset.
//
// The two must then agree that the field sits immediately after `_dl_pagesize`.
// That is the same shape of argument as `_dl_stack_cache` above: an adjacency,
// used only to confirm an offset that was read out of the code rather than to
// stand in for one.
func minsigstacksizeVMA(objs []*Object, syms map[string]uint64) uint64 {
	rtldRO := syms["_rtld_global_ro"]
	if rtldRO == 0 {
		return 0
	}
	word := func(at uint64) (uint32, bool) {
		w, err := readInsn(objs, at)
		return w, err == nil
	}
	// Reading one: the page size accessor, which must be the ONLY such shape in
	// its own body.
	pg := glroAsserts(objs, syms, "__getpagesize", word)
	if len(pg) != 1 {
		return 0
	}
	gotSlot, pageOff := pg[0].got, pg[0].field
	// The slot really has to hold `&_rtld_global_ro`. Without this the decode
	// could be reading any other global's accessor and reporting an offset into
	// the wrong struct.
	if base, err := readMem(objs, gotSlot); err != nil || base != rtldRO {
		return 0
	}

	// Reading two: every asserted GLRO field in sysconf that is not the page
	// size. They must agree with each other, and there must be at least one.
	var minsigOff uint64
	for _, a := range glroAsserts(objs, syms, "__sysconf", word) {
		if a.got != gotSlot || a.field == pageOff {
			continue
		}
		if minsigOff != 0 && minsigOff != a.field {
			return 0 // sysconf asserts on two different fields; cannot tell them apart
		}
		minsigOff = a.field
	}
	if minsigOff == 0 || minsigOff != pageOff+8 {
		return 0
	}
	return rtldRO + minsigOff
}

// decodeLdp64 matches `ldp Xt1, Xt2, [Xn, #imm7]` (64-bit, signed offset,
// scaled by 8) with Xn == rn, returning the byte offset of the FIRST member.
func decodeLdp64(w uint32, rn uint32) (uint64, bool) {
	if w&0xffc00000 != 0xa9400000 || (w>>5)&0x1f != rn {
		return 0, false
	}
	imm := int64((w >> 15) & 0x7f)
	if imm&0x40 != 0 {
		imm -= 0x80 // sign-extend imm7
	}
	if imm < 0 {
		return 0, false
	}
	return uint64(imm) * 8, true
}

// glroTLSStaticVMAs locates `_rtld_global_ro._dl_tls_static_size` and
// `._dl_tls_static_align`, which ld.so sets and a fused image leaves zero.
//
// Why they matter: glibc's `allocate_stack` computes
// `size &= ~(GLRO(dl_tls_static_align) - 1)`, so with the align unset that mask
// is all-ones and EVERY stack size becomes zero -- `Fatal glibc error:
// allocatestack.c:335 (allocate_stack): assertion failed: size != 0`. A fused
// dynamic glibc image therefore cannot create a thread at all. Measured
// 2026-08-15, and localised by giving the guest an EXPLICIT stacksize, which
// changed nothing: the fault is downstream of every attr.
//
// The offsets come from `__pthread_get_minstack`, whose whole body is
// `roundup(GLRO(dl_tls_static_size), GLRO(dl_tls_static_align)) +
// GLRO(dl_pagesize) + PTHREAD_STACK_MIN`:
//
//	adrp x0, <page>
//	ldr  x0, [x0, #3712]      ; the GOT slot holding &_rtld_global_ro
//	ldp  x1, x2, [x0, #472]   ; _dl_tls_static_size, _dl_tls_static_align
//	ldr  x0, [x0, #24]        ; _dl_pagesize
//	add  x0, x0, #0x20, lsl #12
//	add  x1, x2, x1 ; sub x1, x1, #1 ; udiv x1, x1, x2 ; madd x0, x1, x2, x0
//
// Three things must line up or nothing is emitted: the GOT slot must really
// hold `&_rtld_global_ro`; the function must contain exactly ONE such `ldp`,
// since a second would make "which pair is the TLS one" a guess; and the plain
// `ldr` off the same base must name the offset `__getpagesize` independently
// gives for `_dl_pagesize`. That last one is the cross-check -- it ties this
// decode to a field whose offset was already derived from different code.
func glroTLSStaticVMAs(objs []*Object, syms map[string]uint64) (uint64, uint64) {
	rtldRO := syms["_rtld_global_ro"]
	at := syms["__pthread_get_minstack"]
	if rtldRO == 0 || at == 0 {
		return 0, 0
	}
	word := func(a uint64) (uint32, bool) {
		w, err := readInsn(objs, a)
		return w, err == nil
	}
	pg := glroAsserts(objs, syms, "__getpagesize", word)
	if len(pg) != 1 {
		return 0, 0
	}
	pageOff := pg[0].field

	const window = 24
	var adrpReg uint32
	var adrpPage uint64
	var haveADRP bool
	glro := map[uint32]bool{}
	var pairOff uint64
	var pairs, pageReads int
	for i := uint64(0); i < window; i++ {
		w, ok := word(at + i*4)
		if !ok {
			break
		}
		if !haveADRP {
			if p, rd, ok := decodeADRP(w, at+i*4); ok {
				adrpReg, adrpPage, haveADRP = rd, p, true
			}
			continue
		}
		// `ldr Xd, [adrp_base, #off]` where the slot holds &_rtld_global_ro.
		if off, ok := decodeLdr64Unsigned(w, adrpReg); ok {
			if v, err := readMem(objs, adrpPage+off); err == nil && v == rtldRO {
				glro[w&0x1f] = true
				continue
			}
		}
		for rn := range glro {
			if off, ok := decodeLdp64(w, rn); ok {
				pairs++
				pairOff = off
			}
			if off, ok := decodeLdr64Unsigned(w, rn); ok && off == pageOff {
				pageReads++
			}
		}
	}
	if pairs != 1 || pageReads == 0 || pairOff == 0 {
		return 0, 0
	}
	return rtldRO + pairOff, rtldRO + pairOff + 8
}

// glroAssert is one `assert (GLRO(field) != 0)` site: the GOT slot holding
// `&_rtld_global_ro` and the byte offset of the field tested.
type glroAssert struct{ got, field uint64 }

// glroAsserts finds every `assert (GLRO(x) != 0)` in the function named `sym`.
// gcc compiles each to four consecutive instructions, register-chained:
//
//	adrp Xd, <page>            ; the GOT page for _rtld_global_ro
//	ldr  Xe, [Xd, #<disp>]     ; Xe = &_rtld_global_ro
//	ldr  Xf, [Xe, #<offset>]   ; the field
//	cbz/cbnz Xf, ...           ; the assert, either polarity
//
// Requiring all four to be adjacent AND the registers to chain is what keeps
// this from matching the dozens of other GOT loads in a function the size of
// `__sysconf`. The scan is bounded by the symbol's own size, so it cannot run
// off into a neighbouring function and report its offsets.
func glroAsserts(objs []*Object, syms map[string]uint64, sym string, word func(uint64) (uint32, bool)) []glroAssert {
	vma := syms[sym]
	size, ok := symbolSize(objs, sym)
	if vma == 0 || !ok || size < 16 {
		return nil
	}
	var out []glroAssert
	for at := vma; at+16 <= vma+size; at += 4 {
		w0, ok0 := word(at)
		w1, ok1 := word(at + 4)
		w2, ok2 := word(at + 8)
		w3, ok3 := word(at + 12)
		if !ok0 || !ok1 || !ok2 || !ok3 {
			continue
		}
		page, rd, ok := decodeADRP(w0, at)
		if !ok {
			continue
		}
		rt1, rn1, disp, ok := decodeLdr64(w1)
		if !ok || rn1 != rd {
			continue
		}
		rt2, rn2, off, ok := decodeLdr64(w2)
		if !ok || rn2 != rt1 {
			continue
		}
		if rt, ok := decodeCbzCbnz64(w3); !ok || rt != rt2 {
			continue
		}
		out = append(out, glroAssert{got: page + disp, field: off})
	}
	return out
}

// decodeLdr64 matches `ldr Xt, [Xn, #imm12]`, returning Xt, Xn and the BYTE
// displacement -- the encoded immediate is scaled by 8 for a 64-bit load.
func decodeLdr64(w uint32) (rt, rn uint32, disp uint64, ok bool) {
	if w&0xffc00000 != 0xf9400000 {
		return 0, 0, 0, false
	}
	return w & 0x1f, (w >> 5) & 0x1f, uint64((w>>10)&0xfff) * 8, true
}

// decodeCbzCbnz64 matches `cbz Xt, .` or `cbnz Xt, .`. Both polarities occur:
// gcc emits `cbz` where the assert falls through to the failure and `cbnz`
// where the success path is the branch.
func decodeCbzCbnz64(w uint32) (rt uint32, ok bool) {
	if w&0xff000000 != 0xb4000000 && w&0xff000000 != 0xb5000000 {
		return 0, false
	}
	return w & 0x1f, true
}

// symbolSize returns the size a dynamic symbol declares. Used to bound a code
// scan: an unbounded one walks into the next function and reports its offsets
// as if they belonged here.
func symbolSize(objs []*Object, name string) (uint64, bool) {
	for _, o := range objs {
		for _, s := range o.symbols {
			if s.Name == name && s.Section != elf.SHN_UNDEF && s.Size != 0 {
				return s.Size, true
			}
		}
	}
	return 0, false
}

// threadDBOffset reads the byte offset out of a glibc `_thread_db_*` field
// descriptor. Each is three uint32s -- the field's width in BITS, its element
// count, and its byte offset -- so the offset is the third word, not the first.
func threadDBOffset(objs []*Object, syms map[string]uint64, name string) (uint64, bool) {
	vma, ok := syms[name]
	if !ok || vma == 0 {
		return 0, false
	}
	v, err := readMem(objs, vma+8)
	if err != nil {
		return 0, false
	}
	return v & 0xffffffff, true
}

// threadDBValue reads a scalar `_thread_db_sizeof_*` descriptor, which is a
// single uint32 rather than a field triple.
func threadDBValue(objs []*Object, syms map[string]uint64, name string) (uint64, bool) {
	vma, ok := syms[name]
	if !ok || vma == 0 {
		return 0, false
	}
	v, err := readMem(objs, vma)
	if err != nil {
		return 0, false
	}
	if n := v & 0xffffffff; n != 0 {
		return n, true
	}
	return 0, false
}

// initStubs reports DT_INIT and DT_FINI as function ranges.
//
// These are the crti.o `_init`/`_fini` stubs in the `.init`/`.fini` sections.
// They carry no `.eh_frame` FDE and are usually not in the symbol table either,
// so neither of the other two boundary sources finds them -- yet `.ecv.init`
// names DT_INIT as a constructor, and the runtime FATALs on a constructor that
// is not a lifted function. The containing section bounds the stub exactly:
// `.init` holds nothing but `_init`.
func initStubs(o *Object) []funcRange {
	var out []funcRange
	for _, tag := range []elf.DynTag{elf.DT_INIT, elf.DT_FINI} {
		v, ok := dynFirst(o, tag)
		if !ok || v == 0 {
			continue
		}
		for _, sec := range o.file.Sections {
			// Only a DEDICATED .init/.fini bounds the stub. glibc has one, and
			// it holds nothing but `_init`, so the bound is exact. musl has
			// neither: its DT_INIT points into a 321 KB `.text`, and bounding
			// by that would declare the rest of the library one giant function
			// -- which suppresses real boundaries inside it and would tell
			// elflift to disassemble the lot as a single span. musl exports
			// `_init` as a sized symbol anyway, so nothing is lost by skipping.
			if sec.Name != ".init" && sec.Name != ".fini" {
				continue
			}
			if sec.Type == elf.SHT_NOBITS || sec.Addr == 0 {
				continue
			}
			if v >= sec.Addr && v < sec.Addr+sec.Size {
				out = append(out, funcRange{addr: v, size: sec.Addr + sec.Size - v})
				break
			}
		}
	}
	return out
}

// Pseudo-syscall numbers the runtime traps for the intercepted dl* API. Must
// match NR_ECV_DL* in runtime/src/sys.rs.
const (
	nrDlopen  = 0xF00
	nrDlsym   = 0xF01
	nrDlclose = 0xF02
	nrDlerror = 0xF03
)

// dlStubLen is the size of the three-instruction stub below.
const dlStubLen = 12

// dlStub assembles `movz x8, #nr; svc #0; ret`.
//
// MOVZ Xd,#imm16 is 0xd2800000 | imm16<<5 | Rd, so with x8 and no shift the
// word is 0xd2800008 | nr<<5.
func dlStub(nr uint32) []byte {
	b := make([]byte, dlStubLen)
	binary.LittleEndian.PutUint32(b[0:], 0xd2800008|nr<<5)
	binary.LittleEndian.PutUint32(b[4:], 0xd4000001) // svc #0
	binary.LittleEndian.PutUint32(b[8:], 0xd65f03c0) // ret
	return b
}

// patchDLStubs overwrites dlopen/dlsym/dlclose/dlerror with pseudo-syscall
// stubs so they trap to the runtime instead of running glibc's real loader.
//
// The fused image has no ld.so link-map, so the genuine dlopen cannot work; the
// runtime answers these itself (dlopen returns a sentinel handle, dlsym resolves
// against `.ecv.dlsyms`, dlclose/dlerror no-op). A name that is absent, or whose
// definition is too small to hold the stub, is left alone -- there is nothing
// safe to write and the guest simply keeps the original behaviour.
//
// Returns the names patched, for diagnostics.
func patchDLStubs(objs []*Object) []string {
	var done []string
	for _, e := range []struct {
		name string
		nr   uint32
	}{
		{"dlopen", nrDlopen},
		{"dlsym", nrDlsym},
		{"dlclose", nrDlclose},
		{"dlerror", nrDlerror},
	} {
		o, sym, ok := findDefinition(objs, e.name)
		if !ok || sym.Size < dlStubLen {
			continue
		}
		if err := o.writeBytes(sym.Value, dlStub(e.nr)); err != nil {
			continue
		}
		done = append(done, e.name)
	}
	return done
}

// findDefinition locates the first object defining name, matching the
// first-definition-wins order globalSymbols uses.
func findDefinition(objs []*Object, name string) (*Object, elf.Symbol, bool) {
	for _, o := range objs {
		for _, s := range o.symbols {
			if s.Name == name && s.Section != elf.SHN_UNDEF {
				return o, s, true
			}
		}
	}
	return nil, elf.Symbol{}, false
}

// dlsymsDescriptor builds the export table the runtime's dlsym resolves
// against. Layout, matching dlsym_lookup in runtime/src/context.rs:
//
//	[count u32][pad u32]
//	count * ([vma u64][name offset u32][pad u32])
//	a blob of NUL-terminated names
//
// The name offset is from the START OF THE SECTION, not from the blob, because
// that is what the reader indexes with. `syms` is already
// first-definition-wins in load order, which is the scope dlsym(RTLD_DEFAULT)
// searches. Names are sorted so the image is reproducible.
func dlsymsDescriptor(syms map[string]uint64, ifuncSyms map[string]bool, resolved map[uint64]uint64) []byte {
	if len(syms) == 0 {
		return nil
	}
	names := make([]string, 0, len(syms))
	for n := range syms {
		// An ifunc whose resolver could not be evaluated at build time has no
		// implementation address to record. Its GOT slots are fixed up at load
		// time from `.ecv.irela`, but this table is static, so the only two
		// options are the resolver or nothing. Recording the resolver is the
		// worse one: calling it returns a pointer and does nothing, which is
		// precisely the silent no-op that made `memset` corrupt OpenSSL's RCU.
		// Omitting the name makes dlsym return NULL, which callers check.
		if ifuncSyms[n] {
			if _, ok := resolved[syms[n]]; !ok {
				continue
			}
		}
		names = append(names, n)
	}
	sort.Strings(names)

	blobStart := 8 + 16*len(names)
	out := make([]byte, 0, blobStart)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(names)))
	out = binary.LittleEndian.AppendUint32(out, 0)
	var blob []byte
	for _, n := range names {
		vma := syms[n]
		// An STT_GNU_IFUNC symbol's value is its resolver; real dlsym returns
		// the implementation, so substitute the one relocation worked out.
		if impl, ok := resolved[vma]; ok && ifuncSyms[n] {
			vma = impl
		}
		out = binary.LittleEndian.AppendUint64(out, vma)
		out = binary.LittleEndian.AppendUint32(out, uint32(blobStart+len(blob)))
		out = binary.LittleEndian.AppendUint32(out, 0)
		blob = append(blob, n...)
		blob = append(blob, 0)
	}
	return append(out, blob...)
}

// muslTPDescriptor derives musl's thread-pointer layout from two exported
// accessors, and is the musl answer to glibc's `_thread_db_*` descriptors.
//
// musl publishes no metadata about `struct pthread`, but it does export
// accessors whose entire body IS the offset. On aarch64 both are three
// instructions:
//
//	gettid:       mrs x0, TPIDR_EL0 ; ldur w0, [x0, #-0xa8] ; ret
//	pthread_self: mrs x0, TPIDR_EL0 ; sub  x0, x0, #0xc8    ; ret
//
// giving tid at TP-0xa8 and the struct base at TP-0xc8, so
// `offsetof(pthread, tid)` is 32 -- which is exactly musl's aarch64 layout
// (`self, prev, next, sysinfo, tid`, with `dtv`/`canary` excluded under
// TLS_ABOVE_TP). The two cross-check each other.
//
// Why this is needed: for a dynamic musl program the loader runs `__init_tp`,
// which issues `set_tid_address` and stores the result in `pthread->tid`. The
// fused image enters at the executable's `_start` and never runs ld.so, so the
// tid stays zero -- and musl's `exit` reads it, sees zero, and branches to a
// deliberate crash path. This is the same defect as glibc's `.ecv.stacklists`
// word 8, in a libc with no thread_db to ask.
//
// Two words: the TP-relative distances (to be SUBTRACTED from the thread
// pointer) of the struct base and of the tid. Nothing is emitted unless both
// accessors decode exactly, which is what keeps this from firing on glibc --
// glibc exports both names too, but neither is this instruction sequence.
func muslTPDescriptor(objs []*Object, syms map[string]uint64) []byte {
	tidOff, ok := tpAccessorOffset(objs, syms["gettid"], decodeLdurNeg)
	if !ok {
		return nil
	}
	baseOff, ok := tpAccessorOffset(objs, syms["pthread_self"], decodeSubImm)
	if !ok {
		return nil
	}
	// tid must lie inside the struct, i.e. nearer the thread pointer than the
	// base. If it does not, the decode matched something that is not this
	// layout and guessing further would corrupt guest memory.
	if tidOff == 0 || baseOff == 0 || tidOff >= baseOff {
		return nil
	}
	out := binary.LittleEndian.AppendUint64(nil, baseOff)
	out = binary.LittleEndian.AppendUint64(out, tidOff)
	// Third word, added later and optional: the fused VMA of
	// `libc.can_do_threads`. Zero when it cannot be decoded, which the runtime
	// reads as "leave it alone" -- an older section with only two words behaves
	// identically.
	libc := muslCanDoThreadsVMA(objs, syms)
	out = binary.LittleEndian.AppendUint64(out, libc)
	// Fourth word: `offsetof(struct __libc, tls_size)`, confirmed against
	// pthread_create's own loads. It is what licenses the runtime to seed the
	// three neighbouring TLS fields; zero means "do not", and the runtime
	// treats a shorter section the same way.
	var tlsOff uint64
	if libc != 0 && syms["pthread_create"] != 0 {
		tlsOff = muslLibcTLSOffset(syms["pthread_create"], libc, func(at uint64) (uint32, bool) {
			w, err := readInsn(objs, at)
			return w, err == nil
		})
	}
	return binary.LittleEndian.AppendUint64(out, tlsOff)
}

// muslCanDoThreadsVMA locates musl's `libc.can_do_threads` by decoding the
// prologue of `pthread_create`, which is the only exported function whose FIRST
// action is to read that byte:
//
//	pthread_create:
//	   ...
//	   adrp x19, <page>              ; &__libc
//	   ldrb w4, [x19, #632]          ; libc.can_do_threads
//	   cbz  w4, <return ENOSYS>
//	   add  x0, x19, #0x278          ; &__libc again, for libc.threaded at +1
//
// Why it is needed: `can_do_threads` is set by `__init_tp`, which for a dynamic
// musl program runs inside ld.so. The fused image enters the executable's
// `_start` and never runs ld.so, so the byte stays zero and every
// `pthread_create` in the guest returns ENOSYS before issuing a single syscall.
// That is what stopped redis:7-alpine, and it looks nothing like a thread
// problem from the outside: redis prints `strerror(errno)` rather than the
// return value, so the message names whatever errno happened to be left over.
//
// It is the same class of gap as the tid seed above -- a field ld.so would have
// written -- and it is derived the same way, from an exported accessor's code
// rather than from a struct layout musl does not publish.
//
// Two independent readings must agree: the `ldrb` displacement and the `add`
// immediate off the same base register are both `offsetof(__libc,
// can_do_threads)` plus the page offset of `__libc`. gcc emits both because the
// function needs `&__libc` again for `libc.threaded`. If only one is present, or
// they disagree, nothing is emitted -- a wrong address here writes 1 into an
// arbitrary byte of guest data.
func muslCanDoThreadsVMA(objs []*Object, syms map[string]uint64) uint64 {
	vma := syms["pthread_create"]
	if vma == 0 {
		return 0
	}
	addr, ok := decodeMuslLibcFlag(vma, func(at uint64) (uint32, bool) {
		w, err := readInsn(objs, at)
		return w, err == nil
	})
	if !ok {
		return 0
	}
	// The byte has to be writable data that is still zero. A hit in .text, or
	// one that is already non-zero, means the decode matched something else.
	o := objectAt(objs, addr)
	if o == nil || !isWritableData(o, addr) {
		return 0
	}
	if b, ok := byteAt(o, addr); !ok || b != 0 {
		return 0
	}
	return addr
}

// decodeMuslLibcFlag reads `pthread_create`'s prologue at `at` and returns the
// address of `libc.can_do_threads`. `word` reads one instruction.
//
// The read is in the prologue but not at a fixed instruction: the register
// allocator interleaves it with the frame setup and the attribute clearing, so
// this scans a window rather than matching a fixed sequence. Both references off
// the adrp base must be found and must agree.
func decodeMuslLibcFlag(at uint64, word func(uint64) (uint32, bool)) (uint64, bool) {
	const window = 24
	var base uint32
	var page uint64
	var haveADRP bool
	var fromLdrb, fromAdd uint64
	for i := uint64(0); i < window; i++ {
		w, ok := word(at + i*4)
		if !ok {
			return 0, false
		}
		if !haveADRP {
			if p, rd, ok := decodeADRP(w, at+i*4); ok {
				base, page, haveADRP = rd, p, true
			}
			continue
		}
		if imm, ok := decodeLdrbUnsigned(w, base); ok && fromLdrb == 0 {
			fromLdrb = page + imm
		}
		if imm, ok := decodeAddImm(w, base); ok && fromAdd == 0 {
			fromAdd = page + imm
		}
	}
	if fromLdrb == 0 || fromAdd == 0 || fromLdrb != fromAdd {
		return 0, false
	}
	return fromLdrb, true
}

// decodeADRP matches `adrp Xd, <page>` and returns the page address and Xd.
func decodeADRP(w uint32, at uint64) (page uint64, rd uint32, ok bool) {
	if w&0x9f000000 != 0x90000000 {
		return 0, 0, false
	}
	imm := int64((w>>5)&0x7ffff)<<2 | int64((w>>29)&3)
	if imm&(1<<20) != 0 {
		imm -= 1 << 21 // sign-extend the 21-bit page displacement
	}
	return uint64(int64(at&^0xfff) + imm<<12), w & 0x1f, true
}

// decodeLdrbUnsigned matches `ldrb Wt, [Xn, #imm12]` with Xn == rn.
func decodeLdrbUnsigned(w uint32, rn uint32) (uint64, bool) {
	if w&0xffc00000 != 0x39400000 || (w>>5)&0x1f != rn {
		return 0, false
	}
	return uint64((w >> 10) & 0xfff), true // byte access: the immediate is unscaled
}

// decodeAddImm matches `add Xd, Xn, #imm12` (no shift) with Xn == rn.
func decodeAddImm(w uint32, rn uint32) (uint64, bool) {
	if w&0xff800000 != 0x91000000 || (w>>5)&0x1f != rn {
		return 0, false
	}
	return uint64((w >> 10) & 0xfff), true
}

// decodeLdr64Unsigned matches `ldr Xt, [Xn, #imm12]` -- 64-bit, unsigned offset,
// so the immediate is scaled by 8 -- with Xn == rn.
func decodeLdr64Unsigned(w uint32, rn uint32) (uint64, bool) {
	if w&0xffc00000 != 0xf9400000 || (w>>5)&0x1f != rn {
		return 0, false
	}
	return uint64((w>>10)&0xfff) * 8, true
}

// muslLibcTLSOffset returns `offsetof(struct __libc, tls_size)` as decoded from
// `pthread_create`, or 0 when it cannot be confirmed.
//
// WHY THIS IS NEEDED AT ALL, and why the runtime cannot check it instead: the
// runtime seeds `libc.tls_head`/`tls_size`/`tls_align`/`tls_cnt` at bring-up,
// before the guest's first instruction. At that moment every field of
// `struct __libc` is still zero -- `__init_libc` runs inside the guest's own
// `_start` -- so there is nothing live to cross-check a struct layout against.
// The first attempt checked `libc.page_size` at run time and always refused,
// correctly: `__libc+48 = 0x0 is not a page size`. The confirmation has to come
// from the image's CODE, which is readable here.
//
// The fingerprint: within `pthread_create`, the 64-bit loads off `&__libc` must
// be exactly {24, 48} -- `tls_size` and `page_size`. On aarch64 musl the
// function reads both:
//
//	adrp x19, <page>          ; &__libc
//	ldrb w4, [x19, #632]      ; +0  can_do_threads
//	add  x22, x19, #0x278     ; &__libc again, in a callee-saved register
//	ldr  x0, [x22, #24]       ; +24 tls_size
//	ldr  x0, [x22, #48]       ; +48 page_size, then the round-up idiom
//
// which is musl's declared field order -- so `tls_head` +16, `tls_align` +32
// and `tls_cnt` +40 follow from the same struct. A build that read a different
// SET produces no match and the runtime seeds nothing: this fails closed, which
// is the only acceptable direction when the alternative is writing pointers
// into arbitrary libc state.
//
// Only registers x19-x28 are followed. A caller-saved register holding
// `&__libc` is reassigned within a few instructions (`ldrb w0,[x0,#1]` then
// `sub x0,x20,#1` here), and tracking those without real liveness analysis
// would attribute a later unrelated load to this base.
func muslLibcTLSOffset(vma uint64, libcAddr uint64, word func(uint64) (uint32, bool)) uint64 {
	const window = 96
	var base uint32
	var page uint64
	var haveADRP bool
	held := map[uint32]bool{}
	seen := map[uint64]bool{}
	for i := uint64(0); i < window; i++ {
		w, ok := word(vma + i*4)
		if !ok {
			break
		}
		if !haveADRP {
			if p, rd, ok := decodeADRP(w, vma+i*4); ok {
				base, page, haveADRP = rd, p, true
				if page == libcAddr {
					held[base] = true
				}
			}
			continue
		}
		if imm, ok := decodeAddImm(w, base); ok && page+imm == libcAddr {
			if rd := w & 0x1f; rd >= 19 && rd <= 28 {
				held[rd] = true
			}
		}
		for rn := range held {
			if off, ok := decodeLdr64Unsigned(w, rn); ok {
				seen[off] = true
			}
		}
	}
	// Exactly the two fields musl's pthread_create reads, and no others.
	if len(seen) != 2 || !seen[24] || !seen[48] {
		return 0
	}
	return 24
}

// decodeStrOrStp matches `str Xt, [Xn, #imm12]` and `stp Xt1, Xt2, [Xn, #imm7]`
// (64-bit, unsigned/signed offset forms). It returns the base register, the
// byte offset, and the source registers -- one for str, two for stp.
func decodeStrOrStp(w uint32) (rn uint32, off uint64, src []uint32, ok bool) {
	switch {
	case w&0xffc00000 == 0xf9000000: // str Xt, [Xn, #imm12]
		return (w >> 5) & 0x1f, uint64((w>>10)&0xfff) * 8, []uint32{w & 0x1f}, true
	case w&0xffc00000 == 0xa9000000: // stp Xt1, Xt2, [Xn, #imm7]
		imm := int64((w >> 15) & 0x7f)
		if imm&0x40 != 0 {
			imm -= 0x80
		}
		if imm < 0 {
			return 0, 0, nil, false
		}
		return (w >> 5) & 0x1f, uint64(imm) * 8, []uint32{w & 0x1f, (w >> 10) & 0x1f}, true
	}
	return 0, 0, nil, false
}

// indirectCallSlots returns the data addresses `o` calls THROUGH: an
// `adrp`-based load into a register that a later `blr` jumps to.
//
// It is the second independent reading behind `rtldHookInitVMAs`. On its own,
// "a function stores two constant code addresses into consecutive words" is a
// weak signature -- plenty of code initialises a small table. Requiring that
// those same words are the target of an indirect call somewhere in the library
// turns "these look like hook pointers" into "these ARE dispatched through",
// which is the property that makes calling the installer safe.
func indirectCallSlots(objs []*Object, o *Object) map[uint64]bool {
	out := map[uint64]bool{}
	for _, sec := range o.file.Sections {
		if sec.Flags&elf.SHF_EXECINSTR == 0 || sec.Addr == 0 || sec.Size == 0 {
			continue
		}
		lo, hi := o.addr(sec.Addr), o.addr(sec.Addr+sec.Size)
		consts := map[uint32]uint64{}
		pend := map[uint32]uint64{}
		for at := lo; at+4 <= hi; at += 4 {
			w, err := readInsn(objs, at)
			if err != nil {
				break
			}
			if page, rd, ok := decodeADRP(w, at); ok {
				consts[rd] = page
				delete(pend, rd)
				continue
			}
			if imm, ok := decodeAddImmAny(w); ok {
				if base, have := consts[imm.rn]; have {
					consts[imm.rd] = base + imm.imm
				} else {
					delete(consts, imm.rd)
				}
				delete(pend, imm.rd)
				continue
			}
			// `ldr Xd, [Xn, #imm]` off a known constant base: Xd now holds
			// whatever that slot contains, and the slot is a candidate.
			if rn, off, dst, ok := decodeLdr64Any(w); ok {
				if base, have := consts[rn]; have {
					pend[dst] = base + off
				} else {
					delete(pend, dst)
				}
				delete(consts, dst)
				continue
			}
			if rn, ok := decodeBlr(w); ok {
				if slot, have := pend[rn]; have {
					out[slot] = true
				}
				continue
			}
		}
	}
	return out
}

// decodeLdr64Any matches `ldr Xt, [Xn, #imm12]` without fixing Xn.
func decodeLdr64Any(w uint32) (rn uint32, off uint64, rt uint32, ok bool) {
	if w&0xffc00000 != 0xf9400000 {
		return 0, 0, 0, false
	}
	return (w >> 5) & 0x1f, uint64((w>>10)&0xfff) * 8, w & 0x1f, true
}

// decodeBlr matches `blr Xn`.
func decodeBlr(w uint32) (uint32, bool) {
	if w&0xfffffc1f != 0xd63f0000 {
		return 0, false
	}
	return (w >> 5) & 0x1f, true
}

// rtldHookInitVMA returns the fused entry address of ld.so's hook installer --
// the function that fills the cluster of indirect-call pointers ld.so keeps in
// its own writable data (`__rtld_malloc` and family).
//
// WHY A CALL AND NOT A POKE. A fused image never runs ld.so, so those pointers
// stay NULL, and the first thing to call one dies:
//
//	fatal: vma 0 not in the lifted function table (__remill_function_call)
//
// which is what `pthread_create` hits -- `_dl_allocate_tls_storage` allocates
// the new thread's TLS through them. The slots are `attribute_hidden` and
// absent from dynsym, so seeding them individually would mean deciding by
// guesswork which of six is malloc and which is free; putting one in the wrong
// place is silent corruption. ld.so already contains a function that installs
// all of them correctly, so the runtime calls THAT.
//
// THE SIGNATURE, which is what makes it identifiable without a name: a function
// whose entire effect is storing ADRP+ADD CONSTANTS into four or more
// consecutive 8-byte slots of one writable object. On glibc 2.41 that is
//
//	14bc0: bti c
//	14bc4: adrp x2, 3f000 ; add x0, x2, #0xb00
//	14bcc: adrp x1, 9000  ; add x1, x1, #0xfc4
//	14bd4: str  x1, [x2, #2816]
//	       adrp/add x3, x2, x1 ...
//	14bf0: stp  x3, x2, [x0, #8]
//	14bf4: str  x1, [x0, #24]
//	14bf8: ret
//
// and it is DISCRIMINATING: the sibling installer that runs once libc is loaded
// stores values obtained from `bl` symbol lookups, not constants, so the
// simulation below discards its register state and finds no constant stores.
// Any instruction this does not model clears the tracked constants, which is
// what keeps a function that merely happens to store something from matching.
//
// Exactly one function must match, or nothing is emitted.
func rtldHookInitVMAs(objs []*Object, syms map[string]uint64) []uint64 {
	ld := objectAt(objs, syms["_rtld_global"])
	if ld == nil || ld.file == nil {
		return nil
	}
	called := indirectCallSlots(objs, ld)
	if len(called) == 0 {
		return nil
	}
	own, extra := funcRangesOf(ld)
	var found []uint64
	for _, fr := range append(append([]funcRange{}, own...), extra...) {
		if constPointerInstaller(objs, ld, ld.addr(fr.addr), fr.size, called) {
			found = append(found, ld.addr(fr.addr))
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i] < found[j] })
	// More than a handful means the signature is matching something it should
	// not, and the runtime CALLS these -- so refuse the lot rather than call
	// into whatever else matched.
	if len(found) > maxRtldHookInits {
		return nil
	}
	return found
}

// The cluster in glibc 2.41 has two families -- the four `__rtld_malloc`
// pointers and a lock pair -- with one installer each. Four is room for a glibc
// that grows another without turning a mis-identification into a call loop.
const maxRtldHookInits = 4

// constPointerInstaller reports whether the function at `start` does nothing but
// store constant code addresses into four or more consecutive pointer slots of
// one writable object. See rtldHookInitVMA for why that shape identifies ld.so's
// hook installer.
func constPointerInstaller(objs []*Object, ld *Object, start, size uint64, called map[uint64]bool) bool {
	if size == 0 || size > 512 {
		return false // the installer is a handful of instructions
	}
	consts := map[uint32]uint64{}
	slots := map[uint64]bool{}
	for at := start; at < start+size; at += 4 {
		w, err := readInsn(objs, at)
		if err != nil {
			return false
		}
		switch {
		case w == 0xd503245f || w == 0xd503201f || w == 0xd65f03c0: // bti c, nop, ret
			continue
		}
		if page, rd, ok := decodeADRP(w, at); ok {
			consts[rd] = page
			continue
		}
		if imm, ok := decodeAddImmAny(w); ok {
			if base, have := consts[imm.rn]; have {
				consts[imm.rd] = base + imm.imm
			} else {
				delete(consts, imm.rd)
			}
			continue
		}
		if rn, off, src, ok := decodeStrOrStp(w); ok {
			base, have := consts[rn]
			if !have {
				continue
			}
			for i, r := range src {
				v, known := consts[r]
				// The stored value must be CODE in this same object: a hook
				// points at a function, and requiring that rejects a function
				// that stores data pointers into an unrelated table.
				if !known || !isExecutableCode(ld, v) {
					continue
				}
				slots[base+off+uint64(i)*8] = true
			}
			continue
		}
		// Anything unmodelled invalidates what we think we know.
		consts = map[uint32]uint64{}
	}
	// TWO is enough BECAUSE of `called`. The lock pair is an installer with
	// exactly two slots, and raising the bar to four to feel safer is what made
	// the first version of this miss it -- the malloc family was installed, the
	// locks were not, and `_dl_allocate_tls_init` died on the very next null.
	// The safety comes from the cross-check, not from the count.
	if len(slots) < 2 {
		return false
	}
	for slot := range slots {
		if !called[slot] {
			return false // written, but never dispatched through: not a hook
		}
	}
	// Consecutive 8-byte slots of one object, which is what a pointer table is.
	var addrs []uint64
	for a := range slots {
		addrs = append(addrs, a)
	}
	sort.Slice(addrs, func(i, j int) bool { return addrs[i] < addrs[j] })
	for i := 1; i < len(addrs); i++ {
		if addrs[i] != addrs[i-1]+8 {
			return false
		}
	}
	return isWritableData(ld, addrs[0])
}

// addImm is `add Xd, Xn, #imm` with both registers, unlike decodeAddImm which
// matches against a known Xn.
type addImm struct {
	rd, rn uint32
	imm    uint64
}

func decodeAddImmAny(w uint32) (addImm, bool) {
	if w&0xff800000 != 0x91000000 {
		return addImm{}, false
	}
	return addImm{rd: w & 0x1f, rn: (w >> 5) & 0x1f, imm: uint64((w >> 10) & 0xfff)}, true
}

// isExecutableCode reports whether `addr` (fused) lands in an executable
// section of `o`.
func isExecutableCode(o *Object, addr uint64) bool {
	for _, sec := range o.file.Sections {
		if sec.Addr == 0 || sec.Flags&elf.SHF_ALLOC == 0 {
			continue
		}
		if addr < o.addr(sec.Addr) || addr >= o.addr(sec.Addr+sec.Size) {
			continue
		}
		return sec.Flags&elf.SHF_EXECINSTR != 0
	}
	return false
}

// objectAt returns the object whose image covers the FUSED address `addr`.
// Addresses here are fused throughout, because `adrp` is pc-relative and the
// pc it was decoded from is fused: an object's base is page-aligned, so the
// instruction's page displacement resolves in fused space unchanged.
func objectAt(objs []*Object, addr uint64) *Object {
	for _, o := range objs {
		if addr >= o.addr(o.lo) && addr < o.addr(o.hi) {
			return o
		}
	}
	return nil
}

// isWritableData reports whether the fused address `addr` lands in an
// allocated, writable, non-executable section of `o`.
func isWritableData(o *Object, addr uint64) bool {
	for _, sec := range o.file.Sections {
		if sec.Addr == 0 || sec.Flags&elf.SHF_ALLOC == 0 {
			continue
		}
		if addr < o.addr(sec.Addr) || addr >= o.addr(sec.Addr+sec.Size) {
			continue
		}
		return sec.Flags&elf.SHF_WRITE != 0 && sec.Flags&elf.SHF_EXECINSTR == 0
	}
	return false
}

// byteAt reads one byte of an object's relocated image at a fused address.
func byteAt(o *Object, addr uint64) (byte, bool) {
	lo := o.addr(o.lo)
	if addr < lo || addr >= o.addr(o.hi) {
		return 0, false
	}
	return o.image[addr-lo], true
}

// tpAccessorOffset decodes a three-instruction `mrs TPIDR_EL0 / <op> / ret`
// accessor at vma and returns the offset its middle instruction applies.
func tpAccessorOffset(objs []*Object, vma uint64, decode func(uint32) (uint64, bool)) (uint64, bool) {
	if vma == 0 {
		return 0, false
	}
	w0, err0 := readInsn(objs, vma)
	w1, err1 := readInsn(objs, vma+4)
	w2, err2 := readInsn(objs, vma+8)
	if err0 != nil || err1 != nil || err2 != nil {
		return 0, false
	}
	// mrs Xd, TPIDR_EL0 -- any destination register.
	if w0&0xffffffe0 != 0xd53bd040 {
		return 0, false
	}
	if w2 != 0xd65f03c0 { // ret
		return 0, false
	}
	return decode(w1)
}

// decodeLdurNeg matches `ldur Wt, [Xn, #-imm]` and returns the magnitude.
func decodeLdurNeg(w uint32) (uint64, bool) {
	if w&0xffe00c00 != 0xb8400000 {
		return 0, false
	}
	imm := int32(w>>12) & 0x1ff
	if imm >= 0x100 {
		imm -= 0x200 // sign-extend the 9-bit displacement
	}
	if imm >= 0 {
		return 0, false // must be below the thread pointer
	}
	return uint64(-imm), true
}

// decodeSubImm matches `sub Xd, Xn, #imm12` with no shift.
func decodeSubImm(w uint32) (uint64, bool) {
	if w&0xff800000 != 0xd1000000 {
		return 0, false
	}
	return uint64((w >> 10) & 0xfff), true
}
