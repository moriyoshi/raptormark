package e2e

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Epolling a SOCKET, on every profile that has one.
//
// # Why this file exists rather than one test in one backend's file
//
// `NetBackend::ready` is reached from exactly ONE place -- `fd_ready`'s socket
// arm, during an `epoll_pwait`/`ppoll` scan. Until 2026-08-24 nothing in `e2e/`
// epolled a socket at all, so that arm was unreached by any run on any profile:
// `timers_test.go` epolls an *eventfd*, every other socket test blocks in
// `accept`/`connect` (which the scheduler serves through `wait`, a different
// call with a different timeout), and `nonblockGuestSrc` is EAGAIN-driven
// throughout and never polls.
//
// The guest below was written for `net::wasix` and closed the gap for that
// profile only, which left the SHIPPING profile's `ready` uncovered -- and its
// `ready` is the one guarding the PostgreSQL postmaster ServerLoop deadlock: a
// listener reported perpetually readable makes the guest `accept()` on an empty
// backlog and block forever.
//
// ⚠️ It was MOVED here, not copied. `.agents/docs/TODO.md` asked for exactly
// that, and the reason is the usual one: two copies of a guest drift, and the
// copy that drifts is the one whose profile nobody is currently debugging.
//
// # What a PASS would look like if the claim were false
//
// A backend whose readiness probe BLOCKS does not fail these tests -- it hangs,
// and the suite times out. That is still a failure, and it is the only shape
// this defect has, so each runner below says so in its diagnostic rather than
// leaving a future reader to work out why there was no output.
//
// A backend that always answers "not ready" passes the first check and
// deadlocks every real server; the second `epoll_wait` is what refuses it.

// epollSocketGuestSrc epolls a SOCKET, which nothing else in `e2e/` does.
//
// ❗ ON `wasix` THIS IS THE ONLY THING THAT COVERS `PROBE_NANOS`, and it covers
// the most dangerous single value in that backend: `net::wasix::ready` probes
// with a ONE-nanosecond clock subscription because WASIX reads a ZERO as
// `Duration::MAX`. Copy `net::wasmedge::ready`, which correctly uses 0 for
// preview1, and the guest hangs inside its first epoll with no error, no bad
// import and nothing to grep for.
//
// The FIRST epoll is the load-bearing one: nothing is pending, so a correct
// backend returns 0 after the timeout and a zero-probe backend never returns.
// The second proves the probe can still say "yes", which stops the fix
// degenerating into "always report not-ready".
//
// Self-contained: it is its own peer over loopback, so nothing outside the
// guest can make it flaky, and it binds port 0 so two profiles' runs cannot
// collide on a fixed port the way `AGENTS.md` warns about.
const epollSocketGuestSrc = `#define _GNU_SOURCE
#include <arpa/inet.h>
#include <errno.h>
#include <netinet/in.h>
#include <stdio.h>
#include <string.h>
#include <sys/epoll.h>
#include <sys/socket.h>
#include <unistd.h>

static int failures = 0;
#define CHECK(cond, what) do { if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } } while (0)

int main(void) {
	int ln = socket(AF_INET, SOCK_STREAM, 0);
	CHECK(ln >= 0, "socket");

	struct sockaddr_in a;
	memset(&a, 0, sizeof a);
	a.sin_family = AF_INET;
	a.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
	a.sin_port = 0; /* let the host choose, so two runs cannot collide */
	CHECK(bind(ln, (struct sockaddr *)&a, sizeof a) == 0, "bind");
	CHECK(listen(ln, 4) == 0, "listen");

	socklen_t alen = sizeof a;
	CHECK(getsockname(ln, (struct sockaddr *)&a, &alen) == 0, "getsockname");
	CHECK(a.sin_port != 0, "getsockname gave a real port");

	int ep = epoll_create1(0);
	CHECK(ep >= 0, "epoll_create1");
	struct epoll_event ev;
	ev.events = EPOLLIN;
	ev.data.fd = ln;
	CHECK(epoll_ctl(ep, EPOLL_CTL_ADD, ln, &ev) == 0, "epoll_ctl");

	/* THE ONE THAT MATTERS. Nothing has connected, so this must time out and
	   return 0. A backend whose readiness probe blocks never gets here at all,
	   and the test times out instead of failing -- which is still a fail, and
	   is the only shape this defect has. */
	struct epoll_event out[4];
	int n = epoll_wait(ep, out, 4, 200);
	CHECK(n == 0, "epoll on an idle listener must return 0, not a connection");

	/* And it must still be able to say yes. */
	int c = socket(AF_INET, SOCK_STREAM, 0);
	CHECK(c >= 0, "client socket");
	CHECK(connect(c, (struct sockaddr *)&a, sizeof a) == 0, "connect to self");

	n = epoll_wait(ep, out, 4, 2000);
	CHECK(n == 1, "epoll must report the pending connection");
	if (n == 1) {
		CHECK(out[0].data.fd == ln, "the ready fd is the listener");
		CHECK((out[0].events & EPOLLIN) != 0, "the listener is READABLE");
		int s = accept(ln, NULL, NULL);
		CHECK(s >= 0, "accept the connection epoll promised");
		if (s >= 0) close(s);
	}

	close(c);
	close(ep);
	close(ln);
	if (failures == 0) puts("EPSOCK-OK");
	return failures == 0 ? 0 : 1;
}
`

// assertEpollSocketOK is the single set of expectations every profile's run is
// judged against.
//
// ⚠️ It checks BOTH the per-check FAIL lines and the terminal banner, and
// neither is redundant. A guest that dies partway through -- which is what a
// trap or an unlifted instruction looks like -- prints no FAIL line at all, so
// the banner is the only thing that notices. A guest that runs to completion
// with a wrong answer prints FAIL and still exits, so the banner alone would
// not.
func assertEpollSocketOK(t *testing.T, profile, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("[%s] guest check failed: %s", profile, strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "EPSOCK-OK") {
		t.Errorf("[%s] the guest did not reach EPSOCK-OK; full output:\n%s", profile, out)
	}
}

// epollHangHint is appended to every runner's failure diagnostic. It names the
// symptom that is NOT a failure message, because that is the one a reader will
// otherwise misread as an infrastructure problem.
const epollHangHint = "\n⚠️ If it produced no output at all, the readiness probe BLOCKED rather " +
	"than answering. `NetBackend::ready` must return promptly with a negative " +
	"answer when nothing is pending -- see the clock-timeout note in " +
	"runtime/src/net/wasix.rs (PROBE_NANOS), whose value differs from the " +
	"preview1 backends' for a reason."

// TestShippingProfileEpollsASocketWithoutHanging covers `net::wasmedge::ready`
// -- the DEFAULT profile's, and the one that has never been exercised.
//
// ❗ This is the profile that actually ships, and the one whose `ready` guards
// the postmaster ServerLoop. Every other epoll-a-socket test in this tree runs
// a backend nothing deploys.
func TestShippingProfileEpollsASocketWithoutHanging(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "epsockedge", epollSocketGuestSrc)
	// No --profile: the default IS the shipping one, and naming it explicitly
	// would let a change to the default pass this test under the old backend.
	wasm := liftOne(t, ctx, img, dir, elf, "epsockedge")

	out, err := runEpollGuest(ctx, t, wasm, "wasmedge --enable-all", nil)
	if err != nil {
		t.Fatalf("the epoll guest failed under wasmedge: %v\n%s%s", err, out, epollHangHint)
	}
	assertEpollSocketOK(t, "wasmedge", out)
}

// TestLoopbackProfileEpollsASocketWithoutHanging covers `net::loopback::ready`.
//
// The loopback backend serves the whole exchange IN PROCESS, so its `ready` is
// answered from its own tables rather than by a host call -- a different
// implementation of the same contract, and the one a stock `wasmtime` runs.
// Following `TestLoopbackProfileServesGuestLocalSockets`, which is why wasmtime
// and not wasmedge.
func TestLoopbackProfileEpollsASocketWithoutHanging(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "epsocklb", epollSocketGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "epsocklb", "--profile", "loopback")

	out, err := runEpollGuest(ctx, t, wasm, "wasmtime run", nil)
	if err != nil {
		t.Fatalf("the epoll guest failed under stock wasmtime: %v\n%s%s", err, out, epollHangHint)
	}
	assertEpollSocketOK(t, "loopback", out)
}

// TestBrowserProfileEpollsASocketWithoutHanging covers `net::browser::ready`.
//
// ❗ THIS IS THE BACKEND WHOSE `wait` NEVER BLOCKS, which makes it the one where
// an epoll is most likely to be answered wrongly and least likely to be noticed:
// the guest goes idle, the host re-enters it, and a `ready` that always said
// "no" would look like a guest that simply had nothing to do. The idle assertion
// below is what separates those two.
//
// Run under Node rather than a browser, for the reason `browserprofile_test.go`
// states at length: what a browser adds is a transport and an event loop, not a
// different ABI or scheduler contract.
func TestBrowserProfileEpollsASocketWithoutHanging(t *testing.T) {
	img := requireE2E(t)
	node := requireNode(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "epsockbr", epollSocketGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "epsockbr", "--profile", "browser")

	out := runNodeHostArgs(t, ctx, node, wasm, "", "--reentrant", "--net-v1")
	assertEpollSocketOK(t, "browser", out)
	if !strings.Contains(out, "HOST-EXIT: 0") {
		t.Errorf("non-zero exit under the browser profile; full output:\n%s%s", out, epollHangHint)
	}
	// It must have gone IDLE at least once. A backend that never blocks has no
	// other way to wait out the 200 ms epoll, so a run with zero idle slices did
	// its waiting somewhere it must not -- and would still print EPSOCK-OK.
	if m := reSliceIdle.FindStringSubmatch(out); m == nil || m[1] == "0" {
		t.Errorf("the guest never went idle, so the browser backend blocked "+
			"somewhere it must not. Full output:\n%s", out)
	}
}

// runEpollGuest runs one lifted module under one runtime, mounting only the
// directory the module is in.
//
// ⚠️ `env` goes BEFORE the module path. wasmedge stops reading its own flags
// there and does not inherit the host environment at all, which `e2e_test.go`
// records; wasmtime tolerates the same placement, so one helper serves both.
func runEpollGuest(ctx context.Context, t *testing.T, wasm, runtimeCmd string, env []string) (string, error) {
	t.Helper()
	dir, err := filepath.Abs(filepath.Dir(wasm))
	if err != nil {
		t.Fatal(err)
	}
	cmd := runtimeCmd
	for _, e := range env {
		cmd += " --env " + e
	}
	return dockerRun(ctx, []string{"-v", dir + ":/out"}, cmd+" /out/"+filepath.Base(wasm))
}
