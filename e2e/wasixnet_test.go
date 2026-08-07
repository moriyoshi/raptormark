package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"raptormark/internal/oci"
	"raptormark/internal/pipeline"
)

// The wasix profile: WASIX sockets from wasmer's `wasix_32v1` namespace.
//
// ⚠️ WHY A RUN AND NOT JUST AN IMPORT COUNT. This tree's own LTM records
// "import count alone is not behavioral evidence", and it is exactly right
// here: every mistake the WASIX ABI invites produces a module whose imports are
// perfect. A byte-swapped port binds successfully to the wrong endpoint. A
// zero-timeout `poll_oneoff` hangs instead of polling. `sock_accept` declared
// with preview1's three parameters fails at instantiation -- that one IS loud,
// and it is the only one that is. So the import test below bounds the host
// surface, and the runs are what decide whether the backend works.
//
// ❗ AND `--net`. Without it `sock_open` SUCCEEDS and `sock_bind` returns errno
// 58, so a run that forgot it fails to bind an address that is perfectly fine.
// `runWasmer` gets its argv FROM `internal/pipeline.runtimeArgs` rather than
// spelling it again, so these runs and `raptormark run --runtime wasmer` cannot
// disagree about the flags -- see the note on `runWasmer`.

// The socket names a wasix-profile module must import, and the namespace they
// come from. Restated here rather than shared with the backend's source for the
// same reason `loopback_test.go` restates `wasmEdgeOnly`: a test that imports
// its expectations from the thing under test cannot disagree with it.
const wasixNS = "wasix_32v1"

var wasixSocketImports = []string{
	"sock_open", "sock_bind", "sock_listen", "sock_connect", "sock_accept",
	"sock_recv", "sock_send", "sock_recv_from", "sock_send_to",
	"sock_addr_local", "sock_addr_peer", "sock_shutdown",
	"sock_set_opt_flag", "sock_get_opt_flag",
	"sock_set_opt_size", "sock_get_opt_size",
}

// requireWasmer skips unless the wasmer container is available.
//
// ⚠️ wasmer is deliberately NOT a build dependency and is on neither the host
// nor the builder image, so this is a separate gate from RAPTORMARK_E2E rather
// than a part of it -- a machine with a builder image still has no wasmer.
// `.agents-workspace/wasmer/Dockerfile` builds the image.
func requireWasmer(t *testing.T) string {
	t.Helper()
	if os.Getenv("RAPTORMARK_E2E_WASMER") != "1" {
		t.Skip("set RAPTORMARK_E2E_WASMER=1 to run the wasmer end-to-end tests")
	}
	img := os.Getenv("RAPTORMARK_WASMER_IMAGE")
	if img == "" {
		img = "raptormark-wasmer:7.3.0"
	}
	if _, err := exec.Command("docker", "image", "inspect", "--format", "{{.Id}}", img).Output(); err != nil {
		t.Skipf("wasmer image %s not present; build it from .agents-workspace/wasmer/Dockerfile "+
			"or set RAPTORMARK_WASMER_IMAGE", img)
	}
	t.Logf("wasmer image %s", img)
	return img
}

// runWasmer runs one module under wasmer and returns its combined output.
//
// ⚠️ `--network host` is not a convenience. The guests below bind and dial
// fixed ports on 127.0.0.1 and their peer is the Go test process on the HOST;
// in the container's own network namespace the two loopbacks are different
// machines and every connection is refused, which reads exactly like a broken
// backend.
//
// ❗ IT ALSO MEANS TWO CONCURRENT `go test ./e2e/` INVOCATIONS COLLIDE, and the
// collision does not look like one. `nonblockGuestSrc` binds a FIXED 39117 and
// the socket guests bind 47825/47826, so a second run in flight makes the first
// guest's `bind` return EADDRINUSE -- after which every later check in that
// guest fails too, and the output is a wall of unrelated-looking errnos
// (`accept should be EAGAIN (errno=5)`, `recv should be EAGAIN (errno=88)`)
// with the one that matters, `FAIL bind (errno=98)`, at the top.
//
// Measured the hard way: a suite run concurrent with a `-run TestWasix` run.
// Sharing the host's port space is what `--network host` buys and what it
// costs. Run one suite at a time.
//
// ⚠️ `--volume HOST:/` is wasmer's spelling: the wasmtime ORDER with the
// wasmedge SEPARATOR. Its `--mapdir GUEST:HOST` is deprecated in 7.3.0.
// ❗ `--name` PLUS A `docker rm -f`, BECAUSE `--rm` AND THE CONTEXT ARE NOT
// ENOUGH. Cancelling the context kills the `docker` CLI, not the container it
// started: the guest keeps running detached and `--rm` never fires, because
// nothing ever exits. Measured while neutralizing this file -- two wasmer
// containers were still up 38 minutes after their tests had finished, each
// holding a guest blocked in a connect that could not succeed.
//
// A hung guest is the NORMAL outcome of a broken socket backend, which is
// exactly when this runs, so the leak would be worst precisely when the suite
// is being used to find one.
func runWasmer(t *testing.T, ctx context.Context, img, dir, wasm string, guestArgs ...string) (string, error) {
	t.Helper()
	// ❗ THE ARGV COMES FROM `runtimeArgs`, NOT FROM A COPY OF IT. Spelling the
	// flags here would give two strings proven against two different things --
	// the unit test pins what `runtimeArgs` produces, this proves a real wasmer
	// accepts what THIS passes -- with nothing comparing them. `--volume` could
	// regress to the deprecated `--mapdir`, or lose `--net`, and both tests
	// would stay green while `raptormark run --runtime wasmer` was broken.
	//
	// The container is where the paths are: the module directory is mounted at
	// /out, so those are the arguments the command under test is given.
	argv := pipeline.RuntimeArgsForTest(
		"wasmer", "/out", "/out/"+filepath.Base(wasm), nil, guestArgs)
	script := "wasmer " + strings.Join(argv, " ")
	// Unique per call: subtests share a test name prefix and one run may still
	// be shutting down when the next starts.
	name := fmt.Sprintf("raptormark-e2e-wasmer-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		// Best effort and deliberately silent: the ordinary case is that the
		// container is already gone, and `docker rm` says so on stderr.
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})
	args := []string{
		"run", "--rm", "--name", name, "--network", "host",
		"-v", dir + ":/out", "--entrypoint", "bash", img, "--login", "-c", script,
	}
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return string(out), err
}

// TestWasixProfileImportsItsOwnNamespace bounds what a wasmer host must supply.
//
// Standard preview1 cannot CREATE an outbound socket, so a separate namespace
// is not a preference here any more than it is for the browser profile -- it is
// the only way to express a network at all. A host missing a name then fails at
// INSTANTIATION, loudly, rather than at the first connect.
func TestWasixProfileImportsItsOwnNamespace(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "wxprobe", importProbeGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "wxprobe", "--profile", "wasix")

	b, err := os.ReadFile(wasm)
	if err != nil {
		t.Fatal(err)
	}
	got, err := oci.ImportSet(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no imports found; a raptormark module always needs WASI")
	}
	t.Logf("wasix profile: %d imports", len(got))

	var wasix, wasmEdge, other []string
	for _, imp := range got {
		mod, field, ok := strings.Cut(imp, ".")
		if !ok {
			continue
		}
		switch mod {
		case wasixNS:
			wasix = append(wasix, field)
		case "wasi_snapshot_preview1":
			if slices.Contains(wasmEdgeOnly, field) {
				wasmEdge = append(wasmEdge, field)
			}
		default:
			other = append(other, imp)
		}
	}
	slices.Sort(wasix)
	t.Logf("%s: %s", wasixNS, strings.Join(wasix, " "))

	// ⚠️ THE POSITIVE CONTROL FIRST. Every assertion below is an ABSENCE, and
	// absences all pass against a module that failed to link the backend at
	// all. This is what makes the rest mean something.
	if len(wasix) == 0 {
		t.Fatalf("the wasix profile imports NOTHING from %s, so the backend was "+
			"not linked in and every check below would pass vacuously", wasixNS)
	}
	for _, want := range wasixSocketImports {
		if !slices.Contains(wasix, want) {
			t.Errorf("missing import %s.%s", wasixNS, want)
		}
	}

	if len(wasmEdge) > 0 {
		t.Errorf("the wasix profile still imports WasmEdge socket extensions: %s. "+
			"Both backends are live, which is what a `dyn` or an enum would cause -- "+
			"and what `runtime/src/net/mod.rs` selects by cfg to prevent.",
			strings.Join(wasmEdge, " "))
	}
	// ⚠️ `env.*` in particular. A flat link DEFINES `__ecv_unwinding` rather
	// than importing it, which is the whole reason `--profile wasix` alone does
	// not imply `--suspend-via-call` (see `pipeline.suspendViaCallFor`). If one
	// ever shows up here, that reasoning is wrong and the object cache is being
	// thrown away for nothing -- or kept when it should not be.
	for _, imp := range other {
		t.Errorf("%s: a third namespace nobody recorded. If this is env.*, a flat "+
			"module now imports a global and suspendViaCallFor needs revisiting.", imp)
	}
}

// TestWasixModuleRunsUnderWasmer is instantiation plus a first instruction: the
// cheapest run that can fail, and the one that catches a mis-declared import.
//
// ⚠️ A WRONG ARITY FAILS HERE AND ONLY HERE. `wasix_32v1.sock_accept` is bound
// to `sock_accept_v2` and takes FOUR parameters, not the three the preview1
// function of the same name takes. Declaring it wrong produces a module that
// links, passes every archive-level exclusion test, imports exactly the right
// NAMES -- and refuses to instantiate.
func TestWasixModuleRunsUnderWasmer(t *testing.T) {
	img := requireE2E(t)
	wimg := requireWasmer(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	const banner = "WASIX-OK"
	elf := compileGuest(t, ctx, dir, "wxrun", fmt.Sprintf(guestSrc, banner))
	wasm := liftOne(t, ctx, img, dir, elf, "wxrun", "--profile", "wasix")

	absDir, err := filepath.Abs(filepath.Dir(wasm))
	if err != nil {
		t.Fatal(err)
	}
	out, err := runWasmer(t, ctx, wimg, absDir, wasm)
	if err != nil {
		t.Fatalf("the wasix module did not run under wasmer: %v\n%s\n"+
			"If this names an incompatible import type, an extern declaration in "+
			"runtime/src/net/wasix.rs disagrees with .agents/docs/WASIX_ABI.md. "+
			"Remember the wording is inverted: \"Expected\" is OURS.", err, out)
	}
	if !strings.Contains(out, banner) {
		t.Errorf("wasmer ran the module but the guest did not print %q; full output:\n%s",
			banner, out)
	}
}

// ⚠️ `epollSocketGuestSrc` MOVED to `e2e/epollsock_test.go` on 2026-08-25, and
// this note is here because the guest was written for THIS backend and a reader
// debugging `net::wasix` will look for it here first.
//
// It moved because it stopped being wasix-specific: `NetBackend::ready` has one
// caller -- `fd_ready`'s socket arm -- and this guest was the only thing in
// `e2e/` that reached it on ANY profile, so covering the shipping profile meant
// a second caller. A second caller is what should move a fixture rather than
// copy it.
//
// ⚠️ The other tests in this file cannot catch what it catches. They block in
// `accept` and `connect`, which the scheduler serves through `wait` -- a
// different call with a different timeout. `nonblockGuestSrc` never polls at
// all: it is EAGAIN-driven throughout (checked -- there is no poll or epoll in
// it, only a comment mentioning nginx's).

// TestWasixProfileEpollsASocketWithoutHanging is the guard for `PROBE_NANOS`.
// See `epollSocketGuestSrc`, now in `epollsock_test.go`, for why nothing else
// covers it.
func TestWasixProfileEpollsASocketWithoutHanging(t *testing.T) {
	img := requireE2E(t)
	wimg := requireWasmer(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "wxepoll", epollSocketGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "wxepoll", "--profile", "wasix")
	absDir, err := filepath.Abs(filepath.Dir(wasm))
	if err != nil {
		t.Fatal(err)
	}

	out, err := runWasmer(t, ctx, wimg, absDir, wasm)
	if err != nil {
		t.Fatalf("the epoll guest failed under wasmer: %v\n%s\n"+
			"If it produced no output at all, the readiness probe BLOCKED -- "+
			"check PROBE_NANOS in runtime/src/net/wasix.rs, which must be 1 and "+
			"not the 0 that net::wasmedge correctly uses for preview1.", err, out)
	}
	assertEpollSocketOK(t, "wasix", out)
}

// The DATAGRAM path is covered by reusing `udpGuestSrc` from
// `nodehost_test.go` rather than by a second guest of my own.
//
// ❗ WHY THIS PATH IS NOT OPTIONAL FOR THIS BACKEND. `net::wasix` deliberately
// does NOT intercept DNS the way `net::browser` must, and the entire
// justification is that WASIX has real datagram sockets, so a guest's own
// resolver works unmodified. That claim IS this code path. Untested, "we do not
// need `net::dns` here" would be an assertion about something nothing had run.
//
// It also puts a third caller on `wasix_addr::decode`: the source address
// `recvfrom` reports goes through the same big-endian read as
// `sock_addr_local`. And that guest does not merely compare it -- it REPLIES to
// it, so a wrongly decoded source makes the reply go nowhere and the second
// `recvfrom` never completes. A codec that byte-swapped the port would pass a
// comparison against its own encoder and fail this.
//
// ⚠️ Written for the Node host and reused unchanged, which is the point: it
// encodes what a real Linux does, not what any one backend does.

// TestWasixProfileCarriesDatagrams is the evidence for "no DNS interception
// needed here". See `udpGuestSrc`.
func TestWasixProfileCarriesDatagrams(t *testing.T) {
	img := requireE2E(t)
	wimg := requireWasmer(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "wxudp", udpGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "wxudp", "--profile", "wasix")
	absDir, err := filepath.Abs(filepath.Dir(wasm))
	if err != nil {
		t.Fatal(err)
	}

	out, err := runWasmer(t, ctx, wimg, absDir, wasm)
	if err != nil {
		t.Fatalf("the datagram guest failed under wasmer: %v\n%s", err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "UDP-OK") {
		t.Errorf("the guest did not reach UDP-OK; full output:\n%s", out)
	}
}

// TestWasixProfileHonoursNonblockingSemantics is the guard `QUALITY_GATE.md` §5
// names for any socket change, run against this backend.
//
// It is self-contained -- one process binding, connecting and reading its own
// loopback -- so nothing here can be flaky for a reason outside the guest. And
// its expectations are pinned to a real Linux kernel by
// `TestNonblockingSocketNativeBaseline`, so passing means FAITHFUL rather than
// merely self-consistent, which is the property an ABI written from a
// measurement most needs checked.
//
// ⚠️ It is EAGAIN-driven and never polls, so it does not reach
// `NetBackend::ready` -- `TestWasixProfileEpollsASocketWithoutHanging` above is
// what covers that.
func TestWasixProfileHonoursNonblockingSemantics(t *testing.T) {
	img := requireE2E(t)
	wimg := requireWasmer(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "wxnbsock", nonblockGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "wxnbsock", "--profile", "wasix")
	absDir, err := filepath.Abs(filepath.Dir(wasm))
	if err != nil {
		t.Fatal(err)
	}

	out, err := runWasmer(t, ctx, wimg, absDir, wasm)
	if err != nil {
		t.Fatalf("the nonblocking guest failed under wasmer: %v\n%s", err, out)
	}
	assertNonblockGuestPassed(t, out)
}

// TestWasixProfileServesTheSocketABI is the one that decides.
//
// It reuses the two kernel-pinned guests from `net_test.go` -- whose native
// behaviour `TestNetGuestsNativeContract` fixes against a real Linux -- so
// passing means faithful rather than merely self-consistent. Between them they
// cover both directions, which matters because the WASIX address codec is
// ASYMMETRIC: the port is little-endian in a bind or connect and big-endian in
// everything that reads one back. An inbound-only test would pass with the
// decode side entirely wrong, and an outbound-only test with the encode side
// wrong, because a byte-swapped port is still a valid port.
func TestWasixProfileServesTheSocketABI(t *testing.T) {
	img := requireE2E(t)
	wimg := requireWasmer(t)
	ctx := ctxFor(t)

	t.Run("server_inbound", func(t *testing.T) {
		dir := t.TempDir()
		elf := compileGuest(t, ctx, dir, "wxserver", netServerSrc)
		wasm := liftOne(t, ctx, img, dir, elf, "wxserver", "--profile", "wasix")
		absDir, err := filepath.Abs(filepath.Dir(wasm))
		if err != nil {
			t.Fatal(err)
		}

		// ⚠️ THE PORT IS THE ASSERTION. The guest is told to bind 47826; if the
		// encode side byte-swapped it, the bind would SUCCEED on 26810 and this
		// dial would time out -- a failure that looks like a dead guest and is
		// actually a codec one byte-order out.
		//
		// ❗ THE PORT IS PASSED ON ARGV, AND IT MUST BE. `netServerSrc` gained an
		// optional port on 2026-08-25 when the native pair was rewritten to
		// coordinate; with no argument it binds 0. That mode is right for the
		// coordinating harnesses and WRONG here: `bind(0)` hands the port choice
		// to the host, so there is no port to encode and this assertion would
		// silently test nothing. Fixed-port binding is the only way to exercise
		// the encode side.
		type result struct {
			out   string
			err   error
			reply []byte
		}
		done := make(chan result, 1)
		go func() {
			out, err := runWasmer(t, ctx, wimg, absDir, wasm, "47826")
			done <- result{out: out, err: err}
		}()

		c := dialWithRetry(t, "127.0.0.1:47826")
		if _, err := c.Write([]byte("hello")); err != nil {
			c.Close()
			t.Fatalf("writing to the guest: %v", err)
		}
		reply := readN(t, c, 4)
		c.Close()

		r := <-done
		if r.err != nil {
			t.Fatalf("the wasix server guest failed: %v\n%s", r.err, r.out)
		}
		if string(reply) != "pong" {
			t.Errorf("the guest replied %q, want %q", reply, "pong")
		}
		if !strings.Contains(r.out, "hello") {
			t.Errorf("the guest did not echo the request; output:\n%s", r.out)
		}
	})

	t.Run("client_outbound", func(t *testing.T) {
		dir := t.TempDir()
		elf := compileGuest(t, ctx, dir, "wxclient", netClientSrc)
		wasm := liftOne(t, ctx, img, dir, elf, "wxclient", "--profile", "wasix")
		absDir, err := filepath.Abs(filepath.Dir(wasm))
		if err != nil {
			t.Fatal(err)
		}

		ln, err := net.Listen("tcp", "127.0.0.1:47825")
		if err != nil {
			t.Fatalf("listening for the guest: %v", err)
		}
		defer ln.Close()
		// ❗ A DEADLINE, OR A BROKEN BACKEND HANGS THE SUITE INSTEAD OF FAILING
		// IT. Found by neutralizing: with the address codec's port byte-swapped
		// the guest dials 26810 instead of 47825, nothing ever arrives, and
		// `Accept` blocks until the whole `go test` timeout expires. The inbound
		// subtest reports the same defect in 17 seconds because `dialWithRetry`
		// gives up; this side had nothing to give up on.
		if d, ok := ln.(*net.TCPListener); ok {
			if err := d.SetDeadline(time.Now().Add(60 * time.Second)); err != nil {
				t.Fatal(err)
			}
		}

		type result struct {
			out string
			err error
		}
		done := make(chan result, 1)
		go func() {
			// ❗ THE PORT IS PASSED ON ARGV. `netClientSrc` stopped hard-coding
			// 47825 on 2026-08-25 and now REFUSES to default -- a default would
			// let a misconfigured run dial something plausible and fail elsewhere.
			// 47825 is kept here rather than made ephemeral because the assertion
			// below is about the codec: a byte-swapped port sends the guest to
			// 26810, and a known number is what makes that legible.
			out, err := runWasmer(t, ctx, wimg, absDir, wasm, "47825")
			done <- result{out: out, err: err}
		}()

		c, err := ln.Accept()
		if err != nil {
			t.Fatalf("accepting from the guest: %v", err)
		}
		got := readN(t, c, 4)
		if _, err := c.Write([]byte("pong")); err != nil {
			t.Errorf("replying to the guest: %v", err)
		}
		c.Close()

		r := <-done
		if r.err != nil {
			t.Fatalf("the wasix client guest failed: %v\n%s", r.err, r.out)
		}
		if string(got) != "ping" {
			t.Errorf("the guest sent %q, want %q", got, "ping")
		}
		if !strings.Contains(r.out, "pong") {
			t.Errorf("the guest did not print our reply; output:\n%s", r.out)
		}
	})
}
