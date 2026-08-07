package e2e

import (
	"os"
	"slices"
	"strings"
	"testing"

	"raptormark/internal/oci"
)

// What a host must supply to run a raptormark module, split by where it comes
// from. This list IS the module's portability surface, and until now it existed
// only as prose in `internal/oci`'s package doc.
//
// The split is the point. "No wasm proposal beyond 2.0" says an engine can
// DECODE the module -- that is `TestWasmOptEnablesNoProposal`, and it is only
// half the question. The other half is whether the embedder can SATISFY it, and
// that is decided here: a stock wasmtime provides every name in the first group
// and none in the second, which is why `README.md` says it cannot run the
// module.
var (
	// Standard `wasi_snapshot_preview1`. Any conforming host has these.
	stdPreview1 = []string{
		"args_get", "args_sizes_get",
		"clock_time_get",
		"environ_get", "environ_sizes_get",
		"fd_close", "fd_fdstat_set_flags", "fd_filestat_get",
		"fd_prestat_dir_name", "fd_prestat_get", "fd_read", "fd_write",
		"path_open", "poll_oneoff", "proc_exit", "random_get",
		// In the preview1 snapshot, though later than most of the above.
		"sock_accept",
	}
	// WasmEdge's socket EXTENSION to preview1. Not standard, and the reason a
	// raptormark module is WasmEdge-bound. Growing this list narrows portability
	// further, so it is a decision rather than a detail.
	wasmEdgeSockets = []string{
		"sock_bind", "sock_connect", "sock_getlocaladdr", "sock_getpeeraddr",
		"sock_getsockopt", "sock_listen", "sock_open", "sock_recv_from",
		"sock_send_to", "sock_setsockopt", "sock_shutdown",
	}
)

// TestModuleImportsOnlyWhatAHostCanSupply pins the portability surface of a real
// linked module.
//
// It asserts a SUBSET rather than an exact set on purpose: which imports survive
// depends on what the guest reaches, so a small guest legitimately needs fewer
// than nginx. What must never happen is a name nobody listed, or an import from
// a namespace other than `wasi_snapshot_preview1` -- either is a new demand on
// the host, and the failure explains why that matters instead of just printing a
// diff.
func TestModuleImportsOnlyWhatAHostCanSupply(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "importprobe", importProbeGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "importprobe")

	b, err := os.ReadFile(wasm)
	if err != nil {
		t.Fatal(err)
	}
	got, err := oci.ImportSet(b)
	if err != nil {
		t.Fatalf("reading the module's imports: %v", err)
	}
	if len(got) == 0 {
		// Without this the test passes on a parser that silently found nothing,
		// which is the shape a subset assertion cannot otherwise detect.
		t.Fatal("no imports found; a raptormark module always needs WASI")
	}
	t.Logf("%d imports", len(got))

	known := append(slices.Clone(stdPreview1), wasmEdgeSockets...)
	for _, imp := range got {
		mod, field, ok := strings.Cut(imp, ".")
		if !ok || mod != "wasi_snapshot_preview1" {
			t.Errorf("%s: imports from a namespace other than wasi_snapshot_preview1. "+
				"Every host that runs this module has to provide it; adding one is a "+
				"portability decision, not a detail.", imp)
			continue
		}
		if !slices.Contains(known, field) {
			t.Errorf("%s: a host requirement nobody has recorded. If it is standard "+
				"preview1, add it to stdPreview1; if it is a WasmEdge extension, add it "+
				"to wasmEdgeSockets and note that it narrows portability further.", imp)
		}
	}

	// ⚠️ THE PROPERTY THAT LETS ONE OBJECT SET SERVE BOTH LINK PATHS. Every
	// object is compiled `-fPIC` so the same bytes can also be linked
	// `--experimental-pic -shared` for an embedder (MULTIMODULE.md §5). That is
	// only free if the ORDINARY link is unchanged by it -- and the way it would
	// show if it were not is exactly here: a PIC object linked as a side module
	// imports `__memory_base` and `__table_base`, and a stock runwasi shim
	// supplies neither. wasm-ld resolves them internally in a static link, so
	// they must not appear.
	for _, forbidden := range []string{"__memory_base", "__table_base"} {
		for _, imp := range got {
			if strings.HasSuffix(imp, "."+forbidden) {
				t.Errorf("%s: the module was linked as a PIC SIDE module, not flat. "+
					"No stock shim provides this; the single-module artifact is broken.", imp)
			}
		}
	}

	// Reported, not asserted: which extensions this particular guest pulled in.
	// A guest that needed NONE would be runnable on a stock wasmtime, and that is
	// worth seeing rather than inferring.
	var ext []string
	for _, imp := range got {
		if _, field, ok := strings.Cut(imp, "."); ok && slices.Contains(wasmEdgeSockets, field) {
			ext = append(ext, field)
		}
	}
	if len(ext) == 0 {
		t.Logf("this module needs no WasmEdge extension -- a stock preview1 host could run it")
	} else {
		t.Logf("WasmEdge-only imports (%d): %s", len(ext), strings.Join(ext, " "))
	}
}

// A guest that does nothing. The imports under test come from ecvisor, which is
// linked into every module whatever the guest does, so the smallest possible
// guest is the cheapest way to ask the question.
const importProbeGuestSrc = `int main(void) { return 0; }
`
