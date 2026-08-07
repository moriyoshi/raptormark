package e2e

import (
	"io"
	"net"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"raptormark/internal/oci"
)

// The browser profile: `raptormark_net_v1` instead of WasmEdge's socket
// extension, and a backend whose `wait` NEVER blocks.
//
// ⚠️ IT IS TESTED UNDER NODE, NOT A BROWSER, AND THAT IS DELIBERATE. What a
// browser adds is a TRANSPORT (fetch, WebSocket, WebTransport) and an event
// loop; what it does not change is the ABI or the scheduler contract. Driving
// the same module from Node -- which already owns real sockets -- settles the
// hard half without standing up headless browsers, and leaves the browser work
// as a transport swap rather than a new ABI to debug at the same time.
//
// A browser test still has to exist. This is not a substitute for it, it is what
// makes it a small job.

var netV1Imports = []string{
	"net_init", "net_socket", "net_bind", "net_listen", "net_connect",
	"net_accept", "net_recv", "net_send", "net_recv_from", "net_send_to",
	"net_addr", "net_sockopt", "net_shutdown", "net_close",
}

// TestBrowserProfileImportsItsOwnNamespace pins the import surface.
//
// Standard preview1 has no way to CREATE an outbound socket -- no `sock_open`,
// no `sock_connect` -- so a separate namespace is not a preference, it is the
// only way to express a browser's network at all. Versioning it means a host
// missing a name fails at INSTANTIATION, loudly, rather than at the first
// connect.
func TestBrowserProfileImportsItsOwnNamespace(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "brprobe", importProbeGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "brprobe", "--profile", "browser")

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
	t.Logf("browser profile: %d imports", len(got))

	var netv1, wasmEdge []string
	for _, imp := range got {
		mod, field, ok := strings.Cut(imp, ".")
		if !ok {
			continue
		}
		switch mod {
		case "raptormark_net_v1":
			netv1 = append(netv1, field)
		case "wasi_snapshot_preview1":
			if slices.Contains(wasmEdgeOnly, field) {
				wasmEdge = append(wasmEdge, field)
			}
		default:
			t.Errorf("%s: a third namespace nobody recorded", imp)
		}
	}
	slices.Sort(netv1)
	t.Logf("raptormark_net_v1: %s", strings.Join(netv1, " "))

	if len(wasmEdge) > 0 {
		t.Errorf("the browser profile still imports WasmEdge socket extensions: %s. "+
			"Both backends are live, which is what a `dyn` or an enum would cause.",
			strings.Join(wasmEdge, " "))
	}
	if len(netv1) == 0 {
		t.Fatal("the browser profile imports NOTHING from raptormark_net_v1, so the " +
			"backend was not linked in and this test proves nothing")
	}
	for _, want := range netV1Imports {
		if !slices.Contains(netv1, want) {
			t.Errorf("missing import %q from raptormark_net_v1", want)
		}
	}
}

// TestBrowserProfileServesTheSocketABIUnderNode is the real check: the same
// kernel-pinned guest as the WasmEdge and loopback profiles, over the new ABI,
// driven by the re-entrant loop.
//
// ⚠️ Both halves are load-bearing. The browser backend's `wait` never blocks, so
// this ONLY works if the host drives slices and supplies readiness -- a
// `_start` run would go idle on the first socket wait and never come back. And
// the guest is `nonblockGuestSrc` rather than a friendlier one because its
// expectations are already pinned to a real Linux kernel by
// `TestNonblockingSocketNativeBaseline`, so passing means faithful rather than
// merely self-consistent.
func TestBrowserProfileServesTheSocketABIUnderNode(t *testing.T) {
	img := requireE2E(t)
	node := requireNode(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "brsock", nonblockGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "brsock", "--profile", "browser")

	out := runNodeHostArgs(t, ctx, node, wasm, "", "--reentrant", "--net-v1")
	assertNonblockGuestPassed(t, out)
	if !strings.Contains(out, "HOST-EXIT: 0") {
		t.Errorf("non-zero exit under the browser profile; full output:\n%s", out)
	}
	// It must have gone idle: a never-blocking backend cannot do otherwise, and
	// if it did not, the host was not the one doing the waiting.
	if m := reSliceIdle.FindStringSubmatch(out); m == nil || m[1] == "0" {
		t.Errorf("the guest never went idle, so the browser backend blocked "+
			"somewhere it must not. Full output:\n%s", out)
	}
}

// dnsGuestSrc resolves a name and connects to what the resolver returned.
//
// ⚠️ IT USES `getaddrinfo`, NOT A HAND-BUILT QUERY. The point of intercepting
// DNS at the WIRE is that the guest's own resolver runs unmodified -- musl's and
// glibc's paths differ substantially, and `/etc/nsswitch.conf`, search domains
// and the hosts file all still apply. A test that spoke DNS itself would prove
// the parser works and nothing about whether a real libc is satisfied.
//
// It resolves "localhost", so the whole path -- query, synthetic address,
// connect, reverse lookup, real connection -- runs with no external network.
const dnsGuestSrc = `#define _GNU_SOURCE
#include <arpa/inet.h>
#include <errno.h>
#include <netdb.h>
#include <netinet/in.h>
#include <stdio.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

static int failures = 0;
#define CHECK(cond, what) do { if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } } while (0)

int main(int argc, char **argv) {
	const char *port = argc > 1 ? argv[1] : "80";

	struct addrinfo hints, *res = NULL;
	memset(&hints, 0, sizeof hints);
	hints.ai_family = AF_INET;
	hints.ai_socktype = SOCK_STREAM;

	printf("check resolve\n");
	fflush(stdout);
	int rc = getaddrinfo("localhost", port, &hints, &res);
	CHECK(rc == 0 && res != NULL, "getaddrinfo");
	if (rc != 0 || res == NULL) { printf("DNS-DONE\n"); return 1; }

	struct sockaddr_in *sin = (struct sockaddr_in *)res->ai_addr;
	char buf[64];
	inet_ntop(AF_INET, &sin->sin_addr, buf, sizeof buf);
	printf("resolved=%s\n", buf);
	fflush(stdout);

	printf("check connect\n");
	fflush(stdout);
	int s = socket(AF_INET, SOCK_STREAM, 0);
	CHECK(s >= 0, "socket");
	CHECK(connect(s, res->ai_addr, res->ai_addrlen) == 0, "connect to resolved address");

	printf("check exchange\n");
	fflush(stdout);
	CHECK(send(s, "PING", 4, 0) == 4, "send");
	char in[16];
	ssize_t n = recv(s, in, sizeof in, 0);
	CHECK(n == 4 && memcmp(in, "PONG", 4) == 0, "recv PONG");

	close(s);
	freeaddrinfo(res);
	if (failures == 0) printf("DNS-OK\n");
	printf("DNS-DONE\n");
	return failures == 0 ? 0 : 1;
}
`

// TestBrowserProfileResolvesAndConnectsThroughTheAddressPool is the end of the
// chain that makes any browser transport possible.
//
// ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? If the resolver were
// NOT intercepted, `getaddrinfo("localhost")` would come back from the guest's
// own hosts file as 127.0.0.1 and the connect would work anyway -- proving
// nothing. So the test asserts the address is from the SYNTHETIC range, which
// only the tap can produce, AND that connecting to it reaches a real server,
// which only the reverse lookup can arrange.
func TestBrowserProfileResolvesAndConnectsThroughTheAddressPool(t *testing.T) {
	img := requireE2E(t)
	node := requireNode(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	// A local echo server: the guest must reach THIS through a synthetic
	// address, which is only possible if the name survived the round trip.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				b := make([]byte, 4)
				if _, err := io.ReadFull(c, b); err != nil {
					return
				}
				if string(b) == "PING" {
					_, _ = c.Write([]byte("PONG"))
				}
			}(c)
		}
	}()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	elf := compileGuest(t, ctx, dir, "brdns", dnsGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "brdns", "--profile", "browser")

	out := runNodeHostArgs(t, ctx, node, wasm, "", "--reentrant", "--net-v1", "--", "guest", port)

	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "DNS-OK") {
		t.Errorf("the guest did not complete the resolve-and-connect path; output:\n%s", out)
	}

	// The address MUST be synthetic. Without this the test passes on a host that
	// never intercepted anything.
	m := reResolved.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the guest never reported a resolved address; output:\n%s", out)
	}
	first, _ := strconv.Atoi(strings.Split(m[1], ".")[0])
	if first < 240 {
		t.Errorf("resolved %s, which is NOT from the synthetic range -- the guest's "+
			"resolver was not intercepted, so the connect proved only that "+
			"localhost works. Output:\n%s", m[1], out)
	}
	t.Logf("resolved through the pool: %s", m[1])
}

var reResolved = regexp.MustCompile(`resolved=([0-9.]+)`)
