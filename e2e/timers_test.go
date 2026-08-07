package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// epollTimerGuestSrc measures what epoll_wait's timeout argument actually does.
//
// On aarch64 there is no epoll_wait syscall; glibc routes it to epoll_pwait
// (nr 22) with a NULL sigmask, so this exercises ecvisor's sys_epoll_pwait
// directly. The argument is a count of MILLISECONDS, negative meaning "wait
// forever", and ecvisor read it only as a boolean for a long time: every finite
// wait became an infinite one, and no guest timer could fire at all. That is not
// an abstract gap -- nginx drives its whole timer wheel off this one argument,
// and a traced run of the lifted nginx shows it asking for 5000 ms
// (keepalive_timeout 5) and 60000 ms on both libcs.
//
// The checks are ordered so a partial pass says which property broke:
//
//	SHORT/LONG   a finite timeout must elapse, and roughly in proportion. The
//	             single-process case is the sharp one: with nothing else
//	             runnable and no socket waiter, the old code returned 0
//	             IMMEDIATELY rather than waiting, so a broken build fails this
//	             with elapsed ~0 ms rather than by hanging.
//	ZERO         timeout 0 must still poll and return at once, not sleep.
//	READY        a signalled fd must return promptly despite a long timeout --
//	             i.e. arming a deadline must not have replaced readiness.
//
// Bounds are deliberately loose at the top end. This runs under a cooperative
// scheduler on a host that is routinely at load average ~17, so lateness is
// expected and only earliness is diagnostic.
const epollTimerGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <stdint.h>
#include <stdio.h>
#include <sys/epoll.h>
#include <sys/eventfd.h>
#include <time.h>
#include <unistd.h>

static int failures = 0;
#define CHECK(cond, what) do { if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } } while (0)

static long long now_ms(void) {
	struct timespec t;
	clock_gettime(CLOCK_MONOTONIC, &t);
	return (long long)t.tv_sec * 1000 + t.tv_nsec / 1000000;
}

/* Returns the epoll_wait return value; stores elapsed milliseconds in *el. */
static int timed_wait(int ep, int timeout_ms, long long *el) {
	struct epoll_event ev[8];
	long long t0 = now_ms();
	int n = epoll_wait(ep, ev, 8, timeout_ms);
	*el = now_ms() - t0;
	return n;
}

int main(void) {
	int ep = epoll_create1(0);
	CHECK(ep >= 0, "epoll_create1");

	/* An eventfd that is NOT signalled: the set is non-empty, so this is a
	   real wait rather than a degenerate empty-set case, but nothing can ever
	   make it ready. Only the clock can end these waits. */
	int efd = eventfd(0, EFD_NONBLOCK);
	CHECK(efd >= 0, "eventfd");
	struct epoll_event reg = { .events = EPOLLIN, .data = { .u64 = 1 } };
	CHECK(epoll_ctl(ep, EPOLL_CTL_ADD, efd, &reg) == 0, "epoll_ctl add");

	long long el = 0;
	int n = timed_wait(ep, 300, &el);
	printf("SHORT n=%d elapsed=%lldms\n", n, el);
	CHECK(n == 0, "short wait returned events");
	CHECK(el >= 250, "short wait returned early (timeout ignored)");
	CHECK(el < 5000, "short wait far overshot");

	long long el2 = 0;
	n = timed_wait(ep, 900, &el2);
	printf("LONG n=%d elapsed=%lldms\n", n, el2);
	CHECK(n == 0, "long wait returned events");
	CHECK(el2 >= 800, "long wait returned early (timeout ignored)");
	CHECK(el2 < 8000, "long wait far overshot");
	/* Proportionality: rules out a fixed-interval wake that happens to clear
	   the 250 ms floor. */
	CHECK(el2 > el + 200, "long wait was not meaningfully longer than short");

	long long el3 = 0;
	n = timed_wait(ep, 0, &el3);
	printf("ZERO n=%d elapsed=%lldms\n", n, el3);
	CHECK(n == 0, "zero-timeout poll returned events");
	CHECK(el3 < 200, "zero timeout slept");

	/* Readiness must still win over the deadline. */
	uint64_t one = 1;
	CHECK(write(efd, &one, sizeof one) == (ssize_t)sizeof one, "eventfd write");
	long long el4 = 0;
	n = timed_wait(ep, 10000, &el4);
	printf("READY n=%d elapsed=%lldms\n", n, el4);
	CHECK(n == 1, "ready fd not reported");
	CHECK(el4 < 2000, "ready fd waited for the timeout");

	if (failures == 0) {
		printf("EPOLLTIMER-OK\n");
	}
	return failures == 0 ? 0 : 1;
}
`

// TestEpollTimeoutUnderEcvisor is the regression guard for epoll_pwait's timeout
// argument. See epollTimerGuestSrc for what each check pins down.
//
// Neutralized against the pre-fix runtime (raptormark-builder:arenaskip2): the
// guest reports SHORT n=0 elapsed=0ms and fails "short wait returned early",
// which is the intended diagnostic rather than a hang or a compile error.
func TestEpollTimeoutUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "epolltimer", epollTimerGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "epolltimer")

	out := runWasm(t, ctx, wasm)
	assertEpollTimerGuestPassed(t, out)
}

// TestEpollTimeoutNativeBaseline runs the same guest natively, so a failure
// under ecvisor cannot be blamed on the guest's own arithmetic or on the host
// being too loaded to hit the bounds.
func TestEpollTimeoutNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "epolltimer", epollTimerGuestSrc)

	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx, []string{"-v", abs + ":/w"}, "/w/epolltimer")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertEpollTimerGuestPassed(t, out)
}

func assertEpollTimerGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "EPOLLTIMER-OK") {
		t.Errorf("guest did not reach EPOLLTIMER-OK; full output:\n%s", out)
	}
}
