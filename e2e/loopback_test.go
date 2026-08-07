package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"raptormark/internal/oci"
)

// The loopback profile links the SAME runtime with `net-loopback` instead of
// `net-wasmedge`, so its network is a pure-Rust in-process one that calls no
// socket import at all.
//
// ⚠️ THIS IS THE ACCEPTANCE TEST FOR THE BACKEND SEAM, and it is the only kind
// of evidence that counts. `runtime/src/net` claims that selecting a backend at
// COMPILE time -- a `cfg`-chosen type alias rather than a `dyn` trait object --
// keeps the unused backend's imports out of the module. That claim is about
// what wasm-ld emits, so it can only be settled by reading the import section
// of a real linked module. Reasoning about vtables, counting symbols in a
// staticlib, or grepping the source all stop short of it.
//
// It also closes a standing item in `.agents/docs/TODO.md`: every module was
// WasmEdge-bound, including ones with no sockets, and that is why
// `containerd-shim-wasmtime` can load a raptormark module but not run it.

// wasmEdgeOnly is the eleven-name extension surface, restated here rather than
// imported from imports_test.go's `wasmEdgeSockets` on purpose: this test's
// whole job is to assert that these are ABSENT, so it should not share a list
// with the test that asserts they are permitted. If the two ever disagree,
// TestLoopbackAndWasmEdgeDisagreeExactlyOnSockets fails.
var wasmEdgeOnly = []string{
	"sock_bind", "sock_connect", "sock_getlocaladdr", "sock_getpeeraddr",
	"sock_getsockopt", "sock_listen", "sock_open", "sock_recv_from",
	"sock_send_to", "sock_setsockopt", "sock_shutdown",
}

// importsOf links the probe guest under `profile` and returns its import set.
func importsOf(t *testing.T, profile string) []string {
	t.Helper()
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	name := "probe" + profile
	elf := compileGuest(t, ctx, dir, name, importProbeGuestSrc)
	var extra []string
	if profile != "wasmedge" {
		extra = append(extra, "--profile", profile)
	}
	wasm := liftOne(t, ctx, img, dir, elf, name, extra...)

	b, err := os.ReadFile(wasm)
	if err != nil {
		t.Fatal(err)
	}
	got, err := oci.ImportSet(b)
	if err != nil {
		t.Fatalf("reading the %s module's imports: %v", profile, err)
	}
	if len(got) == 0 {
		// Without this the test passes on a parser that silently found nothing,
		// which is the shape an absence assertion cannot otherwise detect --
		// "no WasmEdge imports" is trivially true of an empty set.
		t.Fatalf("no imports found in the %s module; a raptormark module always needs WASI", profile)
	}
	t.Logf("%s profile: %d imports", profile, len(got))
	return got
}

func fields(imports []string) []string {
	out := make([]string, 0, len(imports))
	for _, imp := range imports {
		if _, f, ok := strings.Cut(imp, "."); ok {
			out = append(out, f)
		}
	}
	slices.Sort(out)
	return out
}

// TestLoopbackProfileImportsNoSocketExtension is the claim itself.
func TestLoopbackProfileImportsNoSocketExtension(t *testing.T) {
	got := importsOf(t, "loopback")

	var leaked []string
	for _, f := range fields(got) {
		if slices.Contains(wasmEdgeOnly, f) {
			leaked = append(leaked, f)
		}
	}
	if len(leaked) > 0 {
		t.Errorf("the loopback profile still imports %d WasmEdge socket extension(s): %s\n"+
			"The backend seam did not exclude the unused backend. The usual cause is a "+
			"runtime selection creeping back in -- a `dyn NetBackend`, or an enum with a "+
			"match -- either of which keeps every impl live and puts every import set in "+
			"the module. See runtime/src/net/mod.rs.",
			len(leaked), strings.Join(leaked, " "))
	}

	// Every import must still be something a stock preview1 host provides.
	for _, imp := range got {
		mod, field, ok := strings.Cut(imp, ".")
		if !ok || mod != "wasi_snapshot_preview1" {
			t.Errorf("%s: imports from a namespace other than wasi_snapshot_preview1", imp)
			continue
		}
		if !slices.Contains(stdPreview1, field) {
			t.Errorf("%s: not in stdPreview1, so this profile is not stock-host-runnable "+
				"after all. Either it is standard preview1 and belongs on that list, or "+
				"the loopback backend has grown a host dependency it should not have.", imp)
		}
	}
}

// TestLoopbackAndWasmEdgeDisagreeExactlyOnSockets is the differential, and it
// is what stops the test above from passing for the wrong reason.
//
// ⚠️ What would a pass look like if the claim were false? If `--profile
// loopback` were silently ignored -- a typo in the flag, a wrong archive path,
// an image built before the loopback archive existed -- the loopback module
// would simply BE the wasmedge module. It would then import the eleven
// extensions and the test above would fail loudly... unless the flag failure
// also happened to drop them, which is exactly the case this pins: the two
// profiles must produce DIFFERENT import sets, and the difference must be
// precisely the socket extension.
func TestLoopbackAndWasmEdgeDisagreeExactlyOnSockets(t *testing.T) {
	edge := fields(importsOf(t, "wasmedge"))
	loop := fields(importsOf(t, "loopback"))

	if slices.Equal(edge, loop) {
		t.Fatalf("both profiles produced the SAME import set (%d names), so `--profile "+
			"loopback` had no effect and nothing here was tested. Check that the builder "+
			"image contains /opt/ecvisor/loopback/libecvisor.a.\n%s",
			len(edge), strings.Join(edge, " "))
	}

	// What wasmedge has and loopback does not must be exactly the WasmEdge
	// backend's whole host surface -- which is the eleven extension names PLUS
	// two STANDARD preview1 ones that only that backend calls:
	//
	//   fd_fdstat_set_flags   forces each host socket non-blocking, so a
	//                         would-block suspends the guest cooperatively.
	//                         An in-process network has nothing to set.
	//   sock_accept           the standardized 3-arg accept. Standard, so it
	//                         never blocked portability, but it is still a
	//                         socket call and the loopback backend makes none.
	//
	// Measured, not assumed: the seam removes THIRTEEN imports, not eleven. The
	// first version of this test expected eleven and failed, which is how the
	// other two were found.
	var onlyEdge []string
	for _, f := range edge {
		if !slices.Contains(loop, f) {
			onlyEdge = append(onlyEdge, f)
		}
	}
	slices.Sort(onlyEdge)
	want := append(slices.Clone(wasmEdgeOnly), "fd_fdstat_set_flags", "sock_accept")
	slices.Sort(want)
	if !slices.Equal(onlyEdge, want) {
		t.Errorf("the difference between the profiles is not the WasmEdge backend's "+
			"host surface.\nonly in wasmedge: %s\nexpected:         %s",
			strings.Join(onlyEdge, " "), strings.Join(want, " "))
	}

	// And loopback must not have gained anything wasmedge lacks.
	for _, f := range loop {
		if !slices.Contains(edge, f) {
			t.Errorf("the loopback profile imports %q, which the wasmedge profile does not. "+
				"A backend must not ADD host requirements.", f)
		}
	}
}

// TestLoopbackModuleRunsOnStockWasmtime is the payoff.
//
// wasmtime has always been able to DECODE a raptormark module -- the blocker is
// that it cannot satisfy `wasi_snapshot_preview1::sock_open` and friends, which
// is a different failure from the proposal rejection that used to happen and is
// documented as such in README.md. A module with none of those imports should
// therefore run, and this is the first time one can be built.
func TestLoopbackModuleRunsOnStockWasmtime(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	const banner = "LOOPBACK-OK"
	elf := compileGuest(t, ctx, dir, "lbrun", fmt.Sprintf(guestSrc, banner))
	wasm := liftOne(t, ctx, img, dir, elf, "lbrun", "--profile", "loopback")

	// The module lands in liftOne's own output directory, not `dir`.
	absDir, err := filepath.Abs(filepath.Dir(wasm))
	if err != nil {
		t.Fatal(err)
	}
	// No `--enable-all`: the point is a STOCK host. If this needs a flag, the
	// module is not as portable as the test claims.
	out, err := dockerRun(ctx, []string{"-v", absDir + ":/out"},
		"wasmtime run /out/"+filepath.Base(wasm))
	if err != nil {
		t.Fatalf("the loopback module did not run under stock wasmtime: %v\n%s\n"+
			"If this names an unknown import, the seam leaked one.", err, out)
	}
	if !strings.Contains(out, banner) {
		t.Errorf("wasmtime ran the module but the guest did not print %q; full output:\n%s",
			banner, out)
	}
}
