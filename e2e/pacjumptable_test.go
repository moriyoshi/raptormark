package e2e

import (
	"context"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The regression guard for elfconv patch 0062.
//
// # What broke, and why nothing here could see it
//
// A function whose ENTRY instruction is `paciasp` and whose jump table carries
// no BTI landing pads never had its cases lifted: patch 0025's symtab sweep was
// gated on `!entry_is_landing_pad`, and that predicate counts `paciasp`. The
// indirect `br` then missed the block-address map, fell through to the
// function's catch-all and thence to `__remill_jump`, which RE-ENTERS the
// containing function -- and the re-entered prologue stores through a zero stack
// pointer. `ruby:3-slim` died exactly there, in `rb_method_definition_set`.
//
// ⚠️ THE SUITE COULD NOT HAVE CAUGHT THIS, and that is why this file exists.
// Every other e2e guest is `gcc -static` in the builder image, which produces
// **zero** PAC-signed entries -- 876 FUNC symbols, 863 ordinary, 13 `bti c`, and
// not one function in the affected set. `entry_is_landing_pad` was false
// everywhere, so every function already took the swept path and the changed
// branch was unreachable. A green suite said nothing about it either way.
//
// # Why the dispatch function is hand-written assembly
//
// Not for control of the instruction encodings -- for control of the FIRST one.
// gcc 11 will not place `paciasp` at the literal entry of a function that also
// carries a jump table: with `-mbranch-protection=pac-ret` it hoists the range
// check or a register move above the prologue, and `-fno-shrink-wrap` and
// `-fno-schedule-insns2` do not stop it. Four C variants were tried and all
// produced an entry word of `cmp` or `mov`, which is not a landing pad, so the
// old code already swept them and the bug did not reproduce.
//
// Real code does have this shape; ruby's is 2,112 bytes of it. The assembly is
// the smallest thing that does.
//
// Neutralized against the pre-patch builder rather than by argument: the same
// guest on `raptormark-builder:embed` traps with `out of bounds memory access`
// (rc=134), and on the patched builder prints the value below.
func TestPACJumpTableCasesAreLifted(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elfPath := compileGuestWithAsm(t, ctx, dir, "pacjt", pacJumpTableMainSrc, pacJumpTableAsmSrc)

	// The fixture must HAVE the property before its execution means anything.
	// A guest that quietly lost its `paciasp` entry -- a different gcc, a
	// different assembler default -- would pass this test while exercising the
	// path that always worked.
	assertPACJumpTableShape(t, elfPath)

	wasm := liftOne(t, ctx, img, dir, elfPath, "pacjt")
	out := runWasm(t, ctx, wasm)
	if !strings.Contains(out, pacJumpTableExpected) {
		t.Errorf("the guest did not reach %q.\n"+
			"An `out of bounds memory access` here is the ORIGINAL defect: the jump-table\n"+
			"cases were not lifted, the `br` missed the block map, and `__remill_jump`\n"+
			"re-entered the function with a zero stack pointer.\nfull output:\n%s",
			pacJumpTableExpected, out)
	}
}

// assertPACJumpTableShape checks the three properties that put this guest on the
// changed code path. Each one alone is common; the combination is the bug.
func assertPACJumpTableShape(t *testing.T, path string) {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	syms, err := f.Symbols()
	if err != nil {
		t.Fatal(err)
	}
	var addr, size uint64
	for _, s := range syms {
		if s.Name == "dispatch" {
			addr, size = s.Value, s.Size
			break
		}
	}
	if addr == 0 || size < 16 {
		t.Fatalf("no `dispatch` symbol with a size in %s; the fixture is not what this test needs", path)
	}
	body := make([]byte, size)
	for _, s := range f.Sections {
		if s.Flags&elf.SHF_EXECINSTR == 0 || addr < s.Addr || addr+size > s.Addr+s.Size {
			continue
		}
		data, err := s.Data()
		if err != nil {
			t.Fatal(err)
		}
		copy(body, data[addr-s.Addr:addr-s.Addr+size])
	}

	const (
		paciasp = 0xD503233F
		btiJ    = 0xD503249F
		btiJC   = 0xD50324DF
	)
	if w := binary.LittleEndian.Uint32(body[:4]); w != paciasp {
		t.Errorf("dispatch's entry word is 0x%08x, want paciasp (0x%08x). Without a "+
			"landing-pad entry the OLD lifter already swept this function and the test "+
			"exercises nothing.", w, paciasp)
	}
	var brs, pads int
	for i := 0; i+4 <= len(body); i += 4 {
		switch w := binary.LittleEndian.Uint32(body[i : i+4]); {
		case w&0xFFFFFC1F == 0xD61F0000:
			brs++
		case w == btiJ || w == btiJC:
			pads++
		}
	}
	if brs == 0 {
		t.Error("dispatch has no indirect `br`; the assembler folded the jump table away")
	}
	if pads != 0 {
		t.Errorf("dispatch has %d BTI landing pad(s); with pads the BTI scan seeds the "+
			"cases and the bug cannot reproduce", pads)
	}
	t.Logf("fixture shape confirmed: paciasp entry, %d indirect br, %d BTI pads", brs, pads)
}

// compileGuestWithAsm builds a guest from one C file and one assembly file.
// `compileGuest` takes a single translation unit, and this fixture needs the
// assembly one.
func compileGuestWithAsm(t *testing.T, ctx context.Context, dir, name, csrc, ssrc string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".c"), []byte(csrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".S"), []byte(ssrc), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"},
		fmt.Sprintf("gcc -static -O2 -o /w/%s /w/%s.c /w/%s.S", name, name, name))
	if err != nil {
		t.Fatalf("compiling %s: %v\n%s", name, err, out)
	}
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("guest %s was not produced: %v", name, err)
	}
	return p
}

// The value is the native oracle: the same program run under Linux prints it.
const pacJumpTableExpected = "PACJT-OK 10996500"

const pacJumpTableMainSrc = `#include <stdio.h>
long dispatch(int op, long a, long b);
int main(void) {
	long acc = 0;
	for (int i = 0; i < 4000; i++) acc += dispatch(i % 8, i, i + 3);
	printf("PACJT-OK %ld\n", acc);
	return 0;
}
`

// paciasp FIRST, a jump table through `br`, and no `bti j` anywhere -- the three
// properties together, which is what no C the builder's gcc emits will give.
const pacJumpTableAsmSrc = `	.text
	.global	dispatch
	.type	dispatch, %function
dispatch:
	paciasp
	stp	x29, x30, [sp, #-16]!
	mov	x29, sp
	cmp	w0, #7
	b.hi	.Lbad
	adrp	x3, .Ltbl
	add	x3, x3, :lo12:.Ltbl
	ldr	x5, [x3, w0, uxtw #3]
	br	x5
.L0:	add	x0, x1, x2
	b	.Lout
.L1:	sub	x0, x1, x2
	b	.Lout
.L2:	add	x0, x2, x1, lsl #1
	b	.Lout
.L3:	eor	x0, x1, x2
	b	.Lout
.L4:	add	x0, x2, x1, lsl #2
	b	.Lout
.L5:	orr	x0, x1, x2
	b	.Lout
.L6:	and	x0, x1, x2
	b	.Lout
.L7:	sub	x0, x1, x2, lsl #1
	b	.Lout
.Lbad:	mov	x0, #0
.Lout:	ldp	x29, x30, [sp], #16
	autiasp
	ret
	.size	dispatch, .-dispatch
	.section .rodata
	.align	3
.Ltbl:	.quad	.L0, .L1, .L2, .L3, .L4, .L5, .L6, .L7
`
