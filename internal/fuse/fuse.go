// Package fuse is the build-time dynamic linker: it maps a dynamic executable
// and its shared libraries into one address space, resolves relocations eagerly,
// and emits a single ET_EXEC image that elflift can lift.
//
// RECONSTRUCTED on 2026-08-01. The design was lost with .agents/docs/DYNLINK.md
// but recovered from the `dynplink-poc` bitcode left in .agents-workspace, which
// pins down every externally visible property (see
// .agents/docs/LTM/fusing-relocations-and-ifunc.md, "the recovered
// proof-of-concept bitcode established the intended fused-address model"):
//
//   - one address space, executable and libraries at distinct bases
//   - each library's sections merged under an `.lN` suffix, the executable's
//     keeping their plain names
//   - GOT and .got.plt entries pre-resolved to absolute fused addresses; binding
//     is eager and no runtime loader is involved
//   - synthetic program headers, one small PT_LOAD per merged section
//   - e_entry taken from the executable
//
// elflift needs no changes: the PLT is lifted as ordinary code that loads a
// pre-resolved GOT slot and branches indirectly.
package fuse

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Default layout constants, chosen to match the recovered poc: the executable
// at 0x400000 and libraries above it on 2 MiB boundaries.
const (
	defaultExeBase = 0x400000
	libAlign       = 0x200000
	pageSize       = 0x1000
)

// Object is one ELF mapped into the fused address space.
type Object struct {
	Path string
	// Name is the soname or basename, for diagnostics.
	Name string
	// LibIndex is -1 for the executable and 0.. for libraries, giving the `.lN`
	// section suffix.
	LibIndex int
	// Base is the address this object is mapped at. Zero for a non-PIE
	// executable, which already has absolute vaddrs.
	Base uint64
	// isInterp marks the ELF interpreter. It is fused so the symbols glibc's
	// startup expects to find in it resolve, but it is never entered and its
	// constructors must not be run.
	isInterp bool
	// isPlugin marks an object named DIRECTLY in Options.Extra -- something the
	// guest reaches only through dlopen. It is NOT set on the libraries such an
	// object DT_NEEDEDs: those are ordinary shared libraries, possibly shared
	// with the rest of the closure, and they belong in the library band.
	//
	// The distinction exists because the two are placed differently. See
	// PlanLayout's plugin band.
	isPlugin bool
	// maxAlign is the largest PT_LOAD p_align in the object, i.e. the alignment
	// its own program headers actually require.
	maxAlign uint64

	file    *elf.File
	lo, hi  uint64 // vaddr span covered by image, pre-base
	image   []byte // the object's memory contents, indexed by vaddr-lo
	symbols []elf.Symbol

	// PT_TLS geometry and this object's offset within the fused static TLS
	// block. hasTLS distinguishes "no TLS" from "TLS at offset 0".
	hasTLS                 bool
	tlsMemsz, tlsAlignment uint64
	tlsOffset              uint64
	// tlsVaddr/tlsFilesz locate this object's PT_TLS *template* — the `.tdata`
	// bytes a new thread's block is initialised from. layoutTLS decides where
	// the block lives; these say what to seed it with.
	tlsVaddr, tlsFilesz uint64
}

func (o *Object) suffix() string {
	if o.LibIndex < 0 {
		return ""
	}
	return fmt.Sprintf(".l%d", o.LibIndex)
}

// addr maps a link-time vaddr to its fused address.
func (o *Object) addr(v uint64) uint64 { return v + o.Base }

// read64 loads little-endian data at a link-time vaddr.
func (o *Object) read64(vaddr uint64) (uint64, error) {
	if vaddr < o.lo || vaddr+8 > o.hi {
		return 0, fmt.Errorf("relocation target %#x outside %s image [%#x,%#x)", vaddr, o.Name, o.lo, o.hi)
	}
	return binary.LittleEndian.Uint64(o.image[vaddr-o.lo:]), nil
}

// writeBytes stores raw bytes at a link-time vaddr.
func (o *Object) writeBytes(vaddr uint64, b []byte) error {
	if vaddr < o.lo || vaddr+uint64(len(b)) > o.hi {
		return fmt.Errorf("write target %#x outside %s image [%#x,%#x)", vaddr, o.Name, o.lo, o.hi)
	}
	copy(o.image[vaddr-o.lo:], b)
	return nil
}

// write stores little-endian data at a link-time vaddr.
func (o *Object) write64(vaddr, val uint64) error {
	if vaddr < o.lo || vaddr+8 > o.hi {
		return fmt.Errorf("relocation target %#x outside %s image [%#x,%#x)", vaddr, o.Name, o.lo, o.hi)
	}
	binary.LittleEndian.PutUint64(o.image[vaddr-o.lo:], val)
	return nil
}

// Options controls fusing.
type Options struct {
	// LibraryPaths are searched for DT_NEEDED entries, in order. Typically the
	// guest rootfs's library directories.
	LibraryPaths []string
	// ExeBase is where a PIE executable is mapped; 0 selects the default.
	ExeBase uint64
	// SkipInterpreter omits the ELF interpreter (ld.so) from the fuse. Eager
	// binding means it is never entered, but glibc's startup references symbols
	// that live in it, so it is included by default.
	SkipInterpreter bool
	// Layout fixes every library's base across a closure, so the same library
	// occupies the same addresses in every program's image. Nil keeps the
	// per-image dense packing, which is the default and unchanged. See layout.go.
	Layout *Layout
	// Extra names objects to fuse that NOTHING in the closure DT_NEEDEDs --
	// plugins the program reaches only through dlopen.
	//
	// The DT_NEEDED walk cannot find these by construction, and an AOT closure
	// has no second chance: `dlopen` is intercepted and answers with a sentinel
	// handle, then `dlsym` resolves through `.ecv.dlsyms`, which only lists what
	// was fused. A module left out therefore loads "successfully" and then has
	// every symbol resolve to NULL. postgres reports that as
	//   incompatible library ".../dict_snowball.so": missing magic block
	// which reads like a version mismatch and is really an absent object.
	//
	// Entries may be absolute paths or bare sonames; a name containing a
	// separator is taken as a path, anything else is looked up in LibraryPaths.
	// Their own DT_NEEDED are walked like any other library's.
	Extra []string
}

// Fuse links exePath and its dependencies into a single ET_EXEC image.
//
// Unlike FuseClosure it REFUSES an `Options.Extra` plugin it cannot satisfy,
// rather than skipping it. The asymmetry is deliberate: FuseClosure returns a
// Report and can say what it dropped, this signature cannot, and a silently
// absent dlopen'd module is the exact failure Options.Extra exists to prevent.
func Fuse(exePath string, opts Options) ([]byte, error) {
	objs, skipped, err := load(exePath, opts)
	if err != nil {
		return nil, err
	}
	if len(skipped) > 0 {
		return nil, fmt.Errorf("fuse: cannot satisfy dlopen'd plugin %s: %s",
			skipped[0].Name, skipped[0].Reason)
	}
	if err := layout(objs, opts); err != nil {
		return nil, err
	}
	syms, ifuncSyms := globalSymbols(objs)
	tlsSyms := globalTLSSymbols(objs)
	if err := applyRELR(objs); err != nil {
		return nil, err
	}
	var deferred []ifuncFixup
	var tlsdescs []tlsdescFixup
	// One resolver can back many names and many relocations; interpreting it
	// once per distinct resolver keeps fusing linear, and the export table
	// reuses the same answers.
	resolved := map[uint64]uint64{}
	if err := relocate(objs, syms, ifuncSyms, tlsSyms, &deferred, &tlsdescs, resolved); err != nil {
		return nil, err
	}
	// After relocation, so nothing overwrites the stubs.
	patchDLStubs(objs)
	return emit(objs, deferred, tlsdescs, buildTables(objs, syms, ifuncSyms, resolved))
}

// SkippedExtra names an `Options.Extra` plugin that could not be fused, and
// why. A dlopen'd plugin is OPTIONAL by nature -- on a real system a `dlopen`
// that cannot satisfy the object's dependencies simply fails and the program
// carries on -- so one unsatisfiable plugin must not sink the whole image.
//
// python:3-slim is the case that forced this: it ships 77 extension modules in
// `lib-dynload`, and `_tkinter` DT_NEEDEDs `libtk8.6.so`, which the image does
// not contain. Importing tkinter fails on the real image too.
type SkippedExtra struct {
	Name   string
	Reason string
}

// loadState is the dedup bookkeeping shared by every object walked into one
// image, split out so a plugin subtree can be attempted and rolled back.
type loadState struct {
	objs        []*Object
	seen        map[string]bool
	seenPath    map[string]bool
	seenSoname  map[string]bool
	interpName  string
	libraryPath []string
}

func (st *loadState) snapshot() loadState {
	cp := func(m map[string]bool) map[string]bool {
		out := make(map[string]bool, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	return loadState{
		objs:        append([]*Object(nil), st.objs...),
		seen:        cp(st.seen),
		seenPath:    cp(st.seenPath),
		seenSoname:  cp(st.seenSoname),
		interpName:  st.interpName,
		libraryPath: st.libraryPath,
	}
}

// walk loads `roots` and, transitively, everything they DT_NEEDED.
//
// Three levels of de-duplication, because one library reaches the queue under
// several names. On Alpine `libc.musl-aarch64.so.1` is a SYMLINK to
// `ld-musl-aarch64.so.1`: musl's libc and its interpreter are one file, so
// name-only de-duplication fused it twice at two bases -- two copies of every
// function, and worse, two copies of libc's data, so `errno`, the heap and the
// thread lists all existed twice. A real loader de-duplicates by inode and by
// soname; path and soname are the closest equivalents here.
func (st *loadState) walk(roots []string) error {
	queue := append([]string(nil), roots...)
	for i := 0; i < len(queue); i++ {
		name := queue[i]
		if st.seen[name] {
			continue
		}
		st.seen[name] = true
		path, err := findLib(name, st.libraryPath)
		if err != nil {
			return err
		}
		real := path
		if r, err := filepath.EvalSymlinks(path); err == nil {
			real = r
		}
		if st.seenPath[real] {
			continue
		}
		st.seenPath[real] = true
		lib, err := open(path, len(st.objs)-1)
		if err != nil {
			return err
		}
		if st.seenSoname[lib.Name] {
			// A distinct file claiming a soname already loaded: the first wins,
			// as it would with ld.so.
			continue
		}
		st.seenSoname[lib.Name] = true
		lib.isInterp = name == st.interpName
		st.objs = append(st.objs, lib)
		sub, err := lib.file.DynString(elf.DT_NEEDED)
		if err == nil {
			queue = append(queue, sub...)
		}
	}
	return nil
}

// load resolves the executable and, breadth-first, its DT_NEEDED closure, then
// each `Options.Extra` plugin. Returns the plugins it had to skip; see
// SkippedExtra.
func load(exePath string, opts Options) ([]*Object, []SkippedExtra, error) {
	exe, err := open(exePath, -1)
	if err != nil {
		return nil, nil, err
	}

	var queue []string
	needed, err := exe.file.DynString(elf.DT_NEEDED)
	if err != nil {
		return nil, nil, fmt.Errorf("fuse: reading DT_NEEDED from %s: %w", exePath, err)
	}
	queue = append(queue, needed...)

	st := &loadState{
		objs:        []*Object{exe},
		seen:        map[string]bool{},
		seenPath:    map[string]bool{},
		seenSoname:  map[string]bool{},
		libraryPath: opts.LibraryPaths,
	}
	if !opts.SkipInterpreter {
		// The interpreter is fused like any other library: eager binding means
		// it is never *entered*, but glibc's startup path references symbols
		// that are defined in it.
		if interp := interpreterOf(exe.file); interp != "" {
			st.interpName = filepath.Base(interp)
			queue = append(queue, st.interpName)
		}
	}
	// The MANDATORY closure first. A DT_NEEDED of the executable is not
	// optional: without it the program cannot start at all.
	if err := st.walk(queue); err != nil {
		return nil, nil, err
	}

	// Then each dlopen'd plugin, one at a time and all-or-nothing. Partially
	// fusing a plugin would be worse than dropping it: `dlopen` is intercepted
	// and answers with a sentinel handle, so a half-present module loads
	// "successfully" and then has every symbol resolve to NULL -- the silent
	// failure Options.Extra exists to avoid.
	var skipped []SkippedExtra
	for _, name := range opts.Extra {
		before := st.snapshot()
		added := len(st.objs)
		if err := st.walk([]string{name}); err != nil {
			*st = before
			skipped = append(skipped, SkippedExtra{Name: name, Reason: err.Error()})
			continue
		}
		markPlugin(st.objs[added:], name, st.libraryPath)
	}
	return st.objs, skipped, nil
}

// markPlugin sets isPlugin on the object that IS `name`, among the objects that
// walking it just added.
//
// ⚠️ Deliberately not "everything the walk added". A plugin drags in its own
// DT_NEEDED, and those are ordinary shared libraries -- frequently ones the rest
// of the closure already uses. Marking them would move shared library code into
// the plugin band, which is both wrong and a silent loss of cross-program
// sharing.
//
// Matching is by resolved path rather than by position, because `walk` can skip
// its own root: an object whose soname is already loaded is de-duplicated away,
// and then objs[added] is a DEPENDENCY, not the plugin. Nothing is marked when
// the plugin was already in the closure as a DT_NEEDED library, which is
// correct -- it is not dlopen-only.
func markPlugin(added []*Object, name string, dirs []string) {
	path, err := findLib(name, dirs)
	if err != nil {
		return
	}
	real := path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		real = r
	}
	for _, o := range added {
		op := o.Path
		if r, err := filepath.EvalSymlinks(op); err == nil {
			op = r
		}
		if op == real {
			o.isPlugin = true
			return
		}
	}
}

func interpreterOf(f *elf.File) string {
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		b := make([]byte, p.Filesz)
		if _, err := p.ReadAt(b, 0); err != nil {
			return ""
		}
		return strings.TrimRight(string(b), "\x00")
	}
	return ""
}

func findLib(name string, dirs []string) (string, error) {
	// A name with a separator is a path, not a soname. Options.Extra names
	// dlopen'd plugins, which live outside the library search path -- postgres
	// keeps them in its own $libdir -- so searching `dirs` for them would only
	// ever fail.
	if strings.ContainsRune(name, filepath.Separator) {
		if _, err := os.Stat(name); err == nil {
			return name, nil
		}
		return "", fmt.Errorf("fuse: cannot find %s", name)
	}
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("fuse: cannot find %s in %v", name, dirs)
}

// open reads an ELF and materialises its PT_LOAD contents as one contiguous
// image spanning the object's vaddr range.
func open(path string, libIndex int) (*Object, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("fuse: opening %s: %w", path, err)
	}
	if f.Machine != elf.EM_AARCH64 {
		f.Close()
		return nil, fmt.Errorf("fuse: %s is not aarch64", path)
	}
	o := &Object{Path: path, Name: filepath.Base(path), LibIndex: libIndex, file: f}
	if so, err := f.DynString(elf.DT_SONAME); err == nil && len(so) > 0 {
		o.Name = so[0]
	}

	lo, hi := ^uint64(0), uint64(0)
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		lo = min(lo, p.Vaddr)
		hi = max(hi, p.Vaddr+p.Memsz)
		// The object's own alignment requirement, which is what actually
		// constrains where it may be placed. libAlign (2 MiB) is a fixed
		// constant inherited from the recovered poc, not a measured need:
		// sampled over 2,114 real aarch64 shared objects, every one reports
		// p_align 0x10000. The plugin band uses this instead; see PlanLayout.
		o.maxAlign = max(o.maxAlign, p.Align)
	}
	if hi == 0 {
		f.Close()
		return nil, fmt.Errorf("fuse: %s has no PT_LOAD", path)
	}
	lo = lo &^ (pageSize - 1)
	o.lo, o.hi = lo, hi
	o.image = make([]byte, hi-lo)
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD || p.Filesz == 0 {
			continue
		}
		b := make([]byte, p.Filesz)
		if _, err := p.ReadAt(b, 0); err != nil {
			f.Close()
			return nil, fmt.Errorf("fuse: reading segment of %s: %w", path, err)
		}
		copy(o.image[p.Vaddr-lo:], b)
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_TLS {
			o.hasTLS = true
			o.tlsMemsz = p.Memsz
			o.tlsAlignment = max(p.Align, 1)
			o.tlsVaddr = p.Vaddr
			o.tlsFilesz = p.Filesz
		}
	}
	o.symbols, _ = f.DynamicSymbols()
	return o, nil
}

// layout assigns non-overlapping bases. A non-PIE executable keeps its absolute
// addresses; everything else is placed above it on libAlign boundaries.
func layout(objs []*Object, opts Options) error {
	return assignBases(objs, objs[0].file.Type == elf.ET_DYN, opts)
}

// assignBases places the objects in one address space. Split out from layout so
// it is testable without real ELF files on disk.
func assignBases(objs []*Object, exeIsPIE bool, opts Options) error {
	exe := objs[0]
	next := uint64(0)
	if !exeIsPIE {
		exe.Base = 0
		next = exe.hi
	} else {
		exe.Base = opts.ExeBase
		if exe.Base == 0 {
			exe.Base = defaultExeBase
		}
		next = exe.Base + exe.hi
	}
	for _, o := range objs[1:] {
		// With a closure-wide plan a library keeps one base across every program
		// in the closure, so identical code lifts to identical IR. Absent one,
		// pack densely as before. See layout.go.
		if b, ok := opts.Layout.baseFor(o); ok {
			o.Base = b
			continue
		}
		if opts.Layout != nil {
			return fmt.Errorf("fuse: %s is not in the closure-wide layout", o.Path)
		}
		next = (next + libAlign - 1) &^ (libAlign - 1)
		o.Base = next - o.lo
		next = o.Base + o.hi
	}
	layoutTLS(objs)
	return nil
}

// tcbSize is the aarch64 (TLS variant I) thread control block that sits at the
// thread pointer, with the static TLS blocks laid out immediately above it.
const tcbSize = 16

// tlsDescriptor builds the `.ecv.tls` table the runtime initialises static TLS
// from: one 32-byte entry per object that has thread-locals, holding the fused
// VMA of its template, its filesz and memsz, and its offset from the thread
// pointer. Empty when nothing in the image uses TLS, in which case no section
// is emitted and the runtime's single-PT_TLS fallback still applies.
func tlsDescriptor(objs []*Object) []byte {
	var out []byte
	for _, o := range objs {
		if !o.hasTLS {
			continue
		}
		var e [32]byte
		binary.LittleEndian.PutUint64(e[0:], o.addr(o.tlsVaddr))
		binary.LittleEndian.PutUint64(e[8:], o.tlsFilesz)
		binary.LittleEndian.PutUint64(e[16:], o.tlsMemsz)
		binary.LittleEndian.PutUint64(e[24:], o.tlsOffset)
		out = append(out, e[:]...)
	}
	return out
}

// tlsAlignTable builds `.ecv.tlsalign`: one 8-byte entry per `.ecv.tls` entry,
// in the SAME ORDER, holding that module's PT_TLS p_align.
//
// It is a separate section rather than a fifth field in `.ecv.tls` because the
// runtime derives the entry count as `size / 32`. Widening the entry would make
// every fused ELF already on disk parse as the wrong number of modules, and a
// size that divides by both 32 and 40 (160 bytes, five modules) cannot be
// told apart at all. Additive is the only version-safe shape here; a fused
// image that predates this simply has no such section.
//
// Why the runtime needs it: musl's `struct tls_module` carries `align`, and
// `libc.tls_align` is the maximum over the modules. Both are what
// `__copy_tls` sizes a new thread's TLS block from, so a fused musl image
// cannot create a thread without them -- and the alignment is not recoverable
// from the other four fields. `layoutTLS` above already reads `tlsAlignment`
// to place the blocks; this is the same number, told to the consumer.
func tlsAlignTable(objs []*Object) []byte {
	var out []byte
	for _, o := range objs {
		if !o.hasTLS {
			continue
		}
		out = binary.LittleEndian.AppendUint64(out, max(o.tlsAlignment, 1))
	}
	return out
}

// symbolsOf returns the object's symbol table, falling back to .dynsym for a
// stripped library. The merged `.symtab` is built from this.
//
// ⚠️ It used to feed `.ecv.funcs` as well, "so they cannot disagree about which
// symbols exist". That section was removed on 2026-08-25 once measurement showed
// it carried nothing the `.symtab` did not -- which is unsurprising in
// retrospect, since sharing this function is exactly what guaranteed it.
func symbolsOf(o *Object) []elf.Symbol {
	if o.file != nil {
		if src, err := o.file.Symbols(); err == nil && len(src) > 0 {
			return src
		}
	}
	return o.symbols
}

// funcRangesOf returns every function boundary fusing knows for one object, in
// that object's LINK-TIME addresses.
//
// `own` are the object's own sized function symbols. `extra` is everything
// recovered on top: a stripped library exports only a fraction of its functions,
// so the rest are recovered from `.eh_frame` (ehframe.go), from init stubs, and
// finally from computed code pointers (funcptr.go) — which must come last
// because it needs every other boundary to decide what is genuinely
// undiscovered. Entries in `extra` that a real symbol already bounds are the
// caller's to drop, via covered().
//
// ❗ THE MERGED `.symtab` IS BUILT FROM THIS, and that is now the only consumer.
// `.ecv.funcs` was the other until 2026-08-25. The two once computed the
// inventory separately and disagreed -- `.ecv.funcs` listed 4,047 functions for
// a fused bash where the symbol table had 6,578, because it saw only `own`, and
// as a keep-list that would have silently dropped a third of the program.
// Unifying them here fixed that, and thereby made the second copy redundant:
// measured across three fused images, every address it emitted was in the
// symtab with an identical size. It was removed rather than kept as a duplicate
// of a table the lifter already reads.
func funcRangesOf(o *Object) (own []funcRange, extra []funcRange) {
	for _, s := range symbolsOf(o) {
		if s.Name == "" || s.Section == elf.SHN_UNDEF {
			continue
		}
		if elf.ST_TYPE(s.Info) == elf.STT_FUNC && s.Size > 0 {
			own = append(own, funcRange{addr: s.Value, size: s.Size})
		}
	}
	sort.Slice(own, func(i, j int) bool { return own[i].addr < own[j].addr })

	// Recovery needs the backing ELF. An Object without one carries only the
	// symbols it was given, which is the case in unit tests and for anything
	// synthesised rather than loaded from disk.
	if o.file == nil {
		return own, nil
	}
	extra = append(ehFrameFuncs(o.file), initStubs(o)...)
	ranges := execRanges(o)
	// Relocated pointers first: their evidence is the strongest of the three
	// recovered sources, so a boundary they establish should bound the weaker
	// ones rather than the other way round.
	extra = append(extra, relocPointerFuncs(o.file, ranges,
		append(append([]funcRange(nil), own...), extra...))...)
	extra = append(extra, codePointerFuncs(ranges,
		append(append([]funcRange(nil), own...), extra...))...)
	return own, extra
}

// layoutTLS assigns each object a slot in the fused static TLS block. A static
// linker does exactly this, which is why a statically linked binary has no
// leftover TPREL relocations; fusing has to reproduce it.
func layoutTLS(objs []*Object) {
	off := uint64(tcbSize)
	for _, o := range objs {
		if !o.hasTLS {
			continue
		}
		align := o.tlsAlignment
		off = (off + align - 1) &^ (align - 1)
		o.tlsOffset = off
		off += o.tlsMemsz
	}
}

// globalTLSSymbols maps a TLS symbol name to its offset from the thread
// pointer in the fused static TLS block.
func globalTLSSymbols(objs []*Object) map[string]uint64 {
	out := map[string]uint64{}
	for _, o := range objs {
		if !o.hasTLS {
			continue
		}
		for _, s := range o.symbols {
			if s.Name == "" || s.Section == elf.SHN_UNDEF || elf.ST_TYPE(s.Info) != elf.STT_TLS {
				continue
			}
			if _, dup := out[s.Name]; dup {
				continue
			}
			out[s.Name] = o.tlsOffset + s.Value
		}
	}
	return out
}

// globalSymbols builds the resolution table. Definition order is executable
// first, then libraries in load order, which is the interposition order the
// dynamic loader would use.
// sttGNUIFunc is STT_GNU_IFUNC. debug/elf does not name it (it collides with
// STT_LOOS), but the distinction is critical: for an ifunc symbol the symbol
// VALUE is a resolver to be called, not the implementation. Binding the value
// directly makes every cross-object call to that name invoke the resolver,
// which returns a function pointer and does nothing else -- a silent no-op.
const sttGNUIFunc = elf.SymType(10)

// ifuncFixup is an ifunc relocation fuse could not evaluate at build time. The
// runtime resolves these from `.ecv.irela` by running the resolver as real
// guest code (context.rs apply_ifuncs), which has a stack and an
// `__ifunc_arg_t` and so copes with resolvers that dereference their argument.
type ifuncFixup struct{ slot, resolver uint64 }

// tlsdescFixup records an R_AARCH64_TLSDESC site. Unlike every other relocation
// here it cannot be written during `relocate`, because it needs the address of
// the resolver stub and that is only assigned during `emit` -- the same reason
// ifunc fixups are deferred.
type tlsdescFixup struct {
	o     *Object
	off   uint64 // link-time offset of the descriptor's FIRST word
	tpOff uint64 // value for the second word: offset from the thread pointer
}

// tlsdescStub is `ldr x0, [x0, #8]` followed by `ret`.
//
// This is glibc's `_dl_tlsdesc_return` instruction for instruction, and it is
// the whole of what a TLSDESC resolver has to do once the TLS layout is static:
// x0 arrives holding the descriptor's own address, the offset was written into
// the second word at fuse time, so load it and return. TLSDESC resolvers must
// preserve every register except x0, which this trivially does.
//
// The shipped `ld-linux-aarch64.so.1` contains this exact sequence exactly once,
// but it is stripped to 43 `.dynsym` entries and the symbol is local, so there
// is nothing to bind to by name. Emitting our own is both shorter and honest --
// matching an unnamed byte pattern in someone else'"'"'s library is the kind of
// hand-derived match that has been wrong here before.
var tlsdescStub = []byte{
	0x00, 0x04, 0x40, 0xf9, // ldr x0, [x0, #8]
	0xc0, 0x03, 0x5f, 0xd6, // ret
}

// applyTLSDescFixups writes each descriptor: the resolver stub's address into
// the first word and the thread-pointer offset into the second, which is the
// layout the guest's `blr` through slot 0 expects. Split out of `emit` so the
// ordering of the two words is directly testable -- swapping them produces an
// image that fuses and links and then jumps to a small integer.
func applyTLSDescFixups(fixups []tlsdescFixup, stubAddr uint64) error {
	for _, f := range fixups {
		if err := f.o.write64(f.off, stubAddr); err != nil {
			return fmt.Errorf("fuse: TLSDESC resolver slot at %#x: %w", f.off, err)
		}
		if err := f.o.write64(f.off+8, f.tpOff); err != nil {
			return fmt.Errorf("fuse: TLSDESC argument slot at %#x: %w", f.off+8, err)
		}
	}
	return nil
}

// tlsdescStubSym names the emitted stub. elflift discovers functions from the
// symbol table, so without a symbol the stub is never lifted and the guest'"'"'s
// `blr` lands on nothing.
const tlsdescStubSym = "_ecv_tlsdesc_return"

// irelaDescriptor serialises the deferred fixups into the `.ecv.irela` table
// the runtime consumes: 16 bytes per entry, GOT slot then resolver, both fused
// VMAs, little-endian. Must match apply_ifuncs in runtime/src/context.rs.
func irelaDescriptor(fixups []ifuncFixup) []byte {
	var b []byte
	for _, f := range fixups {
		b = binary.LittleEndian.AppendUint64(b, f.slot)
		b = binary.LittleEndian.AppendUint64(b, f.resolver)
	}
	return b
}

// isPointerSlot reports whether a relocation writes nothing but an 8-byte
// address, so the runtime can overwrite it wholesale from `.ecv.irela`.
func isPointerSlot(typ elf.R_AARCH64, addend int64) bool {
	switch typ {
	case elf.R_AARCH64_GLOB_DAT, elf.R_AARCH64_JUMP_SLOT:
		return true
	case elf.R_AARCH64_ABS64:
		return addend == 0
	}
	return false
}

// ❗ FIRST-WINS, EXCEPT THAT A DEFAULT VERSION OUTRANKS A HIDDEN ONE.
//
// Symbol versions are discarded here: `o.symbols` is `f.DynamicSymbols()`, whose
// `Name` is bare, so `exp@GLIBC_2.17` and `exp@GLIBC_2.29` are one key. That is
// deliberate and stays -- every reference in the image is resolved eagerly by
// name, and there is no runtime linker to consult a version table.
//
// ⚠️ But plain first-wins picks by `.dynsym` ORDER, and glibc lists the COMPAT
// definition first. Measured 2026-08-26 on the real postgres:17 closure: 67
// names resolve to more than one implementation, 32 of them bound the oldest,
// and THREE were actively wrong -- something referenced them at a newer version:
//
//	exp    referenced @GLIBC_2.29 by postgres, libLLVM, libz3 -> bound @GLIBC_2.17
//	fmod   referenced @GLIBC_2.38 by postgres, libLLVM        -> bound @GLIBC_2.17
//	log2f  referenced @GLIBC_2.27 by libLLVM                  -> bound @GLIBC_2.17
//
// `glob` was the tell: `glob` and `glob64` are ALIASES of the same two
// addresses, and `.dynsym` happens to list the compat one first for `glob` and
// the default one first for `glob64`, so one fused image gave the same pair of
// functions two different implementations.
//
// The ELF rule is exact, not a heuristic: a version index carries a hidden bit,
// and the definition WITHOUT it is the default -- what `readelf` prints as
// `foo@@VER` versus a compat `foo@VER`. `debug/elf` exposes it as
// `VersionIndex.IsHidden()` (Go 1.26).
//
// ⚠️ UNVERSIONED SYMBOLS ARE TREATED AS DEFAULT, which is what keeps this from
// changing anything outside glibc. musl has no symbol versioning at all and
// neither do the plugins, so on those inputs `hidden` is false everywhere and
// this is exactly the old first-wins. Measured: postgres's 79 extensions have 0
// versioned names, so the per-unit path is untouched.
//
// ❌ This does NOT implement version-aware resolution. Two DEFAULT definitions
// of one name across objects still resolve first-wins, which is the correct
// interposition order. All this does is refuse to prefer a compat definition
// over the default one in the same object.
func globalSymbols(objs []*Object) (map[string]uint64, map[string]bool) {
	out := map[string]uint64{}
	ifuncs := map[string]bool{}
	// hiddenVer records whether the definition currently held for a name came
	// from a non-default (compat) version, so a later default one can replace
	// it. Tracked separately because `out` holds only the address.
	hiddenVer := map[string]bool{}
	for _, o := range objs {
		for _, s := range o.symbols {
			if s.Section == elf.SHN_UNDEF || s.Name == "" {
				continue
			}
			hidden := s.HasVersion && s.VersionIndex.IsHidden()
			if _, dup := out[s.Name]; dup {
				// Held definition is fine unless it is a compat version and
				// this one is the default. Note the asymmetry: a default is
				// never replaced, so ordinary interposition still wins.
				if !hiddenVer[s.Name] || hidden {
					continue
				}
			}
			if elf.ST_TYPE(s.Info) == sttGNUIFunc {
				ifuncs[s.Name] = true
			} else {
				// An earlier compat definition may have been an ifunc while the
				// default is not. Clear it, or the runtime would treat the
				// address as a resolver to call -- a silent no-op, per the
				// sttGNUIFunc note above.
				delete(ifuncs, s.Name)
			}
			out[s.Name] = o.addr(s.Value)
			hiddenVer[s.Name] = hidden
		}
	}
	return out, ifuncs
}

// relocate applies every relocation in every object, writing resolved absolute
// addresses into the images. This is what makes the GOT pre-resolved.
// shtRELR is SHT_RELR. debug/elf still has no constant for it as of Go 1.26,
// which is a large part of why this pass was missing: a section type nothing
// names is easy to walk straight past.
const shtRELR = elf.SectionType(19)

// applyRELR processes packed relative relocations (`.relr.dyn` / DT_RELR).
//
// Debian's glibc is linked with -z pack-relative-relocs, so nearly all of its
// R_AARCH64_RELATIVE relocations live here rather than in `.rela.dyn` -- 34
// RELR words standing in for thousands of entries. Skipping the section is
// silent: the image still fuses and still looks complete, but every relocated
// data pointer keeps its link-time value. `_IO_file_jumps` was the one that
// surfaced it, with all 21 vtable slots holding raw libc offsets, so the first
// fclose jumped to 0x7e1c0.
//
// The encoding alternates between addresses and bitmaps. An even word is an
// address: relocate there, then step one word on. An odd word is a bitmap of
// the next 63 words, bit i standing for the word at where+(i-1)*8; afterwards
// the cursor advances past all 63 whether or not their bits were set. Unlike
// RELA there is no explicit addend -- the addend is the value already stored,
// so each site is read, rebased and written back.
func applyRELR(objs []*Object) error {
	for _, o := range objs {
		for _, sec := range o.file.Sections {
			if sec.Type != shtRELR {
				continue
			}
			data, err := sec.Data()
			if err != nil {
				return fmt.Errorf("fuse: reading %s of %s: %w", sec.Name, o.Name, err)
			}
			apply := func(vaddr uint64) error {
				cur, err := o.read64(vaddr)
				if err != nil {
					return fmt.Errorf("fuse: %s %s: %w", o.Name, sec.Name, err)
				}
				return o.write64(vaddr, cur+o.Base)
			}
			if err := walkRELR(data, apply); err != nil {
				return err
			}
		}
	}
	return nil
}

// walkRELR decodes the RELR stream, calling apply at each relocated link-time
// vaddr. Separated from the I/O so the encoding can be tested directly.
func walkRELR(data []byte, apply func(uint64) error) error {
	var where uint64
	for off := 0; off+8 <= len(data); off += 8 {
		e := binary.LittleEndian.Uint64(data[off:])
		if e&1 == 0 {
			where = e
			if err := apply(where); err != nil {
				return err
			}
			where += 8
			continue
		}
		for i := 1; i < 64; i++ {
			if e&(1<<uint(i)) != 0 {
				if err := apply(where + uint64(i-1)*8); err != nil {
					return err
				}
			}
		}
		where += 63 * 8
	}
	return nil
}

func relocate(objs []*Object, syms map[string]uint64, ifuncSyms map[string]bool, tlsSyms map[string]uint64, deferred *[]ifuncFixup, tlsdescs *[]tlsdescFixup, resolved map[uint64]uint64) error {
	var unsupported []string
	for _, o := range objs {
		for _, sec := range o.file.Sections {
			if sec.Type != elf.SHT_RELA {
				continue
			}
			data, err := sec.Data()
			if err != nil {
				return fmt.Errorf("fuse: reading %s of %s: %w", sec.Name, o.Name, err)
			}
			for off := 0; off+24 <= len(data); off += 24 {
				rOff := binary.LittleEndian.Uint64(data[off:])
				rInfo := binary.LittleEndian.Uint64(data[off+8:])
				rAdd := int64(binary.LittleEndian.Uint64(data[off+16:]))
				typ := elf.R_AARCH64(rInfo & 0xffffffff)
				symIdx := int(rInfo >> 32)

				var symName string
				var symVal uint64
				var haveSym bool
				var symIsIfunc bool
				if symIdx > 0 && symIdx-1 < len(o.symbols) {
					s := o.symbols[symIdx-1]
					symName = s.Name
					if v, ok := syms[s.Name]; ok {
						symVal, haveSym = v, true
					} else if s.Section != elf.SHN_UNDEF {
						symVal, haveSym = o.addr(s.Value), true
					}
					// The reference site says nothing about ifunc-ness -- an
					// undefined reference to `memset` looks like a plain FUNC.
					// Only the DEFINITION carries STT_GNU_IFUNC, so consult the
					// global table, falling back to a local definition's type.
					symIsIfunc = ifuncSyms[s.Name] || elf.ST_TYPE(s.Info) == sttGNUIFunc
				}
				// Run the resolver so the slot gets the implementation. What must
				// never happen is the resolver address surviving into the slot:
				// that is not a crash but a no-op call, and it surfaces much
				// later as unexplained garbage (it cost a long hunt through
				// OpenSSL's RCU before landing here).
				if symIsIfunc && haveSym {
					if impl, ok := resolved[symVal]; ok {
						symVal = impl
					} else if impl, err := resolveIfunc(objs, symVal); err == nil {
						resolved[symVal] = impl
						symVal = impl
					} else if isPointerSlot(typ, rAdd) {
						// Not evaluatable here -- typically the resolver
						// dereferences its `__ifunc_arg_t`, which does not exist
						// until there is a real call frame. Hand it to the
						// runtime, which runs resolvers as lifted guest code.
						// The resolver address written below is a placeholder
						// apply_ifuncs overwrites before the guest starts.
						*deferred = append(*deferred, ifuncFixup{slot: o.addr(rOff), resolver: symVal})
					} else {
						// Deferral only works for a bare 8-byte pointer slot;
						// anything else the runtime would clobber, so refuse.
						unsupported = append(unsupported,
							fmt.Sprintf("%s: ifunc symbol %q (resolver %#x) via %v addend %d: %v",
								o.Name, symName, symVal, typ, rAdd, err))
						continue
					}
				}

				var val uint64
				switch typ {
				case elf.R_AARCH64_RELATIVE:
					val = uint64(int64(o.Base) + rAdd)
				case elf.R_AARCH64_GLOB_DAT, elf.R_AARCH64_JUMP_SLOT:
					if !haveSym {
						// A weak undefined symbol legitimately resolves to 0.
						val = 0
					} else {
						val = symVal
					}
				case elf.R_AARCH64_ABS64:
					if !haveSym && symName != "" {
						val = uint64(rAdd)
					} else {
						val = uint64(int64(symVal) + rAdd)
					}
				case elf.R_AARCH64_IRELATIVE:
					// The addend is the resolver's link-time address; interpret
					// it over the fused image. cpu_features reads as zero there,
					// so this selects the baseline implementation — see ifunc.go.
					sel, err := resolveIfunc(objs, o.addr(uint64(rAdd)))
					if err != nil {
						unsupported = append(unsupported,
							fmt.Sprintf("%s: IRELATIVE at %#x (resolver %#x): %v", o.Name, rOff, uint64(rAdd), err))
						continue
					}
					val = sel
				case elf.R_AARCH64_TLSDESC:
					// A descriptor, not a value: two consecutive words holding a
					// resolver function and its argument, which the guest reaches
					// with `blr`. The argument is the same thread-pointer offset
					// TPREL64 computes below; the resolver address is not known
					// until `emit` places the stub, so record and defer.
					var tp uint64
					if off, ok := tlsSyms[symName]; ok {
						tp = uint64(int64(off) + rAdd)
					} else if o.hasTLS {
						tp = uint64(int64(o.tlsOffset) + rAdd)
					} else {
						unsupported = append(unsupported,
							fmt.Sprintf("%s: TLSDESC at %#x references unknown TLS symbol %q", o.Name, rOff, symName))
						continue
					}
					*tlsdescs = append(*tlsdescs, tlsdescFixup{o: o, off: rOff, tpOff: tp})
					continue
				case elf.R_AARCH64_TLS_TPREL64:
					// Offset from the thread pointer in the fused static TLS
					// block. Resolved against the *defining* object's slot, so
					// a reference from one object to another's TLS variable
					// lands correctly.
					if off, ok := tlsSyms[symName]; ok {
						val = uint64(int64(off) + rAdd)
					} else if o.hasTLS {
						val = uint64(int64(o.tlsOffset) + rAdd)
					} else {
						unsupported = append(unsupported,
							fmt.Sprintf("%s: TPREL64 at %#x references unknown TLS symbol %q", o.Name, rOff, symName))
						continue
					}
				default:
					if isTLS(typ) {
						unsupported = append(unsupported,
							fmt.Sprintf("%s: %v at %#x (needs a static TLS layout)", o.Name, typ, rOff))
						continue
					}
					unsupported = append(unsupported,
						fmt.Sprintf("%s: unhandled %v at %#x", o.Name, typ, rOff))
					continue
				}
				if err := o.write64(rOff, val); err != nil {
					return err
				}
			}
		}
	}
	if len(unsupported) > 0 {
		return &UnsupportedError{Relocations: unsupported}
	}
	return nil
}

func isTLS(t elf.R_AARCH64) bool {
	return strings.Contains(t.String(), "TLS") || strings.Contains(t.String(), "TPREL") ||
		strings.Contains(t.String(), "DTPMOD") || strings.Contains(t.String(), "DTPREL")
}

// UnsupportedError reports relocations the fuser cannot resolve statically. It
// is deliberately a distinct type: hitting it means a policy decision is needed
// (ifunc resolution, TLS layout), not that the input is malformed.
type UnsupportedError struct {
	Relocations []string
}

func (e *UnsupportedError) Error() string {
	n := len(e.Relocations)
	shown := e.Relocations
	if n > 12 {
		shown = shown[:12]
	}
	return fmt.Sprintf("fuse: %d relocation(s) need a policy decision:\n  %s",
		n, strings.Join(shown, "\n  "))
}

// section is one merged, allocatable section in the output.
type section struct {
	name    string
	addr    uint64
	data    []byte
	nobits  bool
	memsz   uint64
	flags   elf.SectionFlag
	secType elf.SectionType
}

// emit writes the fused ET_EXEC: section headers for every allocatable input
// section (libraries suffixed `.lN`), and one PT_LOAD per section.
func emit(objs []*Object, deferredIfuncs []ifuncFixup, tlsdescs []tlsdescFixup, tables bringupTables) ([]byte, error) {
	var secs []section
	for _, o := range objs {
		for _, s := range o.file.Sections {
			if s.Flags&elf.SHF_ALLOC == 0 || s.Size == 0 || s.Name == "" {
				continue
			}
			// Emit only content sections. Dynamic-linking metadata (.dynsym,
			// .dynamic, .gnu.hash, .rela.*, version sections, .interp) is dead
			// weight once binding is eager, and carrying it produces an ELF that
			// BFD — which elflift loads through — rejects outright with "does
			// not look like an executable file". The poc's section list confirms
			// the original fuser dropped it too: it contains .got/.data.l0/
			// .eh_frame.l0 and no dynamic metadata at all.
			switch s.Type {
			case elf.SHT_PROGBITS, elf.SHT_NOBITS,
				elf.SHT_INIT_ARRAY, elf.SHT_FINI_ARRAY, elf.SHT_PREINIT_ARRAY:
			default:
				continue
			}
			if s.Name == ".interp" {
				continue
			}
			sec := section{
				name:    s.Name + o.suffix(),
				addr:    o.addr(s.Addr),
				memsz:   s.Size,
				flags:   s.Flags,
				secType: s.Type,
			}
			if s.Type == elf.SHT_NOBITS {
				sec.nobits = true
			} else {
				if s.Addr < o.lo || s.Addr+s.Size > o.hi {
					continue
				}
				// Take contents from the relocated image, not the file, so
				// resolved GOT entries are what land in the output.
				sec.data = o.image[s.Addr-o.lo : s.Addr-o.lo+s.Size]
			}
			secs = append(secs, sec)
		}
	}
	// The static-TLS descriptor table. A fused image has one PT_TLS per object
	// that has thread-locals, but an ELF may advertise only ONE, so the merged
	// image advertises none and the runtime has nothing to initialise from:
	// `setup_tls` looks for this table first and falls back to a single
	// `tls_phdr()`. Without it every `__thread` variable in every object reads
	// whatever the arena happened to hold and no `.tdata` initial value is ever
	// applied -- a silent wrong-data failure, not a crash.
	//
	// Format is fixed by runtime/src/context.rs setup_tls: 32 bytes per module,
	// four little-endian u64s — template VMA, filesz, memsz, offset from the
	// thread pointer — consumed by arena.rs init_tls_module.
	// Each ecvisor-private table goes in its own page past every mapped
	// section, so adding one can never disturb the guest's own layout.
	addTable := func(name string, b []byte) {
		if len(b) == 0 {
			return
		}
		var top uint64
		for _, s := range secs {
			if end := s.addr + s.memsz; end > top {
				top = end
			}
		}
		top = (top + 0xfff) &^ 0xfff // page-align, past every mapped section
		secs = append(secs, section{
			name:    name,
			addr:    top,
			memsz:   uint64(len(b)),
			flags:   elf.SHF_ALLOC,
			secType: elf.SHT_PROGBITS,
			data:    b,
		})
	}
	// The TLSDESC resolver stub, placed like the tables (its own page past
	// everything mapped) but executable, then the descriptors filled in. This
	// has to happen before `secs` is sorted, so the symbol emitted below finds
	// its section index by the same address lookup every other symbol uses.
	var tlsdescStubAddr uint64
	if len(tlsdescs) > 0 {
		var top uint64
		for _, s := range secs {
			if end := s.addr + s.memsz; end > top {
				top = end
			}
		}
		tlsdescStubAddr = (top + 0xfff) &^ 0xfff
		secs = append(secs, section{
			name:    ".ecv.tlsdesc",
			addr:    tlsdescStubAddr,
			memsz:   uint64(len(tlsdescStub)),
			flags:   elf.SHF_ALLOC | elf.SHF_EXECINSTR,
			secType: elf.SHT_PROGBITS,
			data:    tlsdescStub,
		})
		// Section data aliases each object's image, so patching now still lands
		// in the output.
		if err := applyTLSDescFixups(tlsdescs, tlsdescStubAddr); err != nil {
			return nil, err
		}
	}
	addTable(".ecv.irela", irelaDescriptor(deferredIfuncs))
	addTable(".ecv.tls", tlsDescriptor(objs))
	addTable(".ecv.tlsalign", tlsAlignTable(objs))
	addTable(".ecv.early", tables.early)
	addTable(".ecv.init", tables.initArray)
	addTable(".ecv.stacklists", tables.stacklists)
	addTable(".ecv.dlsyms", tables.dlsyms)
	addTable(".ecv.musltp", tables.muslTP)

	sort.SliceStable(secs, func(i, j int) bool { return secs[i].addr < secs[j].addr })

	// elflift discovers functions from the symbol table and fails with
	// "entry_function is not found" without one. The poc's lifted names —
	// _start_____0_4002a0, foo_____2_600238 — are symbol name, index and fused
	// address, so the original fuser emitted a merged table too. Symbols are
	// rebased into the fused address space here.
	type fsym struct {
		name  string
		value uint64
		size  uint64
		info  uint8
		shndx uint16
	}
	var fsyms []fsym
	seenSym := map[string]bool{}
	for _, o := range objs {
		src := symbolsOf(o)
		// The object's own sized functions, in its link-time addresses, so an
		// FDE that merely re-describes a function the symbol table already
		// bounds does not add a second, competing boundary. Via funcRangesOf,
		// which used to be shared with `.ecv.funcs` so the two inventories could
		// not diverge -- that section is gone (2026-08-25) and this is now the
		// sole inventory the lifter receives.
		ownFuncs, extraFuncs := funcRangesOf(o)
		for _, s := range src {
			if s.Name == "" || s.Section == elf.SHN_UNDEF {
				continue
			}
			switch elf.ST_TYPE(s.Info) {
			case elf.STT_FUNC, elf.STT_OBJECT, elf.STT_NOTYPE:
			default:
				continue
			}
			addr := o.addr(s.Value)
			key := fmt.Sprintf("%s@%#x", s.Name, addr)
			if seenSym[key] {
				continue
			}
			seenSym[key] = true
			shndx := uint16(elf.SHN_ABS)
			for i, sec := range secs {
				if addr >= sec.addr && addr < sec.addr+sec.memsz {
					shndx = uint16(i + 1) // section headers are 1-based after the null entry
					break
				}
			}
			fsyms = append(fsyms, fsym{s.Name, addr, s.Size, s.Info, shndx})
		}

		// Fill the gaps left by stripping from `.eh_frame` (see ehframe.go).
		// A stripped library exports only a fraction of its functions, and
		// elflift needs a symbol for every one of them or it disassembles the
		// unsymbolised remainder as one span and eventually decodes data.
		for _, fr := range extraFuncs {
			if covered(ownFuncs, fr.addr) {
				continue // a real symbol already bounds this function
			}
			addr := o.addr(fr.addr)
			name := fmt.Sprintf("_ecv_fde_%x", addr)
			key := fmt.Sprintf("%s@%#x", name, addr)
			if seenSym[key] {
				continue
			}
			seenSym[key] = true
			shndx := uint16(elf.SHN_ABS)
			for i, sec := range secs {
				if addr >= sec.addr && addr < sec.addr+sec.memsz {
					shndx = uint16(i + 1)
					break
				}
			}
			fsyms = append(fsyms, fsym{
				name, addr, fr.size,
				uint8(elf.ST_INFO(elf.STB_LOCAL, elf.STT_FUNC)), shndx,
			})
		}
	}
	sort.SliceStable(fsyms, func(i, j int) bool { return fsyms[i].value < fsyms[j].value })

	// Name the TLSDESC stub so elflift lifts it. Placed with the other synthetic
	// symbols and resolved to a section index the same way.
	if tlsdescStubAddr != 0 {
		shndx := uint16(elf.SHN_ABS)
		for i, sec := range secs {
			if tlsdescStubAddr >= sec.addr && tlsdescStubAddr < sec.addr+sec.memsz {
				shndx = uint16(i + 1)
				break
			}
		}
		fsyms = append(fsyms, fsym{
			tlsdescStubSym, tlsdescStubAddr, uint64(len(tlsdescStub)),
			uint8(elf.ST_INFO(elf.STB_GLOBAL, elf.STT_FUNC)), shndx,
		})
	}

	// The executable's entry must be a named function or elflift cannot find it.
	entry := objs[0].addr(objs[0].file.Entry)
	hasEntry := false
	for _, s := range fsyms {
		if s.value == entry && elf.ST_TYPE(s.info) == elf.STT_FUNC {
			hasEntry = true
			break
		}
	}
	if !hasEntry {
		shndx := uint16(elf.SHN_ABS)
		for i, sec := range secs {
			if entry >= sec.addr && entry < sec.addr+sec.memsz {
				shndx = uint16(i + 1)
				break
			}
		}
		// ❗ ONLY IF THE ENTRY IS ACTUALLY IN A SECTION.
		//
		// A symbol at an address no section covers is not a function, and
		// naming it one CRASHES THE LIFTER. `TraceManager.cpp:332` sizes a
		// zero-sized symbol from its section:
		//
		//	func_size = bfd_section_vma(func_symbols[i].in_section) + ...
		//
		// and for an `SHN_ABS` symbol BFD hands back a NULL section, so that
		// line dereferences null. Confirmed under gdb:
		//	#0 bfd_section_vma (sec=0x0) at /usr/include/bfd.h:1202
		//	#1 AArch64TraceManager::SetELFData at TraceManager.cpp:332
		//
		// An EXECUTABLE always has its entry inside `.text`, so this never
		// fired before. A UNIT image is a shared object, whose `e_entry` is 0
		// and therefore rebases to the image BASE -- below every section. The
		// unit then carried `_start` at `Ndx ABS` with size 0 and every lift of
		// it died with elfconv's "(Custom) Segmantation Fault." and no
		// diagnostic at all.
		//
		// Emitting nothing is right rather than merely safe: a library HAS no
		// entry point, so there is no function to name. elflift's own entry
		// search then reports "entry_function is not found", which is a
		// diagnosis instead of a crash.
		if shndx != uint16(elf.SHN_ABS) {
			fsyms = append(fsyms, fsym{"_start", entry, 0, uint8(elf.ST_INFO(elf.STB_GLOBAL, elf.STT_FUNC)), shndx})
		} else {
			// The entry names nothing and lies in no section, so it cannot be
			// made to name anything. Point the image at a REAL function
			// instead: the lowest-addressed sized one.
			//
			// elflift REQUIRES an entry function -- `TraceManager.cpp` sets
			// `entry_func_lifted_name` only for the symbol whose address equals
			// `e_entry`, and refuses the image outright when none does
			// ("[ERROR] entry_function is not found."). A shared object has no
			// entry, so a UNIT image built from one would always be refused.
			//
			// ⚠️ Choosing arbitrarily is sound HERE and would not be for a
			// program. A unit is never an exec target -- it has no exec-map
			// path, so `Programs::resolve` can never select it -- and the
			// runtime enters its code only through `dlsym` and the constructors
			// in `.ecv.init`. `EcvProgram.entry_func` for a unit is therefore
			// never called, and its only job is to give the lifter a place to
			// start discovery from.
			//
			// Lowest-addressed rather than first-listed so the choice is
			// deterministic: `fsyms` order follows the symbol tables, which is
			// not a property this should depend on.
			best := uint64(0)
			for _, f := range fsyms {
				if elf.ST_TYPE(f.info) != elf.STT_FUNC || f.size == 0 {
					continue
				}
				if best == 0 || f.value < best {
					best = f.value
				}
			}
			if best != 0 {
				entry = best
			}
		}
	}

	var strtab []byte
	strtab = append(strtab, 0)
	symtab := make([]byte, 24) // index 0 is the null symbol
	for _, s := range fsyms {
		nameOff := uint32(len(strtab))
		strtab = append(strtab, s.name...)
		strtab = append(strtab, 0)
		var e [24]byte
		binary.LittleEndian.PutUint32(e[0:], nameOff)
		e[4] = s.info
		binary.LittleEndian.PutUint16(e[6:], s.shndx)
		binary.LittleEndian.PutUint64(e[8:], s.value)
		binary.LittleEndian.PutUint64(e[16:], s.size)
		symtab = append(symtab, e[:]...)
	}

	// Layout: ehdr, phdrs, section contents, shstrtab, shdrs.
	const ehdrSize, phdrSize, shdrSize = 64, 56, 64
	nph := len(secs)
	off := uint64(ehdrSize + nph*phdrSize)

	offsets := make([]uint64, len(secs))
	for i := range secs {
		if secs[i].nobits {
			offsets[i] = off
			continue
		}
		off = (off + 15) &^ 15
		offsets[i] = off
		off += uint64(len(secs[i].data))
	}

	var shstr []byte
	shstr = append(shstr, 0)
	nameOff := make([]uint32, len(secs))
	for i, s := range secs {
		nameOff[i] = uint32(len(shstr))
		shstr = append(shstr, s.name...)
		shstr = append(shstr, 0)
	}
	symtabNameOff := uint32(len(shstr))
	shstr = append(shstr, ".symtab\x00"...)
	strtabNameOff := uint32(len(shstr))
	shstr = append(shstr, ".strtab\x00"...)
	shstrNameOff := uint32(len(shstr))
	shstr = append(shstr, ".shstrtab\x00"...)

	// .symtab and .strtab are not SHF_ALLOC, so they need file space but no
	// PT_LOAD.
	off = (off + 15) &^ 15
	symtabOff := off
	off += uint64(len(symtab))
	off = (off + 15) &^ 15
	strtabOff := off
	off += uint64(len(strtab))

	off = (off + 15) &^ 15
	shstrOff := off
	off += uint64(len(shstr))
	off = (off + 15) &^ 15
	shoff := off

	// null + sections + .symtab + .strtab + .shstrtab
	nsh := len(secs) + 4
	total := shoff + uint64(nsh*shdrSize)
	out := make([]byte, total)

	// Ehdr
	copy(out, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0})
	le := binary.LittleEndian
	le.PutUint16(out[16:], uint16(elf.ET_EXEC))
	le.PutUint16(out[18:], uint16(elf.EM_AARCH64))
	le.PutUint32(out[20:], 1)
	le.PutUint64(out[24:], entry)    // e_entry (see the entry-naming block above)
	le.PutUint64(out[32:], ehdrSize) // e_phoff
	le.PutUint64(out[40:], shoff)    // e_shoff
	le.PutUint16(out[52:], ehdrSize)
	le.PutUint16(out[54:], phdrSize)
	le.PutUint16(out[56:], uint16(nph))
	le.PutUint16(out[58:], shdrSize)
	le.PutUint16(out[60:], uint16(nsh))
	le.PutUint16(out[62:], uint16(len(secs)+3)) // e_shstrndx

	// Phdrs: one PT_LOAD per section, matching the recovered poc.
	for i, s := range secs {
		p := out[ehdrSize+i*phdrSize:]
		flags := uint32(4) // PF_R
		if s.flags&elf.SHF_WRITE != 0 {
			flags |= 2
		}
		if s.flags&elf.SHF_EXECINSTR != 0 {
			flags |= 1
		}
		le.PutUint32(p[0:], uint32(elf.PT_LOAD))
		le.PutUint32(p[4:], flags)
		le.PutUint64(p[8:], offsets[i])
		le.PutUint64(p[16:], s.addr)
		le.PutUint64(p[24:], s.addr)
		if !s.nobits {
			le.PutUint64(p[32:], uint64(len(s.data)))
		}
		le.PutUint64(p[40:], s.memsz)
		le.PutUint64(p[48:], pageSize)
		if !s.nobits {
			copy(out[offsets[i]:], s.data)
		}
	}

	copy(out[symtabOff:], symtab)
	copy(out[strtabOff:], strtab)
	copy(out[shstrOff:], shstr)

	// Shdrs: null, sections, .symtab, .strtab, .shstrtab.
	for i, s := range secs {
		sh := out[shoff+uint64((i+1)*shdrSize):]
		le.PutUint32(sh[0:], nameOff[i])
		le.PutUint32(sh[4:], uint32(s.secType))
		le.PutUint64(sh[8:], uint64(s.flags))
		le.PutUint64(sh[16:], s.addr)
		le.PutUint64(sh[24:], offsets[i])
		le.PutUint64(sh[32:], s.memsz)
		le.PutUint64(sh[48:], 16)
	}
	symIdx, strIdx, shstrIdx := len(secs)+1, len(secs)+2, len(secs)+3

	sh := out[shoff+uint64(symIdx*shdrSize):]
	le.PutUint32(sh[0:], symtabNameOff)
	le.PutUint32(sh[4:], uint32(elf.SHT_SYMTAB))
	le.PutUint64(sh[24:], symtabOff)
	le.PutUint64(sh[32:], uint64(len(symtab)))
	le.PutUint32(sh[40:], uint32(strIdx)) // sh_link -> .strtab
	le.PutUint32(sh[44:], 1)              // sh_info: first non-local symbol
	le.PutUint64(sh[48:], 8)
	le.PutUint64(sh[56:], 24) // sh_entsize

	sh = out[shoff+uint64(strIdx*shdrSize):]
	le.PutUint32(sh[0:], strtabNameOff)
	le.PutUint32(sh[4:], uint32(elf.SHT_STRTAB))
	le.PutUint64(sh[24:], strtabOff)
	le.PutUint64(sh[32:], uint64(len(strtab)))
	le.PutUint64(sh[48:], 1)

	sh = out[shoff+uint64(shstrIdx*shdrSize):]
	le.PutUint32(sh[0:], shstrNameOff)
	le.PutUint32(sh[4:], uint32(elf.SHT_STRTAB))
	le.PutUint64(sh[24:], shstrOff)
	le.PutUint64(sh[32:], uint64(len(shstr)))
	le.PutUint64(sh[48:], 1)

	return out, nil
}

// Close releases the parsed ELF handles.
func Close(objs []*Object) {
	for _, o := range objs {
		o.file.Close()
	}
}
