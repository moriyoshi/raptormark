package fuse

import (
	"debug/elf"
	"encoding/binary"
	"sort"
)

// Function-boundary recovery from RELOCATED code pointers.
//
// This is the fourth source of boundaries, after the symbol table, `.eh_frame`
// (ehframe.go) and computed `adrp`/`add` pointers (funcptr.go). The three of
// them together still miss an entire category on a stripped musl closure: a
// function whose address appears only in a STATIC INITIALISER.
//
// musl's `__stdout_FILE` is the case that forced it:
//
//	hidden FILE __stdout_FILE = { ..., .write = __stdout_write, ... };
//
// `__stdout_write` is hidden, Alpine's libc carries no `.symtab`, musl ships 916
// bytes of `.eh_frame` for the whole library, and no instruction anywhere
// computes the address -- the linker emits a relative relocation and the loader
// writes it. So all three earlier sources are blind by construction, and redis
// died on its first buffered write with
//
//	vma 0xa53478 not in the lifted function table (__remill_function_call)
//
// The evidence a relocation gives is stronger than anything funcptr.go works
// with, which is why the filter here is much shorter. A relative relocation
// exists precisely because a pointer must be fixed up at load time; if its
// target lands in an EXECUTABLE section, that pointer is a code pointer. The one
// counterexample worth naming is an absolute-address jump table, whose entries
// point INSIDE a function -- and `covered` already rejects those wherever the
// enclosing function is known, which for a jump table's owner it essentially
// always is, since a function large enough to need one is not a stub.
//
// Both encodings have to be read. Debian's glibc and Alpine's musl are both
// linked with packed relative relocations, so `.relr.dyn` carries most of them
// and `.rela.dyn` the remainder.

// relocRelative is R_AARCH64_RELATIVE. debug/elf has the constant, but naming it
// here keeps the two encodings side by side.
const relocRelative = elf.R_AARCH64_RELATIVE

// relocPointerFuncs returns function starts recovered from relative
// relocations whose targets land in one of `ranges`.
//
// It reads the object's ORIGINAL section contents rather than the mutated fused
// image, so it does not matter whether `applyRELR` has already rebased the
// sites: the value read is always the link-time target, in the same address
// space as `ranges` and `known`.
func relocPointerFuncs(f *elf.File, ranges []execRange, known []funcRange) []funcRange {
	if f == nil || len(ranges) == 0 {
		return nil
	}
	return relocTargetFuncs(relativeRelocTargets(f), ranges, known)
}

// relocTargetFuncs is relocPointerFuncs' filter, separated from the ELF reading
// so the acceptance rules can be tested without an on-disk fixture.
func relocTargetFuncs(targets []uint64, ranges []execRange, known []funcRange) []funcRange {
	if len(ranges) == 0 {
		return nil
	}
	inExec := func(v uint64) bool {
		for _, r := range ranges {
			if v >= r.addr && v < r.addr+r.size {
				return true
			}
		}
		return false
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

	seen := map[uint64]bool{}
	var cands []uint64
	consider := func(target uint64) {
		if target == 0 || seen[target] || !inExec(target) {
			return
		}
		if covered(sorted, target) {
			return
		}
		if i := sort.Search(len(sorted), func(k int) bool { return sorted[k].addr >= target }); i < len(sorted) && sorted[i].addr == target {
			return
		}
		// The same cheap sanity check funcptr.go makes: whatever precedes a
		// function start has to be something a function can end with, or padding
		// between functions. Without it a relocation into the middle of an
		// unsymbolised function would declare a boundary there, which is the
		// failure mode this whole area exists to avoid.
		if prev, ok := wordAt(target - 4); ok && !isTerminator(prev) && !isPadding(prev) {
			return
		}
		seen[target] = true
		cands = append(cands, target)
	}

	for _, t := range targets {
		consider(t)
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i] < cands[j] })
	return sizeByNextBoundary(cands, ranges, sorted)
}

// relativeRelocTargets returns the link-time address every relative relocation
// in `f` resolves to, in both encodings.
//
// It reads the object's ORIGINAL section contents rather than the mutated fused
// image, so it does not matter whether `applyRELR` has already rebased the
// sites: the value read is always the link-time target.
func relativeRelocTargets(f *elf.File) []uint64 {
	var out []uint64
	// Packed: no explicit addend, the target is the value already at the site.
	for _, sec := range f.Sections {
		if sec.Type != shtRELR {
			continue
		}
		data, err := sec.Data()
		if err != nil {
			continue
		}
		_ = walkRELR(data, func(vaddr uint64) error {
			if v, ok := read64At(f, vaddr); ok {
				out = append(out, v)
			}
			return nil
		})
	}
	// Unpacked: the addend IS the target.
	for _, sec := range f.Sections {
		if sec.Type != elf.SHT_RELA {
			continue
		}
		data, err := sec.Data()
		if err != nil {
			continue
		}
		for off := 0; off+24 <= len(data); off += 24 {
			info := binary.LittleEndian.Uint64(data[off+8:])
			if elf.R_AARCH64(info&0xffffffff) != relocRelative {
				continue
			}
			out = append(out, binary.LittleEndian.Uint64(data[off+16:]))
		}
	}
	return out
}

// isPadding recognises the filler a linker puts between functions. A `nop`
// after a function's `ret` is alignment, not code that fell through.
func isPadding(w uint32) bool {
	return w == 0xd503201f || w == 0 // nop, or zero fill
}

// read64At reads a little-endian u64 at a link-time vaddr from the file's
// original section contents.
func read64At(f *elf.File, vaddr uint64) (uint64, bool) {
	for _, sec := range f.Sections {
		if sec.Type == elf.SHT_NOBITS || sec.Addr == 0 {
			continue
		}
		if vaddr < sec.Addr || vaddr+8 > sec.Addr+sec.Size {
			continue
		}
		data, err := sec.Data()
		if err != nil || uint64(len(data)) < vaddr-sec.Addr+8 {
			return 0, false
		}
		return binary.LittleEndian.Uint64(data[vaddr-sec.Addr:]), true
	}
	return 0, false
}

// sizeByNextBoundary bounds each recovered start by the next boundary of any
// kind -- the next known function, the next recovered start, or the end of its
// executable section. Shared with codePointerFuncs' own sizing rule: it is the
// most a disassembler can honestly claim, and it never overlaps something
// already known.
func sizeByNextBoundary(starts []uint64, ranges []execRange, sorted []funcRange) []funcRange {
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
		if end > a {
			out = append(out, funcRange{addr: a, size: end - a})
		}
	}
	return out
}
