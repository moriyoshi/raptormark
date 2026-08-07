package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestLoopbackProfileServesGuestLocalSockets asks whether the loopback profile
// is merely IMPORT-clean or actually functional.
//
// It reuses nonblockGuestSrc, whose whole shape is a single process binding
// 127.0.0.1, connecting to its own listener, accepting, and exchanging bytes --
// which is exactly what an in-process network can serve, and which also pins
// EAGAIN semantics and a refused connect against real kernel behaviour
// (TestNonblockingSocketNativeBaseline).
//
// ⚠️ Without this, "15 imports" could describe a backend that answers ENOSYS to
// everything. That would satisfy every other check in loopback_test.go.
func TestLoopbackProfileServesGuestLocalSockets(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "lbnbsock", nonblockGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "lbnbsock", "--profile", "loopback")

	absDir, err := filepath.Abs(filepath.Dir(wasm))
	if err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx, []string{"-v", absDir + ":/out"},
		"wasmtime run /out/"+filepath.Base(wasm))
	if err != nil {
		t.Fatalf("the loopback module failed under stock wasmtime: %v\n%s", err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "NBSOCK-OK") {
		t.Errorf("the guest did not reach NBSOCK-OK under the loopback profile; output:\n%s", out)
	}
}
