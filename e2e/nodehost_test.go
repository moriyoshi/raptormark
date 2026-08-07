package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"raptormark/internal/rootfs"
)

// The Node reference host (`web/`) is the only in-repo implementation of the 28
// imports a raptormark module needs.
//
// ⚠️ IT IS ALSO THE ONLY ONE THAT EXISTS AT ALL. `runtime/src/sys.rs:520` and
// `:568` both name `third_party/wazero/imports/wasi_snapshot_preview1/
// sock_wasmedge.go` as the reference host for the WasmEdge socket ABI. That
// path is not in this tree, is not in `.gitmodules` (which has one entry,
// elfconv), and is not in any commit. So before `web/`, the ABI was specified
// only by the Rust `extern` blocks that consume it, and nothing could check a
// host against it.
//
// These tests run on the HOST, not inside the builder image. The image is
// needed to BUILD a module, not to run a finished one, and its emsdk node is
// 20.18 -- which predates the type-stripping that lets node execute the .ts
// sources directly (>= 22.6). Copying a 100 MB artifact into a container to run
// it there would be slower and would buy nothing.
//
// ⚠️ PASS `-count=1` WHEN CHANGING `web/`. Go's test cache keys on Go sources
// and the files a test declares; it knows nothing about the TypeScript under
// `web/`. So a broken shim re-reports the previous PASS, from the cache, in
// under a second -- which is exactly the shape of "the fix didn't work" that
// turns out to be a stale artifact. Found while neutralizing these guards.

const minNodeMajor = 22
const minNodeMinor = 6

// requireNode returns a node binary new enough to execute TypeScript directly,
// or skips. `RAPTORMARK_NODE` overrides the lookup, matching the convention
// `RAPTORMARK_BUILDER` sets: never guess at a toolchain that has to be right.
func requireNode(t *testing.T) string {
	t.Helper()
	node := os.Getenv("RAPTORMARK_NODE")
	if node == "" {
		var err error
		if node, err = exec.LookPath("node"); err != nil {
			t.Skip("node not on PATH; set RAPTORMARK_NODE=<path> " +
				"(node >= 22.6 is required to run web/ without a build step)")
		}
	}
	out, err := exec.Command(node, "--version").Output()
	if err != nil {
		t.Skipf("%s --version failed: %v", node, err)
	}
	v := strings.TrimPrefix(strings.TrimSpace(string(out)), "v")
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		t.Skipf("cannot parse node version %q", v)
	}
	major, _ := strconv.Atoi(parts[0])
	minor, _ := strconv.Atoi(parts[1])
	if major < minNodeMajor || (major == minNodeMajor && minor < minNodeMinor) {
		t.Skipf("node %s is too old; >= %d.%d is required to execute web/'s TypeScript "+
			"without a build step", v, minNodeMajor, minNodeMinor)
	}
	t.Logf("node %s at %s", v, node)
	return node
}

// runNodeHost runs a module under the Node reference host and returns its
// combined output. `wasm` and the sidecar beside it are read straight off disk.
func runNodeHost(t *testing.T, ctx context.Context, node, wasm, sidecar string) string {
	t.Helper()
	runner, err := filepath.Abs(filepath.Join("..", "web", "bin", "run.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(runner); err != nil {
		t.Fatalf("the Node host is missing at %s: %v", runner, err)
	}
	return runNodeHostArgs(t, ctx, node, wasm, sidecar)
}

// runNodeHostArgs is `runNodeHost` with extra host flags, e.g. `--reentrant`.
func runNodeHostArgs(
	t *testing.T,
	ctx context.Context,
	node, wasm, sidecar string,
	extra ...string,
) string {
	t.Helper()
	runner, err := filepath.Abs(filepath.Join("..", "web", "bin", "run.ts"))
	if err != nil {
		t.Fatal(err)
	}
	args := []string{runner, "--module", wasm}
	if sidecar != "" {
		args = append(args, "--rootfs", sidecar)
	}
	args = append(args, extra...)
	// ⚠️ BOUND THE RUN, AND KEEP WHAT IT PRINTED. A wedged host is the most
	// likely way this harness fails -- the guest's scheduler and the socket
	// worker can each wait on the other -- and an unbounded run turns that into
	// a silent hang of the whole suite. Worse, node BUFFERS piped stdout, so a
	// killed process loses everything it printed and the failure arrives with no
	// evidence at all. Reading the pipes into a buffer we own keeps the partial
	// output, which is what names the operation that stalled.
	runCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, node, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		t.Errorf("the Node host did not finish within 90s -- it is wedged. "+
			"Output up to that point:\n%s", buf.String())
		return buf.String()
	}
	// A non-zero exit is the guest's own status and is reported by the caller;
	// only a failure to LAUNCH is fatal here.
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("launching the Node host: %v\n%s", err, buf.String())
		}
	}
	return buf.String()
}

// TestNodeHostRunsTheSameModuleAsWasmEdge is a DIFFERENTIAL, not an output
// check.
//
// ⚠️ What would a pass look like if the claim were false? A test that only
// asserted "NODEHOST-OK appears in the Node output" would pass against a host
// that got the sidecar wrong and let ecvisor fall back to host argv, against
// one whose stdout decoding dropped bytes it never received, and against one
// that differed from wasmedge in every way that this guest happens not to
// exercise. Comparing the two runs of the SAME module is what makes the
// assertion about fidelity rather than about liveness.
func TestNodeHostRunsTheSameModuleAsWasmEdge(t *testing.T) {
	img := requireE2E(t)
	node := requireNode(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "nodehost", fmt.Sprintf(guestSrc, "NODEHOST-OK"))
	wasm := liftOne(t, ctx, img, dir, elf, "nodehost")

	// A near-empty rootfs: the sidecar is here for the boot record, as in
	// TestMkdirModeUnderANonRootBootUID.
	root := t.TempDir()
	image, _, err := rootfs.Build(root, rootfs.Options{Boot: &rootfs.Boot{
		Argv: []string{"nodehost"},
		Cwd:  "/",
	}})
	if err != nil {
		t.Fatalf("building the rfs sidecar: %v", err)
	}
	sidecar := filepath.Join(filepath.Dir(wasm), "rootfs.img")
	if err := os.WriteFile(sidecar, image, 0o644); err != nil {
		t.Fatal(err)
	}

	edge := runWasmIn(t, ctx, wasm, nil, []string{"RAPTORMARK_ROOTFS=/rootfs.img"}, "/:/out")
	host := runNodeHost(t, ctx, node, wasm, sidecar)

	const want = "NODEHOST-OK"
	if !strings.Contains(edge, want) {
		t.Fatalf("wasmedge did not produce the baseline; full output:\n%s", edge)
	}
	if !strings.Contains(host, want) {
		t.Errorf("the Node host did not reproduce wasmedge's output.\n"+
			"wasmedge:\n%s\nnode:\n%s", edge, host)
	}
	if !strings.Contains(host, "HOST-EXIT: 0") {
		t.Errorf("the Node host reported a non-zero exit; full output:\n%s", host)
	}
	// A guest that reaches a socket call the host has not implemented is a
	// silent wrong answer otherwise: NullSockets returns ENOTSUP, which a guest
	// may well handle by taking a different path and still printing its banner.
	if strings.Contains(host, "HOST-NOTE:") {
		t.Errorf("the guest reached an unimplemented socket call; full output:\n%s", host)
	}
}

// TestNodeHostServesTheSocketABI is the point of the socket backend: the SAME
// guest that pins ecvisor's O_NONBLOCK contract against a real Linux kernel
// (`nonblockGuestSrc`, and `TestNonblockingSocketNativeBaseline` beside it) must
// also pass against Node sockets.
//
// ⚠️ This is the test that could not exist before. It reuses that guest rather
// than writing a friendlier one precisely because its expectations are already
// pinned to kernel behaviour, so passing it means the host is faithful rather
// than merely self-consistent. It covers, in one process: bind, listen,
// getsockname, accept returning EAGAIN on an empty non-blocking listener,
// connect completing through the backlog, accept4 propagating SOCK_NONBLOCK,
// recv returning EAGAIN on an empty connection, a real send/recv round trip,
// and -- the one most likely to be got wrong -- a connect to a dead port that
// must FAIL rather than be swallowed by the resumed-connect path.
//
// If it passed against a host that answered EAGAIN to everything, it would be
// worthless; check 4 (real data arrives) and check 2 (connect completes) are
// what make that impossible.
func TestNodeHostServesTheSocketABI(t *testing.T) {
	img := requireE2E(t)
	node := requireNode(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "nbsock", nonblockGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "nbsock")

	// No sidecar: this guest touches no files, and ecvisor boots without one.
	out := runNodeHost(t, ctx, node, wasm, "")
	assertNonblockGuestPassed(t, out)
	if !strings.Contains(out, "HOST-EXIT: 0") {
		t.Errorf("the guest exited non-zero under the Node host; full output:\n%s", out)
	}
}

// TestNodeHostSocketsMatchWasmEdge runs the socket guest under both hosts and
// requires the same verdict from each.
//
// Kept separate from the test above so a failure says which claim broke: that
// one asks "is the Node host correct against a kernel-pinned contract", this
// one asks "do the two hosts agree". A single combined test would report both
// as one failure and leave it ambiguous.
func TestNodeHostSocketsMatchWasmEdge(t *testing.T) {
	img := requireE2E(t)
	node := requireNode(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "nbsockcmp", nonblockGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "nbsockcmp")

	edge := runWasm(t, ctx, wasm)
	host := runNodeHost(t, ctx, node, wasm, "")

	edgeOK := strings.Contains(edge, "NBSOCK-OK")
	hostOK := strings.Contains(host, "NBSOCK-OK")
	if edgeOK != hostOK {
		t.Errorf("the two hosts disagree: wasmedge NBSOCK-OK=%v, node NBSOCK-OK=%v\n"+
			"wasmedge:\n%s\nnode:\n%s", edgeOK, hostOK, edge, host)
	}
	if !edgeOK {
		t.Fatalf("wasmedge did not pass, so there is no baseline to compare against:\n%s", edge)
	}
}

// udpGuestSrc exercises the sendto/recvfrom path, which no other guest in this
// tree touches.
//
// ⚠️ IT IS SPECIFICALLY A TEST OF THE ADDRESS FORM. sendto/recvfrom do NOT use
// the `WasiAddress` struct the other socket calls use: they pass a bare
// 128-byte buffer whose first two bytes are the address family as a
// little-endian u16, with the octets from offset 2 (`runtime/src/sys.rs`
// builds it at :5978-5981 and reads it back at :6056-6061). A host that treats
// it as raw octets is off by two bytes, which turns 127.0.0.1 into something
// else entirely.
//
// The round trip is what pins it: B sends to A's known port, then A replies to
// the source address recvfrom REPORTED. If that address were decoded wrongly,
// the reply goes nowhere and the second recvfrom never completes -- so the test
// fails by timing out rather than by comparing bytes, which is why the guest
// bounds its own wait instead of blocking forever.
const udpGuestSrc = `#define _GNU_SOURCE
#include <arpa/inet.h>
#include <errno.h>
#include <netinet/in.h>
#include <stdio.h>
#include <string.h>
#include <sys/socket.h>
#include <time.h>
#include <unistd.h>

static int failures = 0;
#define CHECK(cond, what) do { if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } } while (0)

int main(void) {
	int a = socket(AF_INET, SOCK_DGRAM, 0);
	CHECK(a >= 0, "socket a");
	int b = socket(AF_INET, SOCK_DGRAM, 0);
	CHECK(b >= 0, "socket b");

	struct sockaddr_in aa;
	memset(&aa, 0, sizeof aa);
	aa.sin_family = AF_INET;
	aa.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
	aa.sin_port = htons(39221);
	CHECK(bind(a, (struct sockaddr *)&aa, sizeof aa) == 0, "bind a");

	/* B must also be bound, or it has no source port for A to reply to. */
	struct sockaddr_in bb = aa;
	bb.sin_port = htons(39222);
	CHECK(bind(b, (struct sockaddr *)&bb, sizeof bb) == 0, "bind b");

	printf("check sendto\n");
	fflush(stdout);
	CHECK(sendto(b, "ping", 4, 0, (struct sockaddr *)&aa, sizeof aa) == 4, "sendto ping");

	printf("check recvfrom\n");
	fflush(stdout);
	char buf[64];
	struct sockaddr_in from;
	socklen_t flen = sizeof from;
	ssize_t n = -1;
	for (int i = 0; i < 400 && n < 0; i++) {
		memset(&from, 0, sizeof from);
		flen = sizeof from;
		n = recvfrom(a, buf, sizeof buf, 0, (struct sockaddr *)&from, &flen);
		if (n < 0) { struct timespec ts = { 0, 5 * 1000 * 1000 }; nanosleep(&ts, NULL); }
	}
	CHECK(n == 4 && memcmp(buf, "ping", 4) == 0, "recvfrom ping");
	/* The decoded source must be loopback and B's port. A two-byte shift shows
	   up here first, as a nonsense address. */
	CHECK(from.sin_family == AF_INET, "recvfrom family");
	CHECK(from.sin_addr.s_addr == htonl(INADDR_LOOPBACK), "recvfrom source address");
	CHECK(ntohs(from.sin_port) == 39222, "recvfrom source port");

	printf("check reply-to-source\n");
	fflush(stdout);
	CHECK(sendto(a, "pong", 4, 0, (struct sockaddr *)&from, flen) == 4, "sendto pong");
	n = -1;
	for (int i = 0; i < 400 && n < 0; i++) {
		n = recvfrom(b, buf, sizeof buf, 0, NULL, NULL);
		if (n < 0) { struct timespec ts = { 0, 5 * 1000 * 1000 }; nanosleep(&ts, NULL); }
	}
	CHECK(n == 4 && memcmp(buf, "pong", 4) == 0, "recvfrom pong");

	close(a);
	close(b);
	if (failures == 0) printf("UDP-OK\n");
	return failures == 0 ? 0 : 1;
}
`

// TestNodeHostServesDatagramsAndAddresses covers sendto/recvfrom, and with them
// the 128-byte family-prefixed address form. See udpGuestSrc.
func TestNodeHostServesDatagramsAndAddresses(t *testing.T) {
	img := requireE2E(t)
	node := requireNode(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "udpsock", udpGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "udpsock")

	out := runNodeHost(t, ctx, node, wasm, "")
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "UDP-OK") {
		t.Errorf("the guest did not reach UDP-OK; full output:\n%s", out)
	}
}

// A guest that reports what ecvisor gave it as argv[0].
//
// The banner guest cannot serve this test. It prints a constant, so its output
// is identical whether the sidecar was parsed, opened-and-ignored, or never
// found -- which is exactly what a first version of this test asserted, and it
// failed for that reason. What the boot record controls is argv, so argv is
// what has to be observed. Audit first, then write the test.
const argvGuestSrc = `#include <stdio.h>
int main(int argc, char **argv) { printf("ARGV0=%s\n", argc > 0 ? argv[0] : "(none)"); return 0; }
`

// TestNodeHostReadsTheSidecarBootRecord pins the one host interaction ecvisor
// actually has with a filesystem.
//
// `load_sidecar` (`runtime/src/entry.rs:320`) is a single `std::fs::read`, and
// it is the sole reason `path_open`, `fd_prestat_get`, `fd_prestat_dir_name`,
// `fd_filestat_get` and `fd_close` are imported at all.
//
// ⚠️ Serving the FILE is not the claim; PARSING it is. A host could satisfy
// every one of those five calls and still hand back bytes ecvisor cannot use,
// so the assertion is on a value that can only come from inside the image: the
// boot record's argv[0] (`entry.rs:43-52`). Without a sidecar ecvisor falls
// back to host argv, so the two runs must disagree, and they must disagree in
// the specific direction named here rather than merely differing.
func TestNodeHostReadsTheSidecarBootRecord(t *testing.T) {
	img := requireE2E(t)
	node := requireNode(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "argv0", argvGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "argv0")

	const sentinel = "sentinel-argv-from-the-boot-record"
	root := t.TempDir()
	image, _, err := rootfs.Build(root, rootfs.Options{Boot: &rootfs.Boot{
		Argv: []string{sentinel},
		Cwd:  "/",
	}})
	if err != nil {
		t.Fatalf("building the rfs sidecar: %v", err)
	}
	sidecar := filepath.Join(dir, "rootfs.img")
	if err := os.WriteFile(sidecar, image, 0o644); err != nil {
		t.Fatal(err)
	}

	withImg := runNodeHost(t, ctx, node, wasm, sidecar)
	if !strings.Contains(withImg, "ARGV0="+sentinel) {
		t.Errorf("argv[0] did not come from the boot record, so the sidecar was not "+
			"parsed even if it was opened; full output:\n%s", withImg)
	}

	// ecvisor boots without a sidecar -- `load_sidecar` returns None and
	// `Vfs::new(None)` is valid -- so this must not crash. It must, however,
	// stop reporting the sentinel, because there is nowhere else it could come
	// from.
	without := runNodeHost(t, ctx, node, wasm, "")
	if strings.Contains(without, sentinel) {
		t.Errorf("argv[0] still reported the boot record's value with NO sidecar "+
			"supplied, so it cannot have come from the image; full output:\n%s", without)
	}
}
