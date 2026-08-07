package e2e

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"raptormark/internal/relay"
	"raptormark/internal/serve"
)

// bothGuestSrc is a server that is also a client: for every request it accepts,
// it resolves and dials an upstream and folds the answer into its reply.
//
// ⚠️ THAT COMBINATION IS THE ENTIRE POINT. Serving and dialling are different
// transports in a browser -- a service worker mints the inbound connection, a
// WebSocket relay carries the outbound one -- and `socket(AF_INET, SOCK_STREAM)`
// looks identical for both. A guest doing only one of them cannot detect a
// composite that routes every socket to the same place.
//
// It also resolves a NAME per request, so the DNS tap and the address pool are
// on the path too: the reply proves resolve, connect, exchange, accept and
// respond all worked in one process.
const bothGuestSrc = `#define _GNU_SOURCE
#include <arpa/inet.h>
#include <errno.h>
#include <netdb.h>
#include <netinet/in.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

/* Dials the upstream, exchanges one message, and returns its reply in out. */
static int ask_upstream(const char *host, const char *port, char *out, size_t cap) {
	struct addrinfo hints, *res = NULL;
	memset(&hints, 0, sizeof hints);
	hints.ai_family = AF_INET;
	hints.ai_socktype = SOCK_STREAM;
	if (getaddrinfo(host, port, &hints, &res) != 0 || res == NULL) {
		snprintf(out, cap, "resolve-failed");
		return -1;
	}
	int s = socket(AF_INET, SOCK_STREAM, 0);
	if (s < 0) { freeaddrinfo(res); snprintf(out, cap, "socket-failed"); return -1; }
	if (connect(s, res->ai_addr, res->ai_addrlen) != 0) {
		snprintf(out, cap, "connect-failed-%d", errno);
		close(s); freeaddrinfo(res); return -1;
	}
	freeaddrinfo(res);
	if (send(s, "PING", 4, 0) != 4) { snprintf(out, cap, "send-failed"); close(s); return -1; }
	char in[64];
	ssize_t n = recv(s, in, sizeof in - 1, 0);
	if (n <= 0) { snprintf(out, cap, "recv-failed-%d", errno); close(s); return -1; }
	in[n] = 0;
	snprintf(out, cap, "%s", in);
	close(s);
	return 0;
}

int main(int argc, char **argv) {
	int port = argc > 1 ? atoi(argv[1]) : 8080;
	const char *up_host = argc > 2 ? argv[2] : "localhost";
	const char *up_port = argc > 3 ? argv[3] : "80";

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

	printf("BOTH-READY port=%d upstream=%s:%s\n", port, up_host, up_port);
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

		char path[1024];
		path[0] = 0;
		sscanf(req, "%*s %1023s", path);

		char up[128];
		ask_upstream(up_host, up_port, up, sizeof up);

		char body[1400];
		int bl = snprintf(body, sizeof body,
			"RAPTORMARK-BOTH req=%d path=%s upstream=%s\n", n, path, up);

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

		printf("BOTH-SERVED %d %s upstream=%s\n", n, path, up);
		fflush(stdout);
	}
	return 0;
}
`

// TestCompositeBackendServesAndDialsFromOneGuest is the composite's reason to
// exist: one process, both directions, two different browser transports.
//
// ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? A composite that
// routed every socket to the inbound backend would still serve the page, and one
// that routed everything to the relay would still reach the upstream. Only a
// response that carries BOTH -- the guest's own per-connection counter AND the
// upstream's reply -- requires the two to have been routed differently within a
// single process.
func TestCompositeBackendServesAndDialsFromOneGuest(t *testing.T) {
	requireBrowserFixtures(t, "both.wasm")
	node := requireNode(t)

	web, err := filepath.Abs(filepath.Join("..", "web"))
	if err != nil {
		t.Fatal(err)
	}

	// The upstream the GUEST dials, reachable only through the relay: it listens
	// on loopback, which a browser cannot reach by any other route.
	upstream := pingPongServer(t)

	port := freePort(t)
	base := "http://127.0.0.1:" + port
	srv, _, err := serve.ListenWithRelay("127.0.0.1:"+port, web, &relay.Config{
		Allow:   []string{"localhost:" + upstream},
		Origins: []string{base},
		Log:     func(f string, a ...any) { t.Logf(f, a...) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	t.Logf("serving %s, relay allows localhost:%s", base, upstream)

	cmd := exec.CommandContext(ctxFor(t), "npx", "playwright", "test", "tests/composite.spec.ts")
	cmd.Dir = web
	cmd.Env = append(os.Environ(),
		"RAPTORMARK_BASE_URL="+base,
		"RAPTORMARK_RELAY_URL=ws://127.0.0.1:"+port+"/relay",
		"RAPTORMARK_UPSTREAM_PORT="+upstream,
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

// pingPongServer answers "PING" with "PONG" and returns its port.
func pingPongServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 16)
				n, err := c.Read(buf)
				if err != nil || n == 0 {
					return
				}
				_, _ = c.Write([]byte("PONG"))
			}(c)
		}
	}()
	_, p, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return p
}
