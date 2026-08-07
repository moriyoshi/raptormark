package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// dumpableGuestSrc exercises prctl(PR_SET_DUMPABLE / PR_GET_DUMPABLE), which
// every nginx worker calls and which ecvisor used to refuse with EINVAL from the
// NR_PRCTL catch-all. Linux returns 0 for it, so the refusal was a divergence.
//
// Every expectation here is written as an assertion INSIDE the guest, and the
// same source runs under ecvisor and natively (…NativeBaseline). That is what
// pins the expectations to the kernel rather than to a model of it: if any line
// below is wrong about Linux, the native run says so.
//
// PR_SET_DUMPABLE governs core dumps and same-uid ptrace, and ecvisor offers
// neither -- so accepting it is truthful precisely because nothing observable
// follows from it. The test therefore checks the syscall contract (which values
// are accepted, what the GET reports, what fork/threads inherit) and NOTHING
// about dumps; there is no core-dump behaviour to claim.
const dumpableGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/prctl.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <unistd.h>

static int failures = 0;
#define CHECK(c, what) do { if (!(c)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } } while (0)

/* The raw syscall, so an argument WIDER than 32 bits can be passed: prctl(3)
   takes it as unsigned long and the kernel compares the whole thing. */
static long pr(long op, unsigned long a2)
{
	errno = 0;
	return syscall(SYS_prctl, op, a2, 0UL, 0UL, 0UL);
}

static void *worker(void *unused)
{
	(void)unused;
	/* Per-MM in Linux, so a thread's SET is what its siblings GET. */
	CHECK(pr(PR_SET_DUMPABLE, 0) == 0, "worker thread PR_SET_DUMPABLE(0)");
	return NULL;
}

int main(void)
{
	/* A process that has never called prctl reads Linux's boot value. */
	CHECK(pr(PR_GET_DUMPABLE, 0) == 1, "initial PR_GET_DUMPABLE == 1");

	CHECK(pr(PR_SET_DUMPABLE, 0) == 0, "PR_SET_DUMPABLE(0) succeeds");
	CHECK(pr(PR_GET_DUMPABLE, 0) == 0, "PR_GET_DUMPABLE reads back 0");

	/* SUID_DUMP_ROOT (2) exists but userspace may not set it, and a REFUSED
	   set must not have stored anything either. */
	errno = 0;
	CHECK(pr(PR_SET_DUMPABLE, 2) == -1 && errno == EINVAL, "PR_SET_DUMPABLE(2) is EINVAL");
	CHECK(pr(PR_GET_DUMPABLE, 0) == 0, "a refused set left the value alone");

	/* The 64-bit form of 1. Truncating to 32 bits would accept it. */
	errno = 0;
	CHECK(pr(PR_SET_DUMPABLE, 0x100000001UL) == -1 && errno == EINVAL,
	      "PR_SET_DUMPABLE(0x100000001) is EINVAL");
	CHECK(pr(PR_GET_DUMPABLE, 0) == 0, "a wide argument did not sneak through");

	/* What nginx's worker actually calls. */
	CHECK(pr(PR_SET_DUMPABLE, 1) == 0, "PR_SET_DUMPABLE(1) succeeds");
	CHECK(pr(PR_GET_DUMPABLE, 0) == 1, "PR_GET_DUMPABLE reads back 1");

	/* fork copies the mm's flag. */
	CHECK(pr(PR_SET_DUMPABLE, 0) == 0, "PR_SET_DUMPABLE(0) before fork");
	pid_t child = fork();
	if (child == 0) {
		_exit(pr(PR_GET_DUMPABLE, 0) == 0 ? 42 : 43);
	}
	CHECK(child > 0, "fork");
	int st = 0;
	CHECK(waitpid(child, &st, 0) == child, "reap the child");
	CHECK(WIFEXITED(st) && WEXITSTATUS(st) == 42, "the child inherited dumpable=0");

	/* Back to 1, so the thread check below observes a CHANGE rather than a
	   value that was already 0. (execve's reset to 1 is not checked here:
	   liftOne builds a single-program module with nothing to exec into --
	   the same limitation threadproc_test.go records.) */
	CHECK(pr(PR_SET_DUMPABLE, 1) == 0, "PR_SET_DUMPABLE(1) after fork");

	pthread_t th;
	CHECK(pthread_create(&th, NULL, worker, NULL) == 0, "create worker");
	CHECK(pthread_join(th, NULL) == 0, "join worker");
	CHECK(pr(PR_GET_DUMPABLE, 0) == 0, "a worker's PR_SET_DUMPABLE reached the group");

	if (failures == 0) {
		printf("DUMPABLE-OK\n");
	}
	return failures == 0 ? 0 : 1;
}
`

// TestPrctlDumpableUnderEcvisor guards the PR_SET_DUMPABLE / PR_GET_DUMPABLE
// ruling: accepted, stored, and readable back.
func TestPrctlDumpableUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	elf := compileGuest(t, ctx, dir, "dumpable", dumpableGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "dumpable")
	assertDumpableGuestPassed(t, runWasm(t, ctx, wasm))
}

// TestPrctlDumpableNativeBaseline pins the expectations to Linux.
func TestPrctlDumpableNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "dumpable", dumpableGuestSrc)
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx, []string{"-v", abs + ":/w"}, "/w/dumpable")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertDumpableGuestPassed(t, out)
}

func assertDumpableGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "DUMPABLE-OK") {
		t.Errorf("guest did not reach DUMPABLE-OK; full output:\n%s", out)
	}
}
