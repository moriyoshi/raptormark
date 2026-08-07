package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"raptormark/internal/serve"
)

// httpdGuestSrc is a real HTTP/1.1 server: bind, listen, accept, read, respond.
//
// ⚠️ IT IS AN ORDINARY BLOCKING SERVER, deliberately. It uses no non-blocking
// flags and no event loop, so `accept` and `recv` PARK the process and the
// runtime has to hand control back to the host and resume it later. A guest
// that polled instead would exercise the ABI without ever exercising the thing
// that makes inbound hard in a browser.
//
// ⚠️ IT NEVER EXITS. Every other browser fixture runs to completion, so the
// page could await the result; this one is a server and the host has to drive
// it while serving requests against it.
//
// The response body carries a per-connection COUNTER and the REQUEST PATH,
// because those are the two things a static stub could not produce: the counter
// proves each request reached the guest rather than a cache, and the echoed path
// proves the bytes the guest received were the ones the browser sent.
const httpdGuestSrc = `#define _GNU_SOURCE
#include <arpa/inet.h>
#include <errno.h>
#include <netinet/in.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

int main(int argc, char **argv) {
	int port = argc > 1 ? atoi(argv[1]) : 8080;

	int ls = socket(AF_INET, SOCK_STREAM, 0);
	if (ls < 0) { printf("FAIL socket (errno=%d)\n", errno); return 1; }
	int one = 1;
	setsockopt(ls, SOL_SOCKET, SO_REUSEADDR, &one, sizeof one);

	struct sockaddr_in sa;
	memset(&sa, 0, sizeof sa);
	sa.sin_family = AF_INET;
	sa.sin_addr.s_addr = htonl(INADDR_ANY);
	sa.sin_port = htons((unsigned short)port);
	if (bind(ls, (struct sockaddr *)&sa, sizeof sa) != 0) {
		printf("FAIL bind (errno=%d)\n", errno);
		return 1;
	}
	if (listen(ls, 16) != 0) { printf("FAIL listen (errno=%d)\n", errno); return 1; }

	printf("HTTPD-READY port=%d\n", port);
	fflush(stdout);

	int n = 0;
	for (;;) {
		int c = accept(ls, NULL, NULL);
		if (c < 0) {
			if (errno == EINTR) continue;
			printf("FAIL accept (errno=%d)\n", errno);
			return 1;
		}
		n++;

		char req[4096];
		ssize_t r = recv(c, req, sizeof req - 1, 0);
		if (r <= 0) { close(c); continue; }
		req[r] = 0;

		/* "GET <path> HTTP/1.1" -- the path is all this demo needs. */
		char path[1024];
		path[0] = 0;
		sscanf(req, "%*s %1023s", path);

		char body[1200];
		int bl = snprintf(body, sizeof body,
			"RAPTORMARK-GUEST req=%d path=%s\n", n, path);

		char out[2048];
		int ol = snprintf(out, sizeof out,
			"HTTP/1.1 200 OK\r\n"
			"Content-Type: text/plain; charset=utf-8\r\n"
			"Content-Length: %d\r\n"
			"Connection: close\r\n"
			"\r\n"
			"%s", bl, body);

		send(c, out, ol, 0);
		shutdown(c, SHUT_WR);
		close(c);

		printf("HTTPD-SERVED %d %s\n", n, path);
		fflush(stdout);
	}
	return 0;
}
`

// TestServiceWorkerServesAGuestInABrowser renders a page whose bytes were
// produced by an aarch64 server running in the same tab.
//
// ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? A service worker that
// synthesized its own response would satisfy any assertion about the iframe
// merely rendering, and so would a cached one. So the test fetches TWO distinct
// paths and requires that each response echo ITS OWN path and carry a DIFFERENT,
// INCREASING connection counter. Only something holding per-connection state
// behind a socket can produce that, and the only such thing here is the guest.
//
// The whole chain runs with no network at all: the request never leaves the
// browser process, and there is no relay involved.
func TestServiceWorkerServesAGuestInABrowser(t *testing.T) {
	requireBrowserFixtures(t, "httpd.wasm")
	node := requireNode(t)

	web, err := filepath.Abs(filepath.Join("..", "web"))
	if err != nil {
		t.Fatal(err)
	}

	// ⚠️ A SERVICE WORKER NEEDS A SECURE CONTEXT. `127.0.0.1` and `localhost`
	// qualify without TLS; any other host would need https, and registration
	// fails with an error that names neither the origin nor the reason.
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	srv, _, err := serve.Listen("127.0.0.1:"+port, web)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	t.Logf("serving %s", base)

	cmd := exec.CommandContext(ctxFor(t), "npx", "playwright", "test",
		"tests/inbound.spec.ts", "tests/swrestart.spec.ts")
	cmd.Dir = web
	cmd.Env = append(os.Environ(),
		"RAPTORMARK_BASE_URL="+base,
		"PATH="+filepath.Dir(node)+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("playwright: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		t.Log(line)
	}
}

// requireBrowserFixtures skips unless the named prebuilt artifacts are present.
//
// They are pipeline output and cost a real translation run, so they are built
// on demand by `TestBuildBrowserFixture` rather than by this test -- which is
// what keeps a browser test from silently inheriting the full pipeline cost.
func requireBrowserFixtures(t *testing.T, names ...string) {
	t.Helper()
	if os.Getenv("RAPTORMARK_E2E_BROWSER") != "1" {
		t.Skip("set RAPTORMARK_E2E_BROWSER=1")
	}
	for _, n := range names {
		p := filepath.Join("..", "web", "public", n)
		st, err := os.Stat(p)
		if err != nil {
			t.Skipf("missing fixture %s; build it with "+
				"RAPTORMARK_BUILD_BROWSER_FIXTURE=1 go test ./e2e/ -run TestBuildBrowserFixture", p)
		}
		t.Logf("fixture %s (%s bytes)", n, strconv.FormatInt(st.Size(), 10))
	}
}
