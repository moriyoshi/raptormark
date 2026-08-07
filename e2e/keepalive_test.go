package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"raptormark/internal/serve"
)

// kaGuestSrc answers with a Content-Length and then NEVER CLOSES the connection.
//
// ⚠️ THAT IS THE ENTIRE FIXTURE, and it is deliberately not what a good server
// does. A real keep-alive server holds the connection open waiting for a
// follow-up request and eventually times it out; this one simply holds it, which
// is the same thing from the host's side and is what makes the question
// answerable at all.
//
// The host used to decide a response was finished when the guest closed. Against
// this guest that never happens, so every request would sit until the request
// deadline and be reported as a timeout against a server that answered correctly
// and promptly. A guest that closes -- like `httpd.wasm` -- cannot tell the two
// mechanisms apart, because closing satisfies both.
//
// ⚠️ It leaks a descriptor per request, which a real server would not. Three
// requests is well inside what that can stand, and closing is exactly the
// behaviour being excluded.
const kaGuestSrc = `#define _GNU_SOURCE
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

	printf("KA-READY port=%d\n", port);
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
		if (r <= 0) { continue; }
		req[r] = 0;

		char path[1024];
		path[0] = 0;
		sscanf(req, "%*s %1023s", path);

		char body[1200];
		int bl = snprintf(body, sizeof body,
			"RAPTORMARK-KEEPALIVE req=%d path=%s\n", n, path);

		char out[2048];
		int ol = snprintf(out, sizeof out,
			"HTTP/1.1 200 OK\r\n"
			"Content-Type: text/plain; charset=utf-8\r\n"
			"Content-Length: %d\r\n"
			"\r\n"
			"%s", bl, body);

		send(c, out, ol, 0);

		/* NO close(c) and NO shutdown(c). The connection stays open, exactly as
		   it would on a server waiting for the next request on it. */

		printf("KA-SERVED %d %s\n", n, path);
		fflush(stdout);
	}
	return 0;
}
`

// TestKeepAliveServerIsFramedRatherThanWaitedOut proves the host ends a response
// by FRAMING it, not by waiting for the guest to hang up.
//
// ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? Nothing subtle: the
// requests would time out. That is the point of a guest which never closes --
// against `httpd.wasm`, which does close, both a framing host and a
// wait-for-close host return exactly the same bytes at nearly the same moment,
// so no assertion on the response could separate them.
func TestKeepAliveServerIsFramedRatherThanWaitedOut(t *testing.T) {
	requireBrowserFixtures(t, "ka.wasm")
	node := requireNode(t)

	web, err := filepath.Abs(filepath.Join("..", "web"))
	if err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	base := "http://127.0.0.1:" + port
	srv, _, err := serve.Listen("127.0.0.1:"+port, web)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	t.Logf("serving %s", base)

	cmd := exec.CommandContext(ctxFor(t), "npx", "playwright", "test", "tests/keepalive.spec.ts")
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
