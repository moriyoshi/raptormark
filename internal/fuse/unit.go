package fuse

import (
	"debug/elf"
	"fmt"
)

// Fusing a dlopen'd plugin as its OWN image.
//
// WHY. Today every `Options.Extra` plugin is merged into the program's one fused
// image, so the module carries every plugin whether or not the guest ever
// dlopens one, and every plugin's exports land in ONE flat `.ecv.dlsyms` that
// the runtime's `dlsym` searches while ignoring the handle. postgres:17 ships 79
// extensions that each define `Pg_magic_func`, `_PG_init` and `pg_finfo_*`, so
// first-definition-wins silently binds the wrong one -- the defect TODO.md
// records and the reason this exists.
//
// A UNIT is one plugin emitted as its own ET_EXEC image, at the base the
// closure-wide Layout planned for it, carrying only its OWN bring-up tables.
//
// WHY THIS IS SOUND, and it is the one property everything here rests on: a
// plugin is relocated against the WHOLE closure, at the closure's fixed bases,
// and only then emitted alone. Its references to libc and friends are therefore
// already resolved to the addresses those objects occupy in every image of the
// closure, so no runtime symbol resolver is needed -- exactly as for a plugin
// fused in-image.
//
// That is measured rather than assumed. `.agents-workspace/drivers/plugunit`
// fuses one plugin into two different programs' images against one Layout and
// compares the bytes occupying its planned range: IDENTICAL, at two different
// layouts, with the content-only digest moving when the layout moves (so the
// comparison is over relocated words, not inert ones). See JOURNAL.md,
// "Phase 0c".

// Unit is one dlopen-able object emitted as its own fused image.
type Unit struct {
	// Path is the plugin's path as `Options.Extra` named it, i.e. the host path
	// under the exported rootfs. The dlopen map keys on the GUEST path, which
	// the caller derives from this.
	Path string
	// Name is the soname or basename, for diagnostics.
	Name string
	// Base is the address the closure-wide Layout planned for this unit. Callers
	// need it because the unit's guest addresses are fixed at fuse time and
	// cannot move at load time.
	Base uint64
	// Image is the fused ET_EXEC image holding only this plugin.
	Image []byte
}

// FuseWithUnits fuses exePath's closure and additionally emits each
// `Options.Extra` plugin as its own unit image.
//
// ⚠️ The returned MAIN image EXCLUDES the plugins. That is the point -- it is
// what stops a postgres module from carrying 79 extensions it may never dlopen
// -- but it means the main image alone is not equivalent to what `Fuse` returns
// for the same options. A caller that emits the main image and drops the units
// has built a program whose every `dlopen` fails.
//
// Like Fuse and unlike FuseClosure, an unsatisfiable plugin is REFUSED rather
// than skipped: this signature can report it, and a silently absent dlopen'd
// module is the failure Options.Extra exists to prevent.
func FuseWithUnits(exePath string, opts Options) ([]byte, []Unit, error) {
	objs, skipped, err := load(exePath, opts)
	if err != nil {
		return nil, nil, err
	}
	if len(skipped) > 0 {
		return nil, nil, fmt.Errorf("fuse: cannot satisfy dlopen'd plugin %s: %s",
			skipped[0].Name, skipped[0].Reason)
	}
	if err := layout(objs, opts); err != nil {
		return nil, nil, err
	}

	// Relocation sees EVERY object, including the plugins. Narrowing it to the
	// main closure would leave each plugin's references to libc unresolved, and
	// an unresolved GLOB_DAT is written as 0 rather than reported -- a plugin
	// whose every libc call goes through a null pointer, discovered at run time.
	allSyms, allIfuncSyms := globalSymbols(objs)
	tlsSyms := globalTLSSymbols(objs)
	if err := applyRELR(objs); err != nil {
		return nil, nil, err
	}
	var deferred []ifuncFixup
	var tlsdescs []tlsdescFixup
	resolved := map[uint64]uint64{}
	if err := relocate(objs, allSyms, allIfuncSyms, tlsSyms, &deferred, &tlsdescs, resolved); err != nil {
		return nil, nil, err
	}
	patchDLStubs(objs)

	var mainObjs, plugins []*Object
	for _, o := range objs {
		if o.isPlugin {
			plugins = append(plugins, o)
		} else {
			mainObjs = append(mainObjs, o)
		}
	}

	// The main image's own tables are built from the main objects only, so its
	// `.ecv.dlsyms` stops advertising every plugin's symbols and its `.ecv.init`
	// stops running their constructors at startup. A plugin's constructors are
	// `dlopen`'s job.
	mainSyms, mainIfuncSyms := globalSymbols(mainObjs)
	main, err := emit(mainObjs,
		ifuncsWithin(mainObjs, deferred), tlsdescsOf(mainObjs, tlsdescs),
		buildTables(mainObjs, mainSyms, mainIfuncSyms, resolved))
	if err != nil {
		return nil, nil, fmt.Errorf("fuse: emitting the main image: %w", err)
	}

	units := make([]Unit, 0, len(plugins))
	for _, p := range plugins {
		// ❗ EMIT A UNIT'S SECTIONS UNSUFFIXED.
		//
		// `Object.suffix()` tags a LIBRARY's sections `.lN` so several objects
		// can share one image without colliding. A unit has exactly one object,
		// so there is nothing to disambiguate -- and the suffix is actively
		// fatal: libdwarf's `dwarf_get_fde_list_eh`, which elflift reads frame
		// data through, looks for a section named exactly `.eh_frame`. A unit
		// carrying only `.eh_frame.lN` has no frame data as far as libdwarf is
		// concerned, so elflift falls into its stripped-binary path and crashes
		// with "(Custom) Segmantation Fault."
		//
		// ⚠️ THIS ALONE DID NOT FIX THE CRASH, and the paragraph above must not
		// be read as saying it did. With the sections unsuffixed AND `e_entry`
		// repointed at a real sized function, a unit still died with
		// "(Custom) Segmantation Fault." The reasoning above stands on its own;
		// it was simply not the whole story.
		//
		// ✅ RESOLVED 2026-08-23 -- three separate causes, all now fixed. This
		// block said "that cause is not yet known" until 2026-08-26, by which
		// time units had been lifting for three days; corrected here rather
		// than left to read as an open blocker.
		//   1. `patches/0066-drvd-rmp-covers-store-before-call.patch` -- the
		//      real one. elflift threw `std::out_of_range` in
		//      `VirtualRegsOpt::GetRegValueFromCacheMap` because our own
		//      `patches/0007` widened the STORE sites (`CheckStoreBeforeCall`)
		//      without widening the PHI seeding (`CheckPassedArgsRegs`), so
		//      x9..x30 and v8..v15 could be stored before a call with no PHI.
		//      Latent for everyone, not unit-specific; the fix is the UNION of
		//      the two predicates, since neither contains the other.
		//   2. A zero-sized `SHN_ABS` FUNC symbol dereferences null in
		//      `TraceManager::SetELFData` (BFD returns a NULL section for
		//      `SHN_ABS`). `emit` synthesised `_start` at `e_entry`, and a
		//      unit's `e_entry` of 0 rebases to the image base, below every
		//      section. `emit` no longer emits it -- see `fuse.go:1252`.
		//   3. A unit has no entry function at all, and elflift refuses an
		//      image whose `e_entry` names none. `emit` points it at the
		//      lowest-addressed sized FUNC symbol (`fuse.go:1278`), which is
		//      sound only because a unit's `entry_func` is never called.
		//
		// See `.agents/docs/LTM/dynamic-side-module-loading.md`, "patches/0066:
		// the VRP crash was OUR bug".
		//
		// Restored afterwards so the object is left as the rest of the closure
		// sees it -- `Base`, `LibIndex` and the section names are what the MAIN
		// image was emitted against, and a unit must not mutate shared state.
		savedIndex := p.LibIndex
		p.LibIndex = -1
		img, err := emit([]*Object{p},
			ifuncsWithin([]*Object{p}, deferred), tlsdescsOf([]*Object{p}, tlsdescs),
			unitTables(p, resolved))
		p.LibIndex = savedIndex
		if err != nil {
			return nil, nil, fmt.Errorf("fuse: emitting unit %s: %w", p.Name, err)
		}
		units = append(units, Unit{Path: p.Path, Name: p.Name, Base: p.Base, Image: img})
	}
	return main, units, nil
}

// unitTables are the bring-up tables for ONE plugin.
//
// Only two of the five apply, and the three that do not are omitted rather than
// left to come out empty by accident:
//
//   - `.ecv.early` names glibc's `__libc_early_init`, `.ecv.stacklists`
//     describes `_rtld_global`'s thread lists, and `.ecv.musltp` describes musl's
//     thread pointer. All three are properties of the LIBC in the main image and
//     are applied once at bring-up (entry.rs). A plugin restating them would
//     have the runtime re-run libc's early init on every dlopen.
//   - `.ecv.init` is the plugin's own constructors, which is exactly what
//     dlopen must run.
//   - `.ecv.dlsyms` is the plugin's own exports, and scoping it is the whole
//     point: it is what lets `dlsym(handle, ...)` answer from the module the
//     handle names instead of from one flat closure-wide table where 79
//     extensions' `_PG_init` collide and the first wins.
func unitTables(p *Object, resolved map[uint64]uint64) bringupTables {
	syms, ifuncSyms := globalSymbols([]*Object{p})
	return bringupTables{
		initArray: initDescriptor([]*Object{p}),
		dlsyms:    dlsymsDescriptor(syms, ifuncSyms, resolved),
	}
}

// ifuncsWithin keeps the deferred ifunc fixups whose GOT slot lies inside one of
// `objs`, so each image's `.ecv.irela` describes only its own slots.
//
// Selected by ADDRESS rather than by which object produced the relocation,
// because that is what the consumer does: `apply_ifuncs` writes to
// `got_slot_vma` in the arena, so the question is whose memory the slot is in.
func ifuncsWithin(objs []*Object, all []ifuncFixup) []ifuncFixup {
	var out []ifuncFixup
	for _, f := range all {
		for _, o := range objs {
			if f.slot >= o.addr(o.lo) && f.slot < o.addr(o.hi) {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

// tlsdescsOf keeps the TLSDESC fixups belonging to one of `objs`. Unlike ifunc
// fixups these already name their object, so no address arithmetic is needed.
func tlsdescsOf(objs []*Object, all []tlsdescFixup) []tlsdescFixup {
	in := make(map[*Object]bool, len(objs))
	for _, o := range objs {
		in[o] = true
	}
	var out []tlsdescFixup
	for _, f := range all {
		if in[f.o] {
			out = append(out, f)
		}
	}
	return out
}

// UnitExports lists the global symbols a unit image defines, for callers that
// need to know what a plugin offers without parsing `.ecv.dlsyms` back out.
//
// Mirrors `globalSymbols`' admission rule -- defined, named, first-wins -- so
// it cannot drift from what the table actually contains, WITH ONE DELIBERATE
// DIFFERENCE.
//
// ❗ THE `STB_LOCAL` FILTER BELOW IS NOT AN INCONSISTENCY. Do not "reconcile"
// it. The two functions read different tables:
//
//   - `globalSymbols` reads `.dynsym`. Measured 2026-08-26 across all 89
//     objects of the postgres:17 closure: ZERO named defined `STB_LOCAL`
//     entries. The only defined locals there are section symbols, whose
//     `st_name` is 0, so `debug/elf` reports an empty Name and the existing
//     `s.Name == ""` check already drops them. A bind filter would be dead code.
//   - This reads the SYNTHESIZED `.symtab`, which `emit` deliberately fills
//     with named `STB_LOCAL` `_ecv_fde_<addr>` boundary symbols recovered from
//     `.eh_frame` (see fuse.go, the `_ecv_fde_` block). `bash-glibc.fused`
//     carries 2,110 of them. Without this filter every one would be reported as
//     an export of the unit.
//
// ⚠️ This comment previously read "mirrors globalSymbols' admission rule
// exactly", which is false and actively misleading -- it invited exactly the
// reconciliation that would break one side or add dead code to the other.
func UnitExports(u Unit) ([]string, error) {
	f, err := elf.NewFile(bytesReaderAt(u.Image))
	if err != nil {
		return nil, fmt.Errorf("fuse: reading unit %s: %w", u.Name, err)
	}
	syms, err := f.Symbols()
	if err != nil {
		return nil, nil // a unit with no symtab exports nothing nameable
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range syms {
		if s.Section == elf.SHN_UNDEF || s.Name == "" || seen[s.Name] {
			continue
		}
		if elf.ST_BIND(s.Info) == elf.STB_LOCAL {
			continue
		}
		seen[s.Name] = true
		out = append(out, s.Name)
	}
	return out, nil
}

type bytesReaderAt []byte

func (r bytesReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(r)) {
		return 0, fmt.Errorf("offset %d outside %d bytes", off, len(r))
	}
	return copy(p, r[off:]), nil
}
