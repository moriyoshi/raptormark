package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// execve from a NON-LEADER thread. The runtime side has existed since
// 2026-08-13 -- `exec_into` retires the thread group and moves the leader's pid
// to the execing task -- and until now nothing had ever executed it, because
// `liftOne` builds a single-program module with no exec map and `execv` there
// resolves back to the same program. This is that verification, in the
// two-program harness `crossprog_test.go` already carries.
//
// Three claims, and they fail separately:
//
//   - the exec RESOLVES from a worker thread at all. A runtime that only looks
//     at the calling task's own program, or that refuses a non-leader, dies
//     here with the worker's _exit(90).
//
//   - the new image keeps the GROUP's pid. Linux moves leader identity to the
//     execing task, so `getpid()` after the exec is the OLD tgid, not the
//     worker's tid. A runtime that execs "the calling task" leaves the pid as
//     the tid, and the child reports the mismatch with the two numbers.
//
//   - the old group is RETIRED. This is the one a marker-only test cannot see:
//     the leader is parked in a 3-second sleep at the moment of the exec, so a
//     group that was not retired is rescheduled afterwards -- into a program
//     that has been replaced. It needs a working nanosleep to express, which is
//     why it could not be written before 2026-08-15.
//
//     ⚠️ The observed symptom of that defect is NOT the guest's FAIL line. With
//     retirement removed from `exec_into` and everything else left in place,
//     the module dies at t=3.3s with `called Option::unwrap() on a None value`
//     in `load_current` -- the leader's saved state was cleared by the rest of
//     the exec, so it cannot even be re-entered. The FAIL line is the backstop
//     for a runtime that manages to resume it; the panic is what this one does.
//     Either way the test fails, which is what the check is for.
//
// NEUTRALIZED 2026-08-15, twice, against builds with each half of `exec_into`
// removed in turn: no pid move -> "the execed image is single-threaded" fires;
// no retirement -> the t=3.3s panic above. The first attempt at this test could
// NOT see the second defect at all, because the execed image exited ~100 ms in
// and the module was gone before the leader's sleep expired. That is why
// xexeced sleeps past it.
const execThreadGuestSrc = `#define _GNU_SOURCE
#include <pthread.h>
#include <stdio.h>
#include <string.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>

static pid_t self_tid(void) { return (pid_t)syscall(SYS_gettid); }

static void *execer(void *unused)
{
	(void)unused;
	pid_t tid = self_tid(), pid = getpid();
	if (tid == pid) {
		/* Not a worker at all: every claim below would be vacuous. */
		printf("FAIL the execing task is the group leader\n");
		fflush(stdout);
		_exit(89);
	}
	/* Let the leader reach its sleep first, so the exec really does happen
	   with another member of the group parked. */
	struct timespec ts = { 0, 50 * 1000000L };
	nanosleep(&ts, NULL);

	char tgid[16], mytid[16];
	snprintf(tgid, sizeof tgid, "%d", (int)pid);
	snprintf(mytid, sizeof mytid, "%d", (int)tid);
	char *const argv[] = { "/bin/xexeced", tgid, mytid, 0 };
	execv("/bin/xexeced", argv);
	printf("FAIL execv from a worker thread did not resolve\n");
	fflush(stdout);
	_exit(90);
}

int main(void)
{
	printf("check execve-from-non-leader\n");
	fflush(stdout);

	pthread_t th;
	if (pthread_create(&th, NULL, execer, NULL) != 0) {
		printf("FAIL create the execing worker\n");
		return 1;
	}

	/* The leader parks here and must NEVER come back: the exec retires the
	   whole group, this task included. If it does come back, it prints from a
	   program that has been replaced -- which is the observable form of "the
	   old group was not retired". */
	struct timespec ts = { 3, 0 };
	nanosleep(&ts, NULL);
	printf("FAIL the leader survived an execve by one of its threads\n");
	fflush(stdout);
	return 1;
}
`

// The image the exec lands in. It also creates a thread, because a group that
// was retired and rebuilt is exactly where a stale tgid or a leaked task shows
// up -- and because "the new image is single-threaded" is only meaningful if it
// can still become multi-threaded.
const execThreadTargetSrc = `#define _GNU_SOURCE
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>

static pid_t self_tid(void) { return (pid_t)syscall(SYS_gettid); }

static int failures = 0;
#define CHECK(c, what) do { if (!(c)) { printf("FAIL %s\n", what); failures++; } } while (0)

static pid_t worker_tid = 0;
static void *worker(void *unused) { (void)unused; worker_tid = self_tid(); return NULL; }

int main(int argc, char **argv)
{
	if (argc < 3) {
		printf("FAIL the execed image lost its argv\n");
		return 1;
	}
	pid_t want_tgid = (pid_t)atoi(argv[1]);
	pid_t execer_tid = (pid_t)atoi(argv[2]);
	pid_t pid = getpid(), tid = self_tid();

	if (pid != want_tgid) {
		/* Print both numbers: "kept the tid" and "invented a pid" are
		   different defects and the values say which. */
		printf("FAIL the execed image has pid %d, not the group's %d (execer tid was %d)\n",
		       (int)pid, (int)want_tgid, (int)execer_tid);
		failures++;
	}
	CHECK(pid == tid, "the execed image is single-threaded (gettid == getpid)");

	pthread_t th;
	CHECK(pthread_create(&th, NULL, worker, NULL) == 0,
	      "the rebuilt group can still create a thread");
	CHECK(pthread_join(th, NULL) == 0, "and join it");
	CHECK(worker_tid != 0 && worker_tid != tid,
	      "the post-exec thread has its own tid");

	/* OUTLIVE THE OLD LEADER'S SLEEP, or the retirement claim is not tested at
	   all. The leader parks for 3 s and the exec happens ~50 ms in, so a group
	   that was NOT retired prints its FAIL line at t=3 s -- and if this image
	   exited at t=0.1 s the module would be gone before that could happen.
	   Measured: with retirement deliberately removed and this sleep absent, the
	   run reported only the pid defect and the leader's line never appeared. */
	struct timespec ts = { 3, 500 * 1000000L };
	nanosleep(&ts, NULL);

	if (failures == 0) {
		printf("EXECTHREAD-OK\n");
	}
	return failures == 0 ? 0 : 1;
}
`

// TestExecFromNonLeaderThreadUnderEcvisor is the verification the runtime's
// exec-from-a-thread path never had. See execThreadGuestSrc for what each check
// isolates.
func TestExecFromNonLeaderThreadUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elfA := compileGuest(t, ctx, dir, "xthread", execThreadGuestSrc)
	elfB := compileGuest(t, ctx, dir, "xexeced", execThreadTargetSrc)
	wasm, sidecar := liftTwoNamed(t, ctx, img, dir,
		[2]string{"xthread", "xexeced"}, [2]string{elfA, elfB})

	out := runWasmIn(t, ctx, wasm, nil,
		[]string{"RAPTORMARK_ROOTFS=/" + filepath.Base(sidecar)}, "/:/out")
	assertExecThreadPassed(t, out)
}

// TestExecFromNonLeaderThreadNativeBaseline pins the expectations to the kernel.
// It runs the pair as two real binaries, so /bin/xexeced has to exist on disk --
// under ecvisor the same path is resolved through the exec map instead.
func TestExecFromNonLeaderThreadNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "xthread", execThreadGuestSrc)
	compileGuest(t, ctx, dir, "xexeced", execThreadTargetSrc)

	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx, []string{"-v", abs + ":/w"},
		"cp /w/xexeced /bin/xexeced && /w/xthread")
	if err != nil {
		t.Errorf("native run failed: %v", err)
	}
	assertExecThreadPassed(t, out)
}

func assertExecThreadPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "EXECTHREAD-OK") {
		t.Errorf("guest did not reach EXECTHREAD-OK; full output:\n%s", out)
	}
}
