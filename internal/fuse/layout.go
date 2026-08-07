package fuse

import (
	"debug/elf"
	"fmt"
	"sort"
	"strings"
)

// Closure-wide library layout.
//
// WHY. assignBases packs each image densely and independently, so a library's
// base depends on every object placed before it -- which differs per program.
// The same libc therefore lands at a different address in each fused image, and
// since lifted IR embeds absolute guest addresses, the same machine code lifts
// to different bitcode per program and nothing can be shared.
//
// The redundancy this wastes is large and was measured at planning time: of
// initdb's .text, 8.87 MB of 9.11 MB is byte-identical to code already in
// postgres (97%); for dash it is 1.25 MB of 1.33 MB (94%). Those programs cost
// 12 and 8 minutes of translation that is almost entirely a recompilation of
// postgres's libraries at a different address.
//
// A closure-wide plan fixes every library's base across all programs in one
// closure, so identical code lifts to identical IR. This is PHASE 1a of the plan
// and is a prerequisite, not a payoff: partitions also carry per-program symbol
// tags from namespace-object, so reuse needs content-addressed naming (Phase 1b)
// before the partition cache can hit across programs.
//
// WHY NOT hash-scattered addressing, which would need no closure knowledge: the
// arena is an identity map and the fused-object region is only
// [0x400000, BRK_START_VMA) = 156 MiB, far too small to scatter into. That was
// spiked and refuted; see JOURNAL.md.
//
// COST OF THE HOLES. None at runtime: the arena is a fixed
// vec![0u8; MEMORY_ARENA_SIZE] cloned wholesale per suspended process
// (runtime/src/arena.rs), so occupancy does not affect memory or snapshot time.
// None on disk either: emit() writes one small PT_LOAD per merged section, so an
// address gap is not file padding.

// brkStartVMA is where the guest heap begins; everything fused must fit below
// it. DUPLICATED from runtime/src/arena.rs -- Go has no view of the Rust
// constants, and the two must not drift.
// TestBrkStartMatchesTheRuntime reads the Rust source and fails if it does.
const brkStartVMA = 0x0A000000

// Layout is a closure-wide assignment of library bases, keyed by the library's
// path in the guest rootfs.
//
// Path is a sound identity WITHIN one closure, because a closure shares one
// rootfs and a path there names one file. PlanLayout verifies that assumption
// rather than trusting it: two programs disagreeing about the contents at a path
// is an error, not a silently wrong layout.
type Layout struct {
	base map[string]uint64
	// spans is every planned library's occupied range, sorted by address. Kept
	// alongside base because a base alone does not say where a library ENDS, and
	// the partitioner needs the boundary; see Ranges.
	spans []LibRange
	// min is the first address the lowest-placed library occupies.
	min uint64
	// top is the first address above every planned library, for diagnostics.
	top uint64
}

// LibRange is one library's span in the fused image, as [Start, End).
type LibRange struct {
	Path  string
	Start uint64
	End   uint64
}

// Program is one program's loaded object set, as layout() sees it.
type Program struct {
	Objs []*Object
	// ExeIsPIE selects where the executable sits, matching assignBases.
	ExeIsPIE bool
}

// PlanLayout computes a base for every library appearing in any of the programs.
//
// It is a pure function of the object sets so it can be tested without ELFs on
// disk; PlanLayoutFor does the loading.
//
// The executable is deliberately NOT planned. Each program has its own, they are
// never shared, and giving them a common region would waste the scarce space
// that the libraries need. Instead every library is placed above the highest
// executable top in the closure, so no program's exe can collide with a library.
//
// An overflow of the fused region is returned as an error. The caller's correct
// response is to fall back to per-image dense packing -- losing the sharing and
// staying correct -- never to emit an image that runs off the end.
func PlanLayout(programs []Program, opts Options) (*Layout, error) {
	if len(programs) == 0 {
		return nil, fmt.Errorf("fuse: PlanLayout needs at least one program")
	}
	exeBase := opts.ExeBase
	if exeBase == 0 {
		exeBase = defaultExeBase
	}

	var exeTop uint64
	libs := map[string]*Object{}
	for i, p := range programs {
		if len(p.Objs) == 0 {
			return nil, fmt.Errorf("fuse: PlanLayout: program %d has no objects", i)
		}
		exe := p.Objs[0]
		// Mirrors assignBases: a non-PIE executable keeps its absolute vaddrs,
		// a PIE one is placed at exeBase.
		top := exe.hi
		if p.ExeIsPIE {
			top = exeBase + exe.hi
		}
		if top > exeTop {
			exeTop = top
		}
		for _, o := range p.Objs[1:] {
			prev, seen := libs[o.Path]
			if !seen {
				libs[o.Path] = o
				continue
			}
			// Same path, different geometry means the closure does not share one
			// rootfs after all, and a shared base would be actively wrong.
			if prev.lo != o.lo || prev.hi != o.hi {
				return nil, fmt.Errorf(
					"fuse: PlanLayout: %s differs between programs (span %#x-%#x vs %#x-%#x); "+
						"a closure must share one rootfs", o.Path, prev.lo, prev.hi, o.lo, o.hi)
			}
		}
	}

	// Deterministic order, so the same closure always yields the same plan. Sort
	// by path: it is stable, and unlike a hash it keeps a library's neighbours
	// stable too when an unrelated program joins the closure.
	paths := make([]string, 0, len(libs))
	for p := range libs {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	l := &Layout{base: make(map[string]uint64, len(paths))}
	next := exeTop
	for _, path := range paths {
		o := libs[path]
		next = (next + libAlign - 1) &^ (libAlign - 1)
		if l.min == 0 {
			l.min = next // the first library's first occupied address
		}
		l.base[path] = next - o.lo
		l.spans = append(l.spans, LibRange{Path: path, Start: next, End: next + (o.hi - o.lo)})
		next = next + (o.hi - o.lo)
	}
	l.top = next

	if next > brkStartVMA {
		return nil, fmt.Errorf(
			"fuse: closure-wide layout needs %#x but the fused region ends at %#x "+
				"(%d libraries over %d programs); fall back to per-image packing",
			next, uint64(brkStartVMA), len(paths), len(programs))
	}
	return l, nil
}

// PlanLayoutFor loads each program's closure and plans a layout over the union.
func PlanLayoutFor(exePaths []string, opts Options) (*Layout, error) {
	programs := make([]Program, 0, len(exePaths))
	for _, exe := range exePaths {
		// A skipped plugin is not an error here: the layout planner only needs
		// to know where objects go, and FuseClosure reports the skips.
		objs, _, err := load(exe, opts)
		if err != nil {
			return nil, fmt.Errorf("fuse: planning layout for %s: %w", exe, err)
		}
		programs = append(programs, Program{Objs: objs, ExeIsPIE: objs[0].file.Type == elf.ET_DYN})
	}
	return PlanLayout(programs, opts)
}

// Top reports the first address above every planned library.
func (l *Layout) Top() uint64 { return l.top }

// SharedMin is the lowest address any planned library occupies, i.e. the
// boundary between program-specific code and code shared across the closure.
//
// Everything below it is an executable, which is never shared; everything at or
// above it is library code at a base fixed for the whole closure. Downstream
// tools use this to decide which lifted functions may keep a common name --
// classifying by "the name looks address-derived" alone would sweep in each
// program's own executable, whose addresses collide across programs by design
// (they all start at ExeBase) while their contents differ.
func (l *Layout) SharedMin() uint64 { return l.min }

// Ranges reports where each planned library sits, sorted by address.
//
// WHY THIS LEAVES THE PACKAGE. The partitioner (builder/ecv-split.cpp) sees only
// bitcode. It can recover a guest address from a lifted name, which carries one
// since patches/0046, but nothing in the module says where one library ends and
// the next begins -- and that boundary is exactly what makes a partition
// reusable.
//
// Bucketing by a hash of the name cannot do it. A partition's cache key is over
// its whole membership, and a name hash spreads every library across every
// bucket: measured on the postgres closure, each shared bucket drew from a median
// of 26 libraries and each library was smeared over 63 of 70 buckets, so one
// library present in program A and absent in B changed EVERY bucket. Partitioning
// within a library instead makes a partition's membership depend on that
// library's own symbol list, which is program-independent -- of the 21 libraries
// postgres and initdb both link, all 21 lift identical symbol sets.
func (l *Layout) Ranges() []LibRange {
	if l == nil {
		return nil
	}
	out := make([]LibRange, len(l.spans))
	copy(out, l.spans)
	return out
}

// FormatLibRanges renders ranges for ECV_LIB_RANGES as `start:end,...` in hex.
//
// One formatter so every caller emits the same thing; the parser on the other
// side is builder/ecv-split.cpp. Ranges are already sorted by address and the
// consumer binary-searches them, so the order is part of the format.
func FormatLibRanges(ranges []LibRange) string {
	var b strings.Builder
	for i, r := range ranges {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%#x:%#x", r.Start, r.End)
	}
	return b.String()
}

// baseFor returns the planned base for a library, and whether it was planned.
func (l *Layout) baseFor(o *Object) (uint64, bool) {
	if l == nil {
		return 0, false
	}
	b, ok := l.base[o.Path]
	return b, ok
}
