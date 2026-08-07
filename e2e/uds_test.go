package e2e

import (
	"strings"
	"testing"
)

// udsGuestSrc exercises a NAMED AF_UNIX socket end to end: bind, the filesystem
// node bind creates, listen, connect, accept, both data directions, EOF, and
// unlink.
//
// WASI has no AF_UNIX. WasmEdge's socket extension knows exactly two families,
// INET4 and INET6, so there is no host object to bridge to -- and there should
// not be, because both endpoints are guest processes inside one module and the
// path they meet on exists only in the guest's rfs/tmpfs overlay. The whole
// thing is implemented in-runtime on top of the pipe table, which is why every
// piece of it needs a guard.
//
// What each check would let through if it were the only one:
//
//   - the S_ISSOCK check is what rules out the tempting shortcut of creating an
//     ordinary empty file at the bound path. Such a stand-in passes bind,
//     listen, connect, accept, read and write -- everything below it -- and
//     fails only where PostgreSQL actually looks: it stats its socket path
//     before unlinking a stale one, and refuses to remove something that is not
//     a socket.
//   - ENOENT vs ECONNREFUSED are separated deliberately. libpq prints them as
//     two different diagnostics and clients retry on one and not the other, so
//     collapsing both into "connect failed" is a silent wrong answer.
//   - round 1 is cross-PROCESS and reaches accept BEFORE the peer connects, so
//     it is the only check that exercises the blocking path
//     (BlockedOn::UnixAccept). A missing wakeup there does not fail this test,
//     it hangs it -- the scheduler reports "deadlock: every process is blocked"
//     and the run times out. That is the intended signal; it cannot be an
//     assertion, because the state being asserted about is "still running".
//   - round 2 connects to the process's own listener, which is legal on Linux
//     and is the only deterministic way to reach the other accept path -- a
//     connection already queued, no blocking at all.
const udsGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <stdio.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#define SOCKPATH "/tmp/uds-e2e.sock"

static int failures = 0;
#define CHECK(cond, what) do { \
	if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } \
} while (0)

static void fill(struct sockaddr_un *a) {
	memset(a, 0, sizeof *a);
	a->sun_family = AF_UNIX;
	strncpy(a->sun_path, SOCKPATH, sizeof a->sun_path - 1);
}

int main(void) {
	struct sockaddr_un a;
	struct stat st;
	char buf[5];
	fill(&a);
	unlink(SOCKPATH);

	/* Nothing bound yet: the path does not exist, so this is ENOENT. */
	int probe = socket(AF_UNIX, SOCK_STREAM, 0);
	CHECK(probe >= 0, "socket");
	errno = 0;
	CHECK(connect(probe, (struct sockaddr *)&a, sizeof a) == -1 && errno == ENOENT,
	      "connect before bind is ENOENT");
	close(probe);

	int srv = socket(AF_UNIX, SOCK_STREAM, 0);
	CHECK(srv >= 0, "server socket");
	CHECK(bind(srv, (struct sockaddr *)&a, sizeof a) == 0, "bind");

	/* bind published a filesystem node, and it is a SOCKET. */
	CHECK(stat(SOCKPATH, &st) == 0, "stat the bound path");
	CHECK(S_ISSOCK(st.st_mode), "the bound path is S_IFSOCK");
	errno = 0;
	CHECK(open(SOCKPATH, O_RDONLY) == -1, "the bound path is not openable");

	/* And it is exclusive. */
	int second = socket(AF_UNIX, SOCK_STREAM, 0);
	errno = 0;
	CHECK(bind(second, (struct sockaddr *)&a, sizeof a) == -1 && errno == EADDRINUSE,
	      "a second bind is EADDRINUSE");
	close(second);

	/* Bound but not listening: refused, and distinct from ENOENT above. */
	probe = socket(AF_UNIX, SOCK_STREAM, 0);
	errno = 0;
	CHECK(connect(probe, (struct sockaddr *)&a, sizeof a) == -1 && errno == ECONNREFUSED,
	      "connect before listen is ECONNREFUSED");
	close(probe);

	CHECK(listen(srv, 5) == 0, "listen");

	struct sockaddr_un got;
	socklen_t glen = sizeof got;
	memset(&got, 0, sizeof got);
	CHECK(getsockname(srv, (struct sockaddr *)&got, &glen) == 0, "getsockname");
	CHECK(got.sun_family == AF_UNIX && strcmp(got.sun_path, SOCKPATH) == 0,
	      "getsockname reports the bound path");

	/* An empty backlog on a NON-blocking listener must not park the process. */
	int fl = fcntl(srv, F_GETFL, 0);
	CHECK(fcntl(srv, F_SETFL, fl | O_NONBLOCK) == 0, "make the listener non-blocking");
	errno = 0;
	CHECK(accept(srv, NULL, NULL) == -1 && (errno == EAGAIN || errno == EWOULDBLOCK),
	      "accept on an empty backlog is EAGAIN");
	CHECK(fcntl(srv, F_SETFL, fl) == 0, "restore the listener to blocking");

	/* Round 1: cross-process, parent blocks in accept until the child dials. */
	pid_t kid = fork();
	CHECK(kid >= 0, "fork");
	if (kid == 0) {
		struct sockaddr_un ca;
		char cb[5];
		fill(&ca);
		int c = socket(AF_UNIX, SOCK_STREAM, 0);
		if (c < 0) { _exit(11); }
		if (connect(c, (struct sockaddr *)&ca, sizeof ca) != 0) { _exit(12); }
		if (write(c, "ping", 4) != 4) { _exit(13); }
		memset(cb, 0, sizeof cb);
		if (read(c, cb, 4) != 4 || memcmp(cb, "pong", 4) != 0) { _exit(14); }
		/* The server closed after answering; the buffered reply arrives first
		   and EOF only after it. */
		if (read(c, cb, 4) != 0) { _exit(15); }
		close(c);
		_exit(0);
	}
	int cfd = accept(srv, NULL, NULL);
	CHECK(cfd >= 0, "accept");
	memset(buf, 0, sizeof buf);
	CHECK(read(cfd, buf, 4) == 4 && memcmp(buf, "ping", 4) == 0, "server reads the request");
	CHECK(write(cfd, "pong", 4) == 4, "server writes the reply");
	close(cfd);
	int status = 0;
	CHECK(waitpid(kid, &status, 0) == kid, "waitpid");
	CHECK(WIFEXITED(status) && WEXITSTATUS(status) == 0, "the client exited 0");
	if (WIFEXITED(status) && WEXITSTATUS(status) != 0) {
		printf("client exit code %d\n", WEXITSTATUS(status));
	}

	/* Round 2: a connection already queued when accept runs. */
	int self = socket(AF_UNIX, SOCK_STREAM, 0);
	CHECK(self >= 0, "self socket");
	CHECK(connect(self, (struct sockaddr *)&a, sizeof a) == 0, "connect to our own listener");
	int acc = accept(srv, NULL, NULL);
	CHECK(acc >= 0, "accept a queued connection");
	CHECK(write(self, "abcd", 4) == 4, "client writes");
	memset(buf, 0, sizeof buf);
	CHECK(read(acc, buf, 4) == 4 && memcmp(buf, "abcd", 4) == 0, "server reads it");

	/* send/recv, NOT just write/read. They are different syscalls -- sendto and
	   recvfrom -- and routing them by "does this fd have a host socket" sent
	   every in-guest endpoint to ENOTSOCK. nginx never caught it because the
	   only thing it does with a socketpair is sendmsg/recvmsg; PostgreSQL's
	   backend calls recv() on the connection it just accepted, and reported
	   "could not receive data from client: Socket operation on non-socket". */
	CHECK(send(self, "wxyz", 4, 0) == 4, "client send()");
	memset(buf, 0, sizeof buf);
	CHECK(recv(acc, buf, 4, 0) == 4 && memcmp(buf, "wxyz", 4) == 0, "server recv()");

	/* poll() on the connection. aarch64 has no poll syscall, so this is ppoll,
	   and libpq calls it on every connect and every query -- an ENOSYS here
	   means a client that reached the server still cannot talk to it. */
	struct pollfd pfd;
	pfd.fd = acc;
	pfd.events = POLLIN;
	pfd.revents = 0;
	CHECK(poll(&pfd, 1, 0) == 0 && pfd.revents == 0, "poll reports nothing pending");
	CHECK(send(self, "q", 1, 0) == 1, "client sends one byte");
	pfd.revents = 0;
	CHECK(poll(&pfd, 1, -1) == 1 && (pfd.revents & POLLIN), "poll reports readable");
	CHECK(recv(acc, buf, 1, 0) == 1, "drain the byte");

	/* A poll with a TIMEOUT and nothing to report. Separate from the two above
	   because it is the only one that reaches ppoll's deadline path: zero
	   returns immediately and -1 parks forever, while a finite timeout has to
	   arm an absolute deadline, park, be released by the clock, and come back
	   with 0. Getting that wrong is not a crash -- it is a guest that waits a
	   multiple of what it asked for, or one that spins. */
	struct timespec t0, t1;
	pfd.revents = 0;
	CHECK(clock_gettime(CLOCK_MONOTONIC, &t0) == 0, "clock before the timed poll");
	int pr = poll(&pfd, 1, 300);
	CHECK(clock_gettime(CLOCK_MONOTONIC, &t1) == 0, "clock after the timed poll");
	CHECK(pr == 0 && pfd.revents == 0, "a timed poll with nothing ready returns 0");
	long elapsed_ms = (t1.tv_sec - t0.tv_sec) * 1000 +
	                  (t1.tv_nsec - t0.tv_nsec) / 1000000;
	/* The LOWER bound is the real check: neutralized by making ppoll return 0
	   for any finite timeout, it fails with "timed poll took 0 ms".

	   The upper bound is NOT coverage of the deadline re-arm bug, and saying so
	   matters more than having the assertion. Re-arming on every resume -- the
	   bug that made an nginx keepalive close at 4.59s or 19.78s for a 5s
	   timeout -- was tried here and this test still PASSED, because reproducing
	   it needs a spurious wake and a single process with nothing else runnable
	   never gets one. It is kept only as a smoke bound against an infinite
	   park. Catching the real thing needs a second process generating wakeups
	   while this one waits. */
	CHECK(elapsed_ms >= 250, "the timed poll actually waited");
	CHECK(elapsed_ms < 3000, "the timed poll did not overshoot its deadline");
	if (elapsed_ms < 250 || elapsed_ms >= 3000) {
		printf("timed poll took %ld ms, expected about 300\n", elapsed_ms);
	}

	close(self);
	CHECK(read(acc, buf, 4) == 0, "EOF once the peer closes");
	close(acc);

	/* Unlink removes the NAME. Nothing can find it afterwards. */
	CHECK(unlink(SOCKPATH) == 0, "unlink the socket path");
	CHECK(stat(SOCKPATH, &st) == -1, "the path is gone");
	probe = socket(AF_UNIX, SOCK_STREAM, 0);
	errno = 0;
	CHECK(connect(probe, (struct sockaddr *)&a, sizeof a) == -1 && errno == ENOENT,
	      "connect after unlink is ENOENT");
	close(probe);
	close(srv);

	/* Everything above moves one connection at a time. These three cover the
	   backlog, which is the part of the implementation that only a second
	   simultaneous client reaches. */
#define QPATH "/tmp/uds-e2e-queue.sock"
	struct sockaddr_un q;
	memset(&q, 0, sizeof q);
	q.sun_family = AF_UNIX;
	strncpy(q.sun_path, QPATH, sizeof q.sun_path - 1);
	unlink(QPATH);
	int qsrv = socket(AF_UNIX, SOCK_STREAM, 0);
	CHECK(bind(qsrv, (struct sockaddr *)&q, sizeof q) == 0, "bind the queue listener");
	CHECK(listen(qsrv, 8) == 0, "listen with a backlog");

	/* THREE connections queued before any accept, each tagged. Accepting must
	   yield them in CONNECT order -- a backlog is a FIFO, and a queue that
	   happened to pop from the wrong end would still pass every one-at-a-time
	   check above. */
	int qc[3];
	for (int i = 0; i < 3; i++) {
		qc[i] = socket(AF_UNIX, SOCK_STREAM, 0);
		CHECK(qc[i] >= 0, "queue client socket");
		CHECK(connect(qc[i], (struct sockaddr *)&q, sizeof q) == 0, "queue a connection");
		char tag = (char)('0' + i);
		CHECK(write(qc[i], &tag, 1) == 1, "tag the queued connection");
	}
	for (int i = 0; i < 3; i++) {
		int s = accept(qsrv, NULL, NULL);
		CHECK(s >= 0, "accept a queued connection");
		char tag = 0;
		CHECK(read(s, &tag, 1) == 1 && tag == (char)('0' + i),
		      "the backlog is served in connect order");
		close(s);
		close(qc[i]);
	}

	/* A listener closed with a connection still QUEUED. The peer must find out:
	   it has a connected socket whose server will never exist, and if the
	   queued half is simply dropped it waits for a process that is gone. Linux
	   resets it; this asserts only that the connection is dead and the read
	   returns, which is the property that matters and is true on both. */
	int orphan = socket(AF_UNIX, SOCK_STREAM, 0);
	CHECK(connect(orphan, (struct sockaddr *)&q, sizeof q) == 0, "queue a connection to orphan");
	close(qsrv);
	memset(buf, 0, sizeof buf);
	ssize_t on = read(orphan, buf, 4);
	CHECK(on <= 0, "a connection orphaned by the listener's close is dead, not hung");
	close(orphan);
	unlink(QPATH);

	/* TWO processes accepting the same inherited listener, two clients. Every
	   accept so far has been by the one process that called listen. */
#define MPATH "/tmp/uds-e2e-multi.sock"
	struct sockaddr_un m;
	memset(&m, 0, sizeof m);
	m.sun_family = AF_UNIX;
	strncpy(m.sun_path, MPATH, sizeof m.sun_path - 1);
	unlink(MPATH);
	int msrv = socket(AF_UNIX, SOCK_STREAM, 0);
	CHECK(bind(msrv, (struct sockaddr *)&m, sizeof m) == 0, "bind the multi listener");
	CHECK(listen(msrv, 8) == 0, "listen on the multi listener");
	pid_t kids[2];
	for (int i = 0; i < 2; i++) {
		kids[i] = fork();
		CHECK(kids[i] >= 0, "fork an acceptor");
		if (kids[i] == 0) {
			int s = accept(msrv, NULL, NULL);
			if (s < 0) { _exit(21); }
			char c = 0;
			if (read(s, &c, 1) != 1) { _exit(22); }
			if (write(s, &c, 1) != 1) { _exit(23); }
			close(s);
			close(msrv);
			_exit(0);
		}
	}
	for (int i = 0; i < 2; i++) {
		int c = socket(AF_UNIX, SOCK_STREAM, 0);
		CHECK(c >= 0, "multi client socket");
		CHECK(connect(c, (struct sockaddr *)&m, sizeof m) == 0, "connect to the multi listener");
		char ch = (char)('a' + i), back = 0;
		CHECK(write(c, &ch, 1) == 1, "multi client writes");
		CHECK(read(c, &back, 1) == 1 && back == ch, "an acceptor echoed it back");
		close(c);
	}
	for (int i = 0; i < 2; i++) {
		int st2 = 0;
		CHECK(waitpid(kids[i], &st2, 0) == kids[i], "reap an acceptor");
		CHECK(WIFEXITED(st2) && WEXITSTATUS(st2) == 0, "an acceptor exited 0");
		if (WIFEXITED(st2) && WEXITSTATUS(st2) != 0) {
			printf("acceptor exit code %d\n", WEXITSTATUS(st2));
		}
	}
	close(msrv);
	unlink(MPATH);

	/* The ABSTRACT namespace: a leading NUL, then a name that is not a path and
	   has no filesystem node. It is delimited by addrlen, NOT by a NUL, which is
	   the part that is easy to get wrong -- treat it as a C string and two names
	   sharing a prefix silently become one socket.

	   Deliberately reusing SOCKPATH's exact bytes after the NUL: an abstract
	   name and a pathname that spell the same thing are DIFFERENT sockets, and
	   the pathname one was unlinked above, so if the two namespaces were
	   conflated this would find a dead listener rather than the live one. */
	struct sockaddr_un ab;
	memset(&ab, 0, sizeof ab);
	ab.sun_family = AF_UNIX;
	ab.sun_path[0] = 0;
	memcpy(ab.sun_path + 1, SOCKPATH, sizeof SOCKPATH - 1);
	socklen_t ablen = (socklen_t)(sizeof(sa_family_t) + 1 + sizeof SOCKPATH - 1);

	int asrv = socket(AF_UNIX, SOCK_STREAM, 0);
	CHECK(asrv >= 0, "abstract server socket");
	CHECK(bind(asrv, (struct sockaddr *)&ab, ablen) == 0, "bind an abstract name");
	/* No filesystem node, and specifically not at the pathname that shares the
	   spelling -- that is what the leading NUL in the registry key buys. */
	CHECK(stat(SOCKPATH, &st) == -1, "an abstract bind creates no filesystem node");
	CHECK(listen(asrv, 5) == 0, "listen on the abstract name");

	/* A name one byte SHORTER is a different socket. If addrlen were ignored and
	   the name taken as a C string, this would connect to asrv instead. */
	int shorter = socket(AF_UNIX, SOCK_STREAM, 0);
	errno = 0;
	CHECK(connect(shorter, (struct sockaddr *)&ab, ablen - 1) == -1 && errno == ECONNREFUSED,
	      "a prefix of an abstract name is a different socket");
	close(shorter);

	int acli = socket(AF_UNIX, SOCK_STREAM, 0);
	CHECK(acli >= 0, "abstract client socket");
	CHECK(connect(acli, (struct sockaddr *)&ab, ablen) == 0, "connect to the abstract name");
	int aacc = accept(asrv, NULL, NULL);
	CHECK(aacc >= 0, "accept on the abstract name");
	CHECK(write(acli, "abst", 4) == 4, "abstract client writes");
	memset(buf, 0, sizeof buf);
	CHECK(read(aacc, buf, 4) == 4 && memcmp(buf, "abst", 4) == 0, "abstract server reads");

	/* getsockname round-trips the name, NUL included and length-delimited. */
	memset(&got, 0, sizeof got);
	glen = sizeof got;
	CHECK(getsockname(asrv, (struct sockaddr *)&got, &glen) == 0, "abstract getsockname");
	CHECK(glen == ablen && got.sun_path[0] == 0 &&
	      memcmp(got.sun_path + 1, SOCKPATH, sizeof SOCKPATH - 1) == 0,
	      "abstract getsockname round-trips the name");
	close(acli);
	close(aacc);
	close(asrv);

	if (failures == 0) {
		printf("UDS-OK\n");
	}
	return failures == 0 ? 0 : 1;
}
`

// TestUnixDomainSocketsUnderEcvisor guards the in-runtime AF_UNIX
// implementation. See udsGuestSrc for what each check rules out.
//
// It also stands in for the twin it absorbed. There used to be a second test
// here, TestUnixDomainSocketsUnderBoundedSnapshots, from the day bounded
// snapshots became the default and this one was pinned to the full-buffer path
// so the two schemes stayed distinct. The environment variable that selected
// them was REMOVED on 2026-08-22 and bounded snapshots are now the only scheme,
// so both halves would run byte-for-byte the same thing while reading, from
// their names, as two-scheme coverage. One test that runs one scheme is honest;
// two that run one scheme twice are not.
//
// The absorbed twin's reason for existing is worth keeping, because it is why
// this guest is the right smoke test for the snapshot scheme and not merely a
// convenient one: it forks, both processes run concurrently, and they exchange
// data through memory the runtime owns. Every context switch between them takes
// the bounded save/restore path, so a range set that misses something the child
// wrote shows up as a wrong byte in a checked payload rather than as a crash --
// which is the failure mode bounded snapshots have to be trusted not to have.
// It cannot prove the scheme correct on a large workload; it fails fast and
// cheaply when it is obviously wrong, in 12 seconds against the 40 minutes a
// postgres run costs.
func TestUnixDomainSocketsUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "udsg", udsGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "udsg")
	out := runWasmIn(t, ctx, wasm, nil, nil, "")
	assertUDSGuestPassed(t, out)
}

// TestUnixDomainSocketsNativeBaseline runs the same guest on Linux, so the
// expectations describe AF_UNIX rather than ecvisor's model of it. Every errno
// above was taken from this run, not from memory.
func TestUnixDomainSocketsNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "udsg", udsGuestSrc)

	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/udsg")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertUDSGuestPassed(t, out)
}

func assertUDSGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "UDS-OK") {
		t.Errorf("guest did not reach UDS-OK; full output:\n%s", out)
	}
}
