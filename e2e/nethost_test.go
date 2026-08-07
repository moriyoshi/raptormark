package e2e

import (
	"context"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The ecvisor halves of the two COORDINATING net guests, which could not exist
// while those guests used fixed ports.
//
// # Why these were blocked, and what unblocked them
//
// `netServerSrc` bound a fixed 47826 and `netClientSrc` dialled a fixed 47825.
// That was a deliberate choice, not an oversight -- `net_test.go` recorded that
// a server's job "is to be found at a known address by a peer it does not
// coordinate with". The obstacle was that a LIFTED guest runs inside a
// container, so its loopback is not the host's unless the container joins the
// host network -- and that shares the host's port space, which is exactly when a
// fixed port bites. `AGENTS.md` records two overlapping `go test ./e2e/` runs
// producing EADDRINUSE and a wall of unrelated-looking errnos.
//
// The guests now coordinate: the server announces its ephemeral port on stdout,
// and the client is TOLD one on argv. Nothing is fixed, so `--network host`
// costs nothing.
//
// # ⚠️ These are the only e2e guests whose peer is OUTSIDE the module
//
// `netforkserver` is its own peer across a fork, and every other socket test is
// guest-internal or driven by a host that owns both ends. Here the lifted
// guest's traffic crosses a real host loopback to an in-process Go peer, so what
// is exercised is the shipping socket backend against something that is not
// ecvisor -- the arrangement a deployed guest actually has.

// runWasmHostNet runs a lifted module under wasmedge on the HOST network,
// streaming stdout so a peer can coordinate with it mid-run.
//
// ❗ `--network host` AND a named container with a forced removal, for the
// reason `wasixnet_test.go` documents: cancelling the context kills the `docker`
// CLI, not the container it started, so a guest blocked in accept() survives
// `--rm` and holds a port. That leak is worst exactly when the suite is being
// used to find a socket bug.
func runWasmHostNet(t *testing.T, ctx context.Context, wasm string, guestArgs []string,
	peer func(nextLine func() string)) (string, error) {
	t.Helper()
	dir, err := filepath.Abs(filepath.Dir(wasm))
	if err != nil {
		t.Fatal(err)
	}
	name := "raptormark-e2e-nethost-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	script := "wasmedge --enable-all /out/" + filepath.Base(wasm)
	for _, a := range guestArgs {
		script += " " + a
	}
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--name", name,
		"--network", "host", "-v", dir+":/out", "--entrypoint", "bash",
		builderImage(), "--login", "-c", script)
	return streamPeer(t, ctx, cmd, peer)
}

// TestNetServerUnderEcvisor: the lifted guest LISTENS and a host peer connects.
//
// The native baseline is `TestNetGuestsNativeContract/server_inbound`, running
// the same source with the same peer. Disagreement means one of them is wrong
// about Linux, and the native one is the half that can be trusted.
func TestNetServerUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "netsrvecv", netServerSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "netsrvecv")

	var reply []byte
	out, err := runWasmHostNet(t, ctx, wasm, nil, func(nextLine func() string) {
		// The guest's own PORT line is both the address and the readiness
		// signal. Under a container there is no other way to know it is up --
		// and no fixed port to guess.
		c := dialWithRetry(t, "127.0.0.1:"+portFromStream(t, nextLine))
		defer c.Close()
		if _, werr := c.Write([]byte("hello")); werr != nil {
			t.Errorf("writing to the lifted server: %v", werr)
			return
		}
		reply = readN(t, c, 4)
	})
	if err != nil {
		t.Fatalf("the lifted server failed: %v\n%s", err, out)
	}
	if string(reply) != "pong" {
		t.Errorf("the lifted server replied %q, want %q", reply, "pong")
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("the lifted server did not echo the request to stdout; got:\n%s", out)
	}
}

// TestNetClientUnderEcvisor: the lifted guest DIALS OUT to a host peer.
//
// ❗ This is the direction `net::wasmedge`'s `connect` is otherwise unexercised
// in: every other socket test either binds and accepts, or connects to a
// listener inside the same guest. Here `sock_connect` reaches a real listener
// that ecvisor does not own.
func TestNetClientUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "netcliecv", netClientSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "netcliecv")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening for the lifted client: %v", err)
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	var got []byte
	out, runErr := runWasmHostNet(t, ctx, wasm, []string{port}, func(func() string) {
		c, aerr := ln.Accept()
		if aerr != nil {
			t.Errorf("accepting from the lifted client: %v", aerr)
			return
		}
		defer c.Close()
		got = readN(t, c, 4)
		if _, werr := c.Write([]byte("pong")); werr != nil {
			t.Errorf("replying to the lifted client: %v", werr)
		}
	})
	if runErr != nil {
		t.Fatalf("the lifted client failed: %v\n%s", runErr, out)
	}
	if string(got) != "ping" {
		t.Errorf("the lifted client sent %q, want %q", got, "ping")
	}
	if !strings.Contains(out, "pong") {
		t.Errorf("the lifted client did not print the reply; got:\n%s", out)
	}
}

// portFromStream finds the guest's `PORT <n>` line among the runtime's own
// chatter.
//
// ❗ WHY THE NATIVE HARNESS NEEDS NO EQUIVALENT. There, the guest is the only
// writer. Under a wasm runtime the ENGINE shares the stream: `wasmedge
// --enable-all` opens with "component model is enabled, this is experimental",
// which is what the first version of this test mistook for the port and failed
// on. `assertForkServerOK` anticipated the same hazard for its banner.
//
// ⚠️ BOUNDED, not a loop-until-found. Scanning forever would turn a guest that
// never announces a port into a hang, and a hang is the one failure shape that
// carries no diagnostic. Twenty lines is far more chatter than any runtime here
// emits, and `nextLine` itself fails on EOF or timeout.
func portFromStream(t *testing.T, next func() string) string {
	t.Helper()
	for i := 0; i < 20; i++ {
		line := next()
		if strings.HasPrefix(line, "PORT ") {
			return portFrom(t, line)
		}
		t.Logf("skipping runtime output before the guest's PORT line: %q", line)
	}
	t.Fatalf("read 20 lines without finding the guest's `PORT <n>` announcement. " +
		"Either the guest is not reaching its getsockname/printf, or the runtime " +
		"is emitting far more than expected on stdout.")
	return ""
}
