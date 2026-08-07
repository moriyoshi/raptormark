package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// waitstatusGuestSrc is the ONE thing `wait4` cannot express with a single
// integer: whether a child exited or was killed.
//
// Linux packs the two into different bits of the same status word --
// `(code & 0xff) << 8` for an exit, the bare signal number in the low 7 bits for
// a kill -- and a parent is expected to tell them apart. ecvisor used to make
// every signal death a `Zombie(128 + signo)`, so the parent read `WIFEXITED`
// with status 143 for an uncaught SIGTERM instead of `WIFSIGNALED` with
// `WTERMSIG == 15`. Non-zero and recoverable, but not the kernel's encoding:
// PostgreSQL's postmaster (`LogChildExit`) branches on exactly this to decide
// between "terminated by signal %d" and "exited with exit code %d", and only the
// former drives its crash-restart path.
//
// ⚠️ PHASE 3 IS THE ONE THAT MATTERS AND IT IS NOT REDUNDANT. A child that
// really calls `_exit(143)` must come back `WIFEXITED`/143, and a child killed
// by SIGTERM must come back `WIFSIGNALED`/15 -- the SAME number under the old
// encoding, and the only pair a runtime that merely renumbered things cannot
// satisfy. Phases 1 and 2 alone would pass on a runtime that reported every
// death as a signal death.
//
// Every phase is run natively as well (`…NativeBaseline`), so the expectations
// are pinned to Linux rather than to what this runtime happens to do.
const waitstatusGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

static int failures = 0;
#define CHECK(c, what) do { if (!(c)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } } while (0)

/* A child that never exits on its own: it has to be killed. The short sleep is
   deliberate -- it gives the child a scheduling point to take the signal at,
   and it keeps the loop from being a busy spin that starves the parent under a
   cooperative scheduler. */
static void never_returns(void)
{
	for (;;) {
		struct timespec ts = { 0, 20 * 1000 * 1000 };
		nanosleep(&ts, NULL);
	}
}

/* Kills a child with the given signal and reports the status word its
   parent reads. */
static void killed_by(int sig, const char *name)
{
	pid_t kid = fork();
	CHECK(kid >= 0, "fork a child to kill");
	if (kid == 0) {
		never_returns();
		_exit(99);   /* unreachable */
	}
	/* Let the child reach its sleep loop before signalling it. */
	struct timespec ts = { 0, 50 * 1000 * 1000 };
	nanosleep(&ts, NULL);
	CHECK(kill(kid, sig) == 0, "kill the child");

	int st = -1;
	CHECK(waitpid(kid, &st, 0) == kid, "reap the killed child");
	printf("  %s: status=0x%04x WIFEXITED=%d WIFSIGNALED=%d WTERMSIG=%d\n",
	       name, st & 0xffff, WIFEXITED(st) ? 1 : 0,
	       WIFSIGNALED(st) ? 1 : 0, WIFSIGNALED(st) ? WTERMSIG(st) : -1);
	CHECK(WIFSIGNALED(st), "a killed child reports WIFSIGNALED");
	CHECK(!WIFEXITED(st), "a killed child does NOT report WIFEXITED");
	CHECK(WIFSIGNALED(st) && WTERMSIG(st) == sig, "WTERMSIG is the signal");
	/* No core is written -- Linux does not set this under the container
	   default RLIMIT_CORE=0 either, so claiming it would advertise a file
	   that does not exist. */
	CHECK(!(WIFSIGNALED(st) && WCOREDUMP(st)), "no core dump is claimed");
}

/* ...and the other half: a child that exits normally must NOT read as killed. */
static void exited_with(int code, const char *name)
{
	pid_t kid = fork();
	CHECK(kid >= 0, "fork a child to exit");
	if (kid == 0)
		_exit(code);

	int st = -1;
	CHECK(waitpid(kid, &st, 0) == kid, "reap the exited child");
	printf("  %s: status=0x%04x WIFEXITED=%d WEXITSTATUS=%d WIFSIGNALED=%d\n",
	       name, st & 0xffff, WIFEXITED(st) ? 1 : 0,
	       WIFEXITED(st) ? WEXITSTATUS(st) : -1, WIFSIGNALED(st) ? 1 : 0);
	CHECK(WIFEXITED(st), "an exited child reports WIFEXITED");
	CHECK(!WIFSIGNALED(st), "an exited child does NOT report WIFSIGNALED");
	CHECK(WIFEXITED(st) && WEXITSTATUS(st) == code, "WEXITSTATUS is the code");
}

int main(void)
{
	printf("check wait-status encoding\n");
	fflush(stdout);

	/* PHASE 1 -- an ordinary exit. The control: a runtime that reported
	   everything as a signal death would fail here and nowhere else. */
	exited_with(7, "exit(7)");

	/* PHASE 2 -- an uncaught SIGTERM. The default action terminates. */
	killed_by(SIGTERM, "SIGTERM");

	/* PHASE 2b -- SIGKILL, which reaches a task no ordinary signal wakes and
	   takes a different path through the scheduler to the same encoding. */
	killed_by(SIGKILL, "SIGKILL");

	/* PHASE 3 -- ⚠️ THE DISCRIMINATOR. 143 is 128 + SIGTERM, which is what a
	   SHELL prints for a SIGTERM death -- and what the old encoding put in
	   the status word. A child that genuinely calls _exit(143) must be
	   distinguishable from a child SIGTERM killed, and under the old
	   encoding it was not: both produced WIFEXITED with 143. */
	exited_with(128 + SIGTERM, "exit(143)");

	if (failures == 0) {
		printf("WAITSTATUS-OK\n");
	}
	return failures == 0 ? 0 : 1;
}
`

// TestWaitStatusEncodingUnderEcvisor is the runtime side.
func TestWaitStatusEncodingUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	elf := compileGuest(t, ctx, dir, "waitstatus", waitstatusGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "waitstatus")
	assertWaitStatusGuestPassed(t, runWasm(t, ctx, wasm))
}

// TestWaitStatusEncodingNativeBaseline pins the expectations to Linux. Without
// it the guest asserts this runtime's model of the encoding rather than the
// kernel's, which is the failure mode a hand-written wait-status test is most
// prone to.
func TestWaitStatusEncodingNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "waitstatus", waitstatusGuestSrc)
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx, []string{"-v", abs + ":/w"}, "/w/waitstatus")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertWaitStatusGuestPassed(t, out)
}

func assertWaitStatusGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	// ⚠️ The completion marker is not decoration. Every check above is a
	// `CHECK` inside the guest, so a guest that died before reaching phase 3 --
	// which is exactly what a wedged `waitpid` or a child that never takes its
	// signal looks like -- would print no FAIL line and pass silently.
	if !strings.Contains(out, "WAITSTATUS-OK") {
		t.Errorf("guest did not reach its completion marker:\n%s", out)
	}
}
