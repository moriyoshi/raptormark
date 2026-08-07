package e2e

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"raptormark/internal/fuse"
	"raptormark/internal/image"
)

// TestMinsigstacksizeIsLocatedInARealGlibc guards the derivation of
// `_rtld_global_ro._dl_minsigstacksize` against a real, current glibc.
//
// The field is what `sysconf(_SC_MINSIGSTKSZ)` asserts is non-zero. ld.so sets
// it and a fused image never runs ld.so, so python:3-slim aborted there after
// every one of its constructors had already run. The prelinker cannot ask for
// the offset -- thread_db does not describe `rtld_global_ro` -- so it DECODES it
// from the code of two accessors and requires them to agree
// (`minsigstacksizeVMA`). A wrong answer writes 5120 into an unrelated member of
// `_rtld_global_ro`, which is silent.
//
// The unit tests in `internal/fuse` pin that decode to instruction encodings
// captured from one glibc. This is the other half: it runs the real decode over
// a real libc the tests have never seen, and checks the result against two facts
// about the STRUCT rather than about the code —
//
//   - the word before must be `_dl_pagesize`, which ld.so's static initialiser
//     sets (65536 on aarch64, EXEC_PAGESIZE) and which a fused image therefore
//     already carries;
//   - the field itself must still be zero, which is the whole reason the guest
//     aborts.
//
// Both are exactly what the runtime re-checks before writing, so a failure here
// means the runtime would have refused and the fix would have silently done
// nothing.
//
// Fuse-only: no translation, no wasm, seconds. The image is a stock Debian
// rather than python:3-slim because any dynamically linked glibc program
// exercises the same code, and this must not depend on a 20-minute lift.
func TestMinsigstacksizeIsLocatedInARealGlibc(t *testing.T) {
	requireE2E(t)
	if err := exec.Command("docker", "image", "inspect", closureFixture).Run(); err != nil {
		t.Skipf("fixture %s not present locally (this test does not pull)", closureFixture)
	}
	ctx := ctxFor(t)

	root := t.TempDir()
	if err := image.ExportRootfs(ctx, closureFixture, root); err != nil {
		t.Fatalf("exporting %s: %v", closureFixture, err)
	}
	exe := filepath.Join(root, "bin/echo")
	if _, err := os.Stat(exe); err != nil {
		t.Skipf("%s has no /bin/echo: %v", closureFixture, err)
	}
	images, _, err := fuse.FuseClosure([]string{exe}, fuse.Options{LibraryPaths: []string{
		filepath.Join(root, "lib"),
		filepath.Join(root, "usr/lib"),
		filepath.Join(root, "lib/aarch64-linux-gnu"),
		filepath.Join(root, "usr/lib/aarch64-linux-gnu"),
	}})
	if err != nil {
		t.Fatalf("FuseClosure: %v", err)
	}

	f, err := elf.NewFile(bytes.NewReader(images[0]))
	if err != nil {
		t.Fatalf("reading the fused image: %v", err)
	}
	sec := f.Section(".ecv.stacklists")
	if sec == nil {
		t.Fatal("no .ecv.stacklists in a fused glibc closure; the whole glibc bring-up table is missing")
	}
	d, err := sec.Data()
	if err != nil {
		t.Fatal(err)
	}
	if len(d) < 80 {
		t.Fatalf(".ecv.stacklists is %d bytes; word 9 (_dl_minsigstacksize) is absent, so "+
			"apply_stacklists will skip the seed entirely:\n%s", len(d), hex.Dump(d))
	}
	field := binary.LittleEndian.Uint64(d[72:])
	if field == 0 {
		t.Fatal("_dl_minsigstacksize was not derived from this glibc. The decode refused, " +
			"which is the safe outcome but leaves sysconf(_SC_MINSIGSTKSZ) aborting; " +
			"see minsigstacksizeVMA for which of the two readings has to be re-derived")
	}

	read := func(vma uint64) (uint64, bool) {
		for _, p := range f.Progs {
			if p.Type != elf.PT_LOAD || vma < p.Vaddr || vma+8 > p.Vaddr+p.Filesz {
				continue
			}
			b := make([]byte, 8)
			if _, err := p.ReadAt(b, int64(vma-p.Vaddr)); err != nil {
				return 0, false
			}
			return binary.LittleEndian.Uint64(b), true
		}
		return 0, false
	}

	pagesize, ok := read(field - 8)
	if !ok {
		t.Fatalf("_dl_minsigstacksize at %#x is not in any PT_LOAD of the fused image", field)
	}
	if pagesize == 0 || pagesize&(pagesize-1) != 0 || pagesize < 4096 || pagesize > 65536 {
		t.Errorf("the word before %#x is %d, which is not a page size — so the offset does "+
			"NOT land on _dl_minsigstacksize, and the runtime will refuse to write it",
			field, pagesize)
	}
	cur, ok := read(field)
	if !ok {
		t.Fatalf("cannot read %#x out of the fused image", field)
	}
	if cur != 0 {
		t.Errorf("_dl_minsigstacksize at %#x already holds %d. Either the offset is wrong, or "+
			"this glibc initialises the field statically — in which case the guest never "+
			"aborted and this seed is not needed", field, cur)
	}
	t.Logf("_dl_minsigstacksize at %#x (_dl_pagesize=%d immediately before it, field still zero)",
		field, pagesize)
}
