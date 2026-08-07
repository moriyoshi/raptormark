package fuse

import (
	"debug/elf"
	"testing"
)

// `globalSymbols` must not prefer a COMPAT version over the default one.
//
// # The defect this guards
//
// Symbol versions are discarded -- `DynamicSymbols()` returns bare names, so
// `exp@GLIBC_2.17` and `exp@GLIBC_2.29` are one key. Plain first-wins then picks
// by `.dynsym` ORDER, and glibc lists the compat definition first.
//
// Measured 2026-08-26 over the real postgres:17 `DT_NEEDED` closure (89 objects,
// 76,458 names, `.agents-workspace/drivers/symver`): 67 names resolved to more
// than one implementation, 32 bound the oldest, and THREE were actively wrong --
// something referenced them at a newer version:
//
//	exp    referenced @GLIBC_2.29 by postgres, libLLVM, libz3 -> bound @GLIBC_2.17
//	fmod   referenced @GLIBC_2.38 by postgres, libLLVM        -> bound @GLIBC_2.17
//	log2f  referenced @GLIBC_2.27 by libLLVM                  -> bound @GLIBC_2.17
//
// # Why `glob` is the subject
//
// ❗ `glob` and `glob64` are ALIASES -- literally the same two addresses in
// libc -- and `.dynsym` lists them in OPPOSITE order:
//
//	1813: 0x0c65a4  glob64@@GLIBC_2.27     <- default first
//	1814: 0x141920  glob64@GLIBC_2.17
//	1930: 0x141920  glob@GLIBC_2.17        <- compat first
//	1933: 0x0c65a4  glob@@GLIBC_2.27
//
// So one fused image gave the same pair of functions two DIFFERENT
// implementations. That makes the property self-evidently wrong without needing
// any claim about which glibc version anyone wanted: whatever `glob` should
// resolve to, it is whatever `glob64` resolves to.
//
// ⚠️ The addresses and ordering above are TRANSCRIBED from
// `readelf --dyn-syms` on the aarch64 glibc in `postgres:17`, not derived from a
// run. A test that generated its own expectation would ratify whatever the code
// currently does.

// versioned builds a `.dynsym` entry. `hidden` marks a non-default (compat)
// version -- bit 15 of the version index, what `readelf` shows as `foo@VER`
// rather than `foo@@VER`.
func versioned(name string, value uint64, hidden bool) elf.Symbol {
	idx := elf.VersionIndex(2)
	if hidden {
		idx = elf.VersionIndex(0x8000 | 2)
	}
	return elf.Symbol{
		Name: name, Value: value, Section: elf.SectionIndex(13),
		Info:       uint8(elf.ST_INFO(elf.STB_GLOBAL, elf.STT_FUNC)),
		HasVersion: true, VersionIndex: idx,
	}
}

// unversioned builds a `.dynsym` entry with no version information at all --
// musl, and every plugin measured.
func unversioned(name string, value uint64) elf.Symbol {
	return elf.Symbol{
		Name: name, Value: value, Section: elf.SectionIndex(13),
		Info: uint8(elf.ST_INFO(elf.STB_GLOBAL, elf.STT_FUNC)),
	}
}

func symObj(name string, base uint64, syms ...elf.Symbol) *Object {
	return &Object{Path: name, Name: name, LibIndex: 0, Base: base, symbols: syms}
}

// TestGlobalSymbolsPrefersTheDefaultVersionOverACompatOne.
func TestGlobalSymbolsPrefersTheDefaultVersionOverACompatOne(t *testing.T) {
	const base = 0x3200000
	const v217, v227 = 0x141920, 0x0c65a4 // the compat and default glob bodies

	// Transcribed .dynsym order, including the asymmetry that is the whole
	// point: glob64's default comes first, glob's compat comes first.
	libc := symObj("libc.so.6", base,
		versioned("glob64", v227, false),
		versioned("glob64", v217, true),
		versioned("glob", v217, true),
		versioned("glob", v227, false),
	)

	syms, _ := globalSymbols([]*Object{libc})

	if syms["glob"] != syms["glob64"] {
		t.Errorf("glob and glob64 are aliases of the same two bodies but resolved "+
			"differently: glob=%#x glob64=%#x.\n"+
			"They differ ONLY in .dynsym order -- glob lists its @GLIBC_2.17 compat "+
			"definition first and glob64 lists its @@GLIBC_2.27 default first -- so "+
			"this is first-wins preferring a compat version over the default one.",
			syms["glob"], syms["glob64"])
	}
	if got, want := syms["glob"], uint64(base+v227); got != want {
		t.Errorf("glob resolved to %#x, want %#x (the @@GLIBC_2.27 default). "+
			"%#x would be the @GLIBC_2.17 compat body.", got, want, base+v217)
	}
}

// TestGlobalSymbolsBindsTheReferencedMathImplementations covers the three that
// were ACTIVELY wrong -- referenced by name at a newer version than the one
// first-wins bound. `exp` is the one `postgres` itself imports.
func TestGlobalSymbolsBindsTheReferencedMathImplementations(t *testing.T) {
	const base = 0x4000000
	cases := []struct {
		name             string
		compat, deflt    uint64
		referencedAt     string
		compatFirstInDyn bool
	}{
		// libm lists the compat body first for each of these.
		{"exp", 0x012b60, 0x038c00, "GLIBC_2.29", true},
		{"log2f", 0x014620, 0x04c420, "GLIBC_2.27", true},
		{"fmod", 0x011000, 0x039000, "GLIBC_2.38", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var order []elf.Symbol
			if c.compatFirstInDyn {
				order = []elf.Symbol{
					versioned(c.name, c.compat, true),
					versioned(c.name, c.deflt, false),
				}
			} else {
				order = []elf.Symbol{
					versioned(c.name, c.deflt, false),
					versioned(c.name, c.compat, true),
				}
			}
			syms, _ := globalSymbols([]*Object{symObj("libm.so.6", base, order...)})
			if got, want := syms[c.name], base+c.deflt; got != want {
				t.Errorf("%s resolved to %#x, want %#x (the default version). "+
					"%#x is the SVID-era compat body. Real callers reference "+
					"%s@%s, so binding the compat one silently runs a different "+
					"implementation.",
					c.name, got, want, base+c.compat, c.name, c.referencedAt)
			}
		})
	}
}

// ❗ CONTROL. The fix must not disturb anything that is not versioned, because
// that is nearly everything: musl has no symbol versioning, and postgres's 79
// extensions were measured at ZERO versioned names. On such inputs the rule has
// to degenerate to exactly the old first-wins, or the per-unit plugin path
// changes meaning as a side effect of a libc fix.
func TestGlobalSymbolsKeepsFirstWinsForUnversionedSymbols(t *testing.T) {
	a := symObj("liba.so", 0x1000, unversioned("dup", 0x10), unversioned("only_a", 0x20))
	b := symObj("libb.so", 0x2000, unversioned("dup", 0x30), unversioned("only_b", 0x40))

	syms, _ := globalSymbols([]*Object{a, b})

	if got, want := syms["dup"], uint64(0x1010); got != want {
		t.Errorf("dup resolved to %#x, want %#x (liba, the first definition). "+
			"Unversioned symbols must keep plain first-wins -- musl and every "+
			"measured plugin are entirely unversioned.", got, want)
	}
	if syms["only_a"] != 0x1020 || syms["only_b"] != 0x2040 {
		t.Errorf("unique names moved: only_a=%#x only_b=%#x", syms["only_a"], syms["only_b"])
	}
}

// ❗ CONTROL. Two DEFAULT definitions in different objects must still resolve
// first-wins. That is INTERPOSITION -- the executable, then libraries in load
// order, which is what a dynamic loader would do. A rule that preferred "the
// later default" would break it, and no versioning test would notice.
func TestGlobalSymbolsKeepsInterpositionBetweenTwoDefaults(t *testing.T) {
	first := symObj("libfirst.so", 0x1000, versioned("shared", 0x10, false))
	second := symObj("libsecond.so", 0x2000, versioned("shared", 0x20, false))

	syms, _ := globalSymbols([]*Object{first, second})

	if got, want := syms["shared"], uint64(0x1010); got != want {
		t.Errorf("shared resolved to %#x, want %#x (libfirst). Two DEFAULT "+
			"definitions must keep load-order interposition; only a compat "+
			"version may be displaced by a default one.", got, want)
	}
}

// ❗ CONTROL. A compat definition that is an ifunc, displaced by a default that
// is not, must clear the ifunc mark.
//
// ⚠️ This is the one way the fix could introduce a NEW silent defect. An ifunc
// symbol's value is a RESOLVER, not the implementation. If `ifuncs` still said
// "resolver" while `out` now held a plain function, every call through that name
// would invoke a function that returns a pointer and does nothing -- the exact
// silent no-op the sttGNUIFunc comment warns about.
func TestGlobalSymbolsClearsTheIfuncMarkWhenADefaultDisplacesACompatIfunc(t *testing.T) {
	compatIfunc := versioned("memcpy", 0x10, true)
	compatIfunc.Info = uint8(elf.ST_INFO(elf.STB_GLOBAL, sttGNUIFunc))
	defaultPlain := versioned("memcpy", 0x20, false)

	syms, ifuncs := globalSymbols([]*Object{
		symObj("libc.so.6", 0x1000, compatIfunc, defaultPlain),
	})

	if got, want := syms["memcpy"], uint64(0x1020); got != want {
		t.Fatalf("memcpy resolved to %#x, want %#x (the default)", got, want)
	}
	if ifuncs["memcpy"] {
		t.Error("memcpy is still marked an ifunc after a plain default displaced " +
			"the compat ifunc. The address now held is an implementation, not a " +
			"resolver; calling it as a resolver returns a pointer and does nothing.")
	}
}

// And the reverse: a default that IS an ifunc must stay marked.
func TestGlobalSymbolsKeepsTheIfuncMarkWhenTheDefaultIsAnIfunc(t *testing.T) {
	compatPlain := versioned("memmove", 0x10, true)
	defaultIfunc := versioned("memmove", 0x20, false)
	defaultIfunc.Info = uint8(elf.ST_INFO(elf.STB_GLOBAL, sttGNUIFunc))

	syms, ifuncs := globalSymbols([]*Object{
		symObj("libc.so.6", 0x1000, compatPlain, defaultIfunc),
	})

	if got, want := syms["memmove"], uint64(0x1020); got != want {
		t.Fatalf("memmove resolved to %#x, want %#x (the default)", got, want)
	}
	if !ifuncs["memmove"] {
		t.Error("memmove lost its ifunc mark although the default definition is " +
			"an ifunc. The runtime would bind the resolver's address as if it " +
			"were the implementation.")
	}
}
