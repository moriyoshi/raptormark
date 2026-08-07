package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// nonblockGuestSrc pins the semantics of O_NONBLOCK on a socket.
//
// ecvisor keeps every host socket non-blocking internally, because turning a
// would-block into a suspension is the only way to give a blocking guest
// blocking semantics. For a long time it applied that to ALL guests: sys_accept
// documented the guest's own SOCK_NONBLOCK as "ignored", and socket_recv and
// socket_send suspended on WASI_EAGAIN without consulting the descriptor's flag.
// The pipe path (read_pipe) had always honoured it; sockets were the outlier.
//
// For an event-driven server that inverts the contract. nginx accepts with
// SOCK_NONBLOCK (traced: accept4(..., 0x800)) precisely so that the recv after a
// response returns EAGAIN and it can go back to epoll_wait with a keepalive
// timer armed. Suspending there parks the entire process inside the read, out of
// its event loop -- it cannot accept, cannot run a timer, and cannot serve any
// other connection -- until that one client sends bytes or hangs up.
//
// This guest is single-process on purpose, so a regression cannot hide behind
// another runnable process: pre-fix it DEADLOCKS at the first check, because the
// blocked-on-socket process is the only one alive and the fd it waits on can
// never become ready. Each check prints before it runs, so the last line of a
// hung run names the operation that stalled.
const nonblockGuestSrc = `#define _GNU_SOURCE
#include <arpa/inet.h>
#include <errno.h>
#include <fcntl.h>
#include <netinet/in.h>
#include <stdio.h>
#include <string.h>
#include <sys/socket.h>
#include <time.h>
#include <unistd.h>

static int failures = 0;
#define CHECK(cond, what) do { if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } } while (0)

int main(void) {
	int ln = socket(AF_INET, SOCK_STREAM, 0);
	CHECK(ln >= 0, "socket listener");

	struct sockaddr_in a;
	memset(&a, 0, sizeof a);
	a.sin_family = AF_INET;
	a.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
	a.sin_port = htons(39117);
	CHECK(bind(ln, (struct sockaddr *)&a, sizeof a) == 0, "bind");
	CHECK(listen(ln, 16) == 0, "listen");

	socklen_t alen = sizeof a;
	CHECK(getsockname(ln, (struct sockaddr *)&a, &alen) == 0, "getsockname");
	printf("bound port=%d\n", (int)ntohs(a.sin_port));
	fflush(stdout);

	/* Mark the LISTENER non-blocking through fcntl, the other route to the
	   same flag (nginx uses ioctl(FIONBIO) on its listeners). */
	int fl = fcntl(ln, F_GETFL, 0);
	CHECK(fl >= 0, "fcntl F_GETFL");
	CHECK(fcntl(ln, F_SETFL, fl | O_NONBLOCK) == 0, "fcntl F_SETFL O_NONBLOCK");

	/* 1. accept on a non-blocking listener with nothing pending must report
	      EAGAIN. Pre-fix this suspends and, as the only live process, hangs. */
	printf("check accept-eagain\n");
	fflush(stdout);
	int c = accept(ln, NULL, NULL);
	CHECK(c < 0 && (errno == EAGAIN || errno == EWOULDBLOCK), "accept should be EAGAIN");

	/* 2. A real connection, then accept4 with SOCK_NONBLOCK. Connecting to our
	      own listening socket completes in the backlog, so a blocking connect
	      returns without needing a second process. */
	printf("check connect\n");
	fflush(stdout);
	int cl = socket(AF_INET, SOCK_STREAM, 0);
	CHECK(cl >= 0, "socket client");
	CHECK(connect(cl, (struct sockaddr *)&a, sizeof a) == 0, "connect");

	printf("check accept4-nonblock\n");
	fflush(stdout);
	int sv = accept4(ln, NULL, NULL, SOCK_NONBLOCK);
	CHECK(sv >= 0, "accept4");

	/* The flag accept4 was asked for must be visible on the new descriptor. */
	int svfl = fcntl(sv, F_GETFL, 0);
	CHECK(svfl >= 0 && (svfl & O_NONBLOCK), "accept4 SOCK_NONBLOCK not recorded");

	/* 3. recv on that non-blocking, empty connection must report EAGAIN rather
	      than suspending. This is the check nginx's event loop depends on: the
	      recv after a response is what parked a worker outside its event loop. */
	printf("check recv-eagain\n");
	fflush(stdout);
	char buf[64];
	ssize_t n = recv(sv, buf, sizeof buf, 0);
	CHECK(n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK), "recv should be EAGAIN");

	/* 4. Non-blocking must not have broken the ordinary path: real data still
	      arrives, and recv reports it. */
	printf("check recv-data\n");
	fflush(stdout);
	CHECK(send(cl, "hello", 5, 0) == 5, "send");
	n = -1;
	for (int i = 0; i < 200 && n < 0; i++) {
		n = recv(sv, buf, sizeof buf, 0);
		if (n < 0) {
			struct timespec ts = { 0, 5 * 1000 * 1000 };
			nanosleep(&ts, NULL);
		}
	}
	CHECK(n == 5 && memcmp(buf, "hello", 5) == 0, "recv data");

	/* 5. A connect to a port nobody listens on must FAIL. The in-progress
	      errnos are reported asynchronously, so this is the check that stops
	      "treat a resumed connect as connected" from swallowing a real refusal. */
	printf("check connect-refused\n");
	fflush(stdout);
	int dead = socket(AF_INET, SOCK_STREAM, 0);
	CHECK(dead >= 0, "socket dead");
	struct sockaddr_in d = a;
	d.sin_port = htons(39118); /* nothing bound here */
	CHECK(connect(dead, (struct sockaddr *)&d, sizeof d) != 0, "connect to dead port should fail");
	close(dead);

	close(sv);
	close(cl);
	close(ln);

	if (failures == 0) {
		printf("NBSOCK-OK\n");
	}
	return failures == 0 ? 0 : 1;
}
`

// TestNonblockingSocketUnderEcvisor guards the O_NONBLOCK contract on sockets.
// See nonblockGuestSrc for what each check pins down and why the guest is
// deliberately single-process.
func TestNonblockingSocketUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "nbsock", nonblockGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "nbsock")

	out := runWasm(t, ctx, wasm)
	assertNonblockGuestPassed(t, out)
}

// TestNonblockingSocketNativeBaseline runs the same guest on Linux, so the
// expectations above are pinned to real kernel behaviour rather than to what
// ecvisor happens to do.
func TestNonblockingSocketNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "nbsock", nonblockGuestSrc)

	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx, []string{"-v", abs + ":/w"}, "/w/nbsock")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertNonblockGuestPassed(t, out)
}

func assertNonblockGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "NBSOCK-OK") {
		t.Errorf("guest did not reach NBSOCK-OK; full output:\n%s", out)
	}
}
