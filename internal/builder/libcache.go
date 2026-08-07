package builder

// Cache for lifted LIBRARY code, one entry PER LIBRARY.
//
// WHY THIS IS THE RIGHT UNIT. After the four serial passes became one
// (ecv-prepare), a partition-cache-warm translation is 68% elflift. That time is
// re-lifting libraries another program of the same closure already lifted:
// `internal/fuse.PlanLayout` gives every library one address across the whole
// closure and patches/0046 names every lifted function from its address, so a
// library's lifted IR is identical in every program that links it. Measured on
// bash-glibc, all 4,071 library function bodies are byte-identical between a
// whole-program lift and a library-only one (modulo per-module metadata
// numbering, which linking renumbers anyway).
//
// PER LIBRARY, NOT PER LIBRARY SET, and the difference decides whether this is
// worth having. The first version cached the whole library half under one key.
// Two programs of a closure rarely link the same SET -- /bin/echo takes libc,
// /bin/bash takes libc and libtinfo -- so that key never matched between them:
// echo+bash cached two halves, shared nothing, and the split lift became pure
// overhead (716 s against 601 s without the cache). Keyed per library, echo's
// libc serves bash's libc and a closure pays once per DISTINCT LIBRARY.
//
// SPLITTING THE LIFT IS CLOSE TO FREE, which is what makes it affordable to do
// per library. bash-glibc's three libraries lifted separately cost
// 0.464 + 7.345 + 0.729 = 8.538 s against 8.687 s for all three in one
// invocation, so elflift's per-invocation setup does not dominate. The exe and
// library halves likewise partition rather than duplicate the work: 5.017 +
// 8.649 = 13.67 s against 13.859 s whole. NOT extrapolated to postgres's 31
// libraries -- build costs in this tree are famously non-uniform.
//
// THE KEY IS THE LIBRARY'S OWN CODE, not the fused ELF. Two programs of one
// closure have different fused images -- different executables -- while sharing
// library code at identical addresses. Hashing the whole ELF would miss every
// time; hashing one library's executable sections and the symbols in its span
// hits exactly when the lifted result would be the same. The lifter's own
// identity goes in too, since a lifter patch changes the IR for unchanged input.
//
// Not enabled unless both ECV_LIB_CACHE (where to keep it) and ECV_LIB_RANGES
// (which addresses are library) are set. Without them this is inert and the
// pipeline lifts whole images exactly as before.

import (
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	libCacheEnv  = "ECV_LIB_CACHE"
	libRangesEnv = "ECV_LIB_RANGES"
)

// errNoCodeInRange means this program has no code in that library's span, i.e.
// it does not link that library.
//
// NOT AN ERROR, and treating it as one cost a closure run. The range list comes
// from the CLOSURE (`internal/fuse.FormatLibRanges` over the whole report), so
// it names every library any program in the closure links -- and a given program
// links a subset. Aborting the split lift for the whole program on the first
// such range made /bin/echo fall back to a whole-image lift while /bin/bash took
// the per-library path. Two programs built by DIFFERENT PIPELINES produce
// different partitions, and cross-program reuse went to -5 of 121 with nothing
// wrong in either module.
var errNoCodeInRange = errors.New("no executable sections in this library range")

// libRange is one half-open guest VMA span, as internal/fuse.FormatLibRanges
// emits it and as elflift's --lift_range parses it.
type libRange struct{ start, end uint64 }

// parseLibRanges reads the "lo:hi,lo:hi" form. An unparsable spec is not an
// error to shout about: it disables the cache, because lifting whole images is
// always correct and this is only ever an optimisation.
func parseLibRanges(spec string) []libRange {
	var out []libRange
	for _, one := range strings.Split(spec, ",") {
		lo, hi, ok := strings.Cut(strings.TrimSpace(one), ":")
		if !ok {
			return nil
		}
		l, err1 := strconv.ParseUint(strings.TrimPrefix(lo, "0x"), 16, 64)
		h, err2 := strconv.ParseUint(strings.TrimPrefix(hi, "0x"), 16, 64)
		if err1 != nil || err2 != nil || h <= l {
			return nil
		}
		out = append(out, libRange{l, h})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// libCacheDir returns where cached library modules live, or "" when off.
func libCacheDir() string { return os.Getenv(libCacheEnv) }

// libRangesSpec returns the raw --lift_range spec, or "" when unset.
func libRangesSpec() string { return os.Getenv(libRangesEnv) }

// libCacheActive reports whether the split lift should be used at all.
func libCacheActive() bool {
	return libCacheDir() != "" && len(parseLibRanges(libRangesSpec())) > 0
}

// exeRangeSpec is everything below the first library, which is where the
// executable lives: `internal/fuse.PlanLayout` places every library ABOVE the
// highest executable top in the closure. One span suffices, and it deliberately
// starts at 0 rather than at the executable's first section -- anything lifted
// that is neither library nor executable (synthesised thunks) must land in the
// per-program half, not in the shared one.
func exeRangeSpec(ranges []libRange) string {
	return fmt.Sprintf("%#x:%#x", 0, ranges[0].start)
}

// overlaps reports whether [addr, addr+size) meets any declared library range.
func overlaps(ranges []libRange, addr, size uint64) bool {
	for _, r := range ranges {
		if addr < r.end && r.start < addr+size {
			return true
		}
	}
	return false
}

// libKey identifies a library half by what actually determines its lifted IR.
//
// TWO INPUTS, and both are needed:
//
// The version tag moved to v4 when halves began being STRIPPED before caching
// (see liftSplit). An unstripped entry is still a valid input, so mixing would
// be safe; the bump is so existing caches actually collect the benefit, and a
// re-lift on a miss costs nothing measurable.
//
//   - EXECUTABLE sections overlapping the declared ranges: address, size, bytes.
//     Address is in the hash because the lifted IR embeds guest addresses in
//     names and bodies, so the same bytes at a different base are a different
//     result.
//
//     NOT THE SECTION NAME, and that omission is the point rather than an
//     oversight. The fuser names a library's sections `.text.l<N>` where N is a
//     PER-PROGRAM slot index in that program's own library order -- so one
//     library is `.text.l0` in /bin/echo and `.text.l1` in /bin/bash, purely
//     because bash links an extra library that takes slot 0. Hashing the name
//     therefore split the key for identical code at an identical address, and
//     per-library caching shared NOTHING between those two programs (5 cached
//     modules for 2+3 libraries). Measured directly: with the name, echo and
//     bash key `8030710094d177e0` against `d5725f7dbb2b66f8`; without it, both
//     key `e0a159b4f38bac3f`. Address plus size plus bytes already identifies
//     the content uniquely -- two sections cannot share an address -- so the
//     name adds nothing but the program's packaging.
//
//   - SYMBOLS whose value falls in those ranges: name, value, size. elflift
//     derives function boundaries from the merged symbol table, so a library
//     whose symbols were recorded differently lifts to a different set of
//     functions even with identical code bytes.
//
// WHAT IT MUST NOT INCLUDE, learned by measuring rather than by reasoning. The
// first version hashed every ALLOCATED section at or above the first library
// base, and echo and cat -- two programs of ONE closure, with byte-identical
// library placement -- produced different keys and both re-lifted. The fused
// layout puts per-program material above that base:
//
//	.got.l0      relocated pointers, some into the executable
//	.ecv.irela   per-program relocation metadata (48 vs 64 bytes)
//	.ecv.init    per-program init table
//	.ecv.dlsyms  per-program dynamic symbols
//	.ecv.funcs   per-program function inventory (62,240 vs 62,400 bytes)
//
// None of it reaches the merged module: it is DATA, the library half's copy is
// dropped as a duplicate at merge time, and the program half supplies the real
// one. Confirmed on those two cached halves -- 7,321 defined symbols each, 5,150
// function definitions each, and all 5,150 bodies identical modulo per-module
// numbering, despite the modules differing by 1,080 bytes.
//
// `lifterID` folds in whatever identifies the lifter build, since a lifter patch
// changes the IR for unchanged input.
func libKey(elfPath, lifterID, rangesSpec string, ranges []libRange) (string, error) {
	f, err := elf.Open(elfPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	writeKeyPart(h, "raptormark-lib-lift-v4")
	writeKeyPart(h, lifterID)
	writeKeyPart(h, rangesSpec)

	var covered int
	for _, s := range f.Sections {
		if s.Flags&elf.SHF_EXECINSTR == 0 || s.Size == 0 || !overlaps(ranges, s.Addr, s.Size) {
			continue
		}
		var n [16]byte
		binary.LittleEndian.PutUint64(n[0:], s.Addr)
		binary.LittleEndian.PutUint64(n[8:], s.Size)
		h.Write(n[:])
		if s.Type == elf.SHT_NOBITS {
			covered++
			continue
		}
		if _, err := io.Copy(h, s.Open()); err != nil {
			return "", fmt.Errorf("hashing section %s: %w", s.Name, err)
		}
		covered++
	}
	if covered == 0 {
		return "", errNoCodeInRange
	}

	// Symbols are hashed in a canonical order: the table's order is not part of
	// what the lifter derives, and two fusings could record the same set
	// differently.
	syms, err := f.Symbols()
	if err != nil && err != elf.ErrNoSymbols {
		return "", fmt.Errorf("reading symbols of %s: %w", elfPath, err)
	}
	var inRange []string
	for _, s := range syms {
		if s.Value != 0 && overlaps(ranges, s.Value, 1) {
			inRange = append(inRange, fmt.Sprintf("%s|%d|%d", s.Name, s.Value, s.Size))
		}
	}
	sort.Strings(inRange)
	writeKeyPart(h, fmt.Sprintf("symbols=%d", len(inRange)))
	for _, s := range inRange {
		writeKeyPart(h, s)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeKeyPart(h io.Writer, s string) {
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(len(s)))
	h.Write(n[:])
	io.WriteString(h, s)
}

// liftSplit lifts the executable half into `bc` and returns the path to the
// library half, taken from cache when another program of the closure has already
// lifted the same libraries at the same addresses.
//
// On a MISS both halves are lifted, which is not a penalty: measured on
// bash-glibc the halves cost 5.017 s and 8.649 s against 13.859 s for one whole
// lift, so the split partitions the work instead of repeating it.
//
// On any error the caller gets a whole-image lift instead. A cache is an
// optimisation and must never be the reason a translation fails -- if the key
// cannot be computed or the cached module cannot be read, lifting everything is
// still correct.
func (c *TranslateOne) liftSplit(bc string) ([]string, error) {
	ranges := parseLibRanges(libRangesSpec())
	dir := libCacheDir()

	// The lifter's identity. BASE_ID names the patched elfconv image the lifter
	// was built in, which is exactly what changes when a lifter patch lands.
	lifterID := os.Getenv("ECV_BASE_ID")
	if lifterID == "" {
		lifterID = "unknown-lifter"
	}

	// ONE CACHE ENTRY PER LIBRARY. Two programs of a closure rarely link the
	// same SET -- /bin/echo takes libc and one other, /bin/bash takes those plus
	// a third -- so a set-granular key never matched between them and each paid
	// a full library lift. Keyed per library, the libraries they share are lifted
	// once for the closure.
	//
	// Splitting the lift per library is close to free: bash-glibc's three
	// libraries cost 0.464 + 7.345 + 0.729 = 8.538 s separately against 8.687 s
	// in one invocation, so elflift's per-invocation setup does not dominate.
	// NOT extrapolated to postgres's 31 libraries.
	// PLAN FIRST, then lift. Each range is an independent elflift invocation,
	// so the misses and the executable half can run CONCURRENTLY -- and on the
	// only workload where it matters they overlap almost perfectly.
	//
	// Measured on postgres, lifting all 33 ranges separately: the sequential sum
	// is 326.6 s against ~346 s for one whole-image lift (splitting is free at
	// this scale too), and the EXECUTABLE range alone is 160.3 s of it -- 49%.
	// Every library together is 166.3 s, the largest single one 31.6 s. So the
	// parallel wall is the exe at 160.3 s and the ceiling is **2.0x**.
	//
	// That shape is what makes the concurrency cheap: because one unit dominates,
	// even two or three workers reach the ceiling, so this never needs 33 elflift
	// processes resident at once. `parallelLifts` caps it.
	type liftJob struct {
		spec, out, key string
		phase          string
	}
	var jobs []liftJob
	var libBCs []string
	var hits, misses, skipped int
	for _, r := range ranges {
		spec := fmt.Sprintf("%#x:%#x", r.start, r.end)
		key, err := libKey(c.ELF, lifterID, spec, []libRange{r})
		if errors.Is(err, errNoCodeInRange) {
			// This program does not link that library. The range list is
			// closure-wide and every program links a subset.
			skipped++
			continue
		}
		if err != nil {
			fmt.Printf("translate-one: library lift cache disabled: %v\n", err)
			return nil, c.tm.step("elflift", c.ELF, bc, func() error { return c.lift(bc) })
		}
		cached := libCachePath(dir, key)
		if _, statErr := os.Stat(cached); statErr == nil {
			libBCs = append(libBCs, cached)
			hits++
			continue
		}
		misses++
		jobs = append(jobs, liftJob{
			spec:  spec,
			out:   filepath.Join(c.Out, fmt.Sprintf("%s.lib_%x.bc", c.ModuleID, r.start)),
			key:   key,
			phase: fmt.Sprintf("elflift-lib-%#x", r.start),
		})
	}

	// The executable half is lifted every time and is the long pole, so it goes
	// into the same pool rather than after it.
	var sem = make(chan struct{}, parallelLifts())
	var wg sync.WaitGroup
	errs := make([]error, len(jobs)+1)
	stripped := make([]string, len(jobs))

	wg.Add(1)
	go func() {
		defer wg.Done()
		sem <- struct{}{}
		defer func() { <-sem }()
		errs[len(jobs)] = c.tm.step("elflift-exe", c.ELF, bc, func() error {
			return c.liftRange(bc, exeRangeSpec(ranges))
		})
	}()

	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j liftJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := c.tm.step(j.phase, c.ELF, j.out, func() error {
				return c.liftRange(j.out, j.spec)
			}); err != nil {
				errs[i] = err
				return
			}
			// STRIP BEFORE CACHING. A lifted half carries the same fixed payload
			// whatever range it covers -- remill's semantics, the ISEL tables, the
			// guest data. At postgres scale that is **42.6 MB per half**: an
			// 8-byte range still lifts to 42.8 MB, and stripping takes it to
			// 0.15 MB. Across 32 libraries it is ~1.36 GB of bitcode that would
			// otherwise be stored and re-parsed on every translation.
			//
			// Byte-identical output: merging stripped halves gives the same
			// partitions as merging unstripped ones, measured 124 of 124.
			out := j.out + ".stripped"
			if err := run("ecv-prepare", "--strip", j.spec, j.out, out); err != nil {
				fmt.Printf("translate-one: could not strip a library half, caching it whole: %v\n", err)
				out = j.out
			}
			stripped[i] = out
		}(i, j)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	// Publishing is serial and cheap; doing it here keeps the cache writes off
	// the concurrent path, where a partial store would be harder to reason about.
	for i, j := range jobs {
		if err := storeLibModule(dir, j.key, stripped[i]); err != nil {
			fmt.Printf("translate-one: could not publish a library lift: %v\n", err)
			libBCs = append(libBCs, stripped[i])
			continue
		}
		libBCs = append(libBCs, libCachePath(dir, j.key))
	}
	if len(libBCs) == 0 {
		// Links no library in the closure's ranges; a whole-image lift is right.
		return nil, c.tm.step("elflift", c.ELF, bc, func() error { return c.lift(bc) })
	}

	fmt.Printf("translate-one: library lift cache: %d hit, %d miss, %d not linked (%d ranges)\n",
		hits, misses, skipped, len(ranges))
	return libBCs, nil
}

// liftRange runs elflift restricted to the given guest VMA ranges
// (patches/0052). Discovery is unaffected: a function outside the range stays a
// known entry and is emitted as a declaration.
func (c *TranslateOne) liftRange(bc, ranges string) error {
	return run(elfconvRoot+"/build/lifter/elflift",
		append(c.liftArgs(bc), "--lift_range", ranges)...)
}

// libCachePath is where a given key's library module lives.
func libCachePath(dir, key string) string {
	return filepath.Join(dir, key[:2], key+".bc")
}

// storeLibModule publishes a freshly lifted library half. Written to a temporary
// name in the same directory and renamed, so a concurrent reader never sees a
// partial module and two programs racing on one key cannot corrupt it -- the
// same discipline partcache.go uses, and for the same reason: a closure's
// programs are translated concurrently.
func storeLibModule(dir, key, src string) error {
	dst := libCachePath(dir, key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".lib-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	tmp.Close()
	if err := copyFile(src, tmpName); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// parallelLifts caps how many elflift processes run at once.
//
// Deliberately small. The measured shape on postgres is one dominant unit --
// the executable range at 160.3 s against 166.3 s for all 32 libraries combined
// and 31.6 s for the largest -- so the wall is the exe whatever else is running,
// and two or three workers already reach the 2.0x ceiling. More would buy
// nothing and cost memory: each elflift loads the semantics module and emits
// tens of MB of bitcode, and 33 at once is a way to meet the OOM killer rather
// than a way to go faster.
//
// ECV_LIFT_JOBS overrides it, for measuring the shape on a workload whose
// libraries are more evenly matched than postgres's.
func parallelLifts() int {
	if v := os.Getenv("ECV_LIFT_JOBS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	if n := runtime.NumCPU(); n < 4 {
		return 1
	}
	return 4
}
