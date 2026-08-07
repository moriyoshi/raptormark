package fuse

import (
	"debug/elf"
	"fmt"
)

// FuseClosure fuses several programs that share one rootfs, giving every library
// the same address in every image.
//
// This is the caller PlanLayout exists for. Fusing programs one at a time cannot
// produce a shared layout, because a base assigned by dense packing depends on
// the other objects in that image alone.
//
// FALLBACK IS NOT AN ERROR. If the union does not fit under BRK_START_VMA, this
// fuses each program independently, exactly as Fuse does today: the sharing is
// lost, the images are correct. Reporting overflow as a failure would make a
// large closure unbuildable in exchange for an optimization, which is the wrong
// trade -- the caller is told via the returned Report so a build can log it
// rather than discover it as a missing speedup much later.
//
// The images are returned in the order the paths were given.
func FuseClosure(exePaths []string, opts Options) ([][]byte, Report, error) {
	var rep Report
	if len(exePaths) == 0 {
		return nil, rep, fmt.Errorf("fuse: FuseClosure needs at least one program")
	}

	// Load once. Every program is loaded before any is emitted, because the
	// layout is a function of the whole closure.
	programs := make([]Program, 0, len(exePaths))
	// Kept OUT of `rep`: `planOrFallback` below returns a fresh Report and
	// assigns over it, so anything accumulated here before that line is
	// silently discarded. That is how the first version of this lost every
	// skip -- the fuse succeeded, reported nothing, and the dropped plugin was
	// only visible as a missing module at run time.
	var skippedExtras []SkippedExtra
	seenSkip := map[string]bool{}
	for _, path := range exePaths {
		objs, skipped, err := load(path, opts)
		if err != nil {
			return nil, rep, fmt.Errorf("fuse: loading %s: %w", path, err)
		}
		// Reported, never silent: a dropped plugin is a capability the image
		// does not have, and the caller has to be able to see which.
		for _, sk := range skipped {
			if !seenSkip[sk.Name] {
				seenSkip[sk.Name] = true
				skippedExtras = append(skippedExtras, sk)
			}
		}
		programs = append(programs, Program{Objs: objs, ExeIsPIE: objs[0].file.Type == elf.ET_DYN})
	}

	layoutOpts, rep := planOrFallback(programs, opts)
	rep.SkippedExtras = skippedExtras

	out := make([][]byte, 0, len(programs))
	for i, p := range programs {
		img, err := fuseLoaded(p, layoutOpts)
		if err != nil {
			return nil, rep, fmt.Errorf("fuse: %s: %w", exePaths[i], err)
		}
		out = append(out, img)
	}
	return out, rep, nil
}

// Report describes what FuseClosure managed to do, so a build can log the
// difference between "sharing applied" and "sharing silently skipped". A missing
// speedup is otherwise indistinguishable from a slow machine.
type Report struct {
	// Shared is false when the closure fell back to per-image packing.
	Shared bool
	// Reason explains a false Shared.
	Reason string
	// Top is the first address above every planned library.
	Top uint64
	// SharedMin is the first address a library occupies: the boundary below
	// which code is program-specific and must not be shared.
	SharedMin uint64
	// LibRanges is where each library sits, sorted by address. The partitioner
	// needs these to keep a partition inside one library; see Layout.Ranges.
	LibRanges []LibRange
	// Libraries is how many distinct libraries the plan covers.
	Libraries int
	// SkippedExtras are the `Options.Extra` plugins that could not be fused,
	// de-duplicated across the closure. Each is a dlopen the guest will find
	// unsatisfiable -- which is what happens on the real image too, but the
	// caller must be told rather than left to discover it at run time.
	SkippedExtras []SkippedExtra
}

// planOrFallback decides between the shared layout and per-image packing.
//
// Split out from FuseClosure so the DECISION is testable without ELF files on
// disk -- the surrounding load-and-emit is not, and the decision is the part
// that can be wrong in a way nothing downstream would notice.
func planOrFallback(programs []Program, opts Options) (Options, Report) {
	var rep Report
	l, err := PlanLayout(programs, opts)
	if err != nil {
		// Overflow, or a rootfs that is not actually shared. Either way the
		// per-image path is still correct, so degrade rather than fail.
		rep.Reason = err.Error()
		return opts, rep
	}
	opts.Layout = l
	rep.Shared = true
	rep.Top = l.Top()
	rep.SharedMin = l.SharedMin()
	rep.LibRanges = l.Ranges()
	rep.Libraries = len(l.base)
	return opts, rep
}

// fuseLoaded is Fuse's body for an already-loaded program. Fuse itself is left
// alone so the single-program path keeps exactly its current behaviour.
func fuseLoaded(p Program, opts Options) ([]byte, error) {
	objs := p.Objs
	if err := assignBases(objs, p.ExeIsPIE, opts); err != nil {
		return nil, err
	}
	syms, ifuncSyms := globalSymbols(objs)
	tlsSyms := globalTLSSymbols(objs)
	if err := applyRELR(objs); err != nil {
		return nil, err
	}
	var deferred []ifuncFixup
	var tlsdescs []tlsdescFixup
	resolved := map[uint64]uint64{}
	if err := relocate(objs, syms, ifuncSyms, tlsSyms, &deferred, &tlsdescs, resolved); err != nil {
		return nil, err
	}
	patchDLStubs(objs)
	return emit(objs, deferred, tlsdescs, buildTables(objs, syms, ifuncSyms, resolved))
}
