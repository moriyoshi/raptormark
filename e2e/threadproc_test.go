package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// threadprocGuestSrc covers where THREADS meet the rest of the process model.
// `e2e/threads_test.go` covers threads on their own; this is the interaction.
//
// A child forked FROM a worker thread must be an ordinary single-threaded
// process, its parent must be the thread GROUP rather than the forking task
// (`getppid` is the tgid), and the MAIN thread must be able to reap it. Getting
// the parent wrong would make a threaded server's children unreapable.
//
// ❌ Two further interactions were written here and removed, because this
// harness cannot express them, not because they are uninteresting:
//
//   - a signal to a threaded process. ecvisor delivers guest handlers only from
//     `ppoll` and `epoll_pwait`, so a `kill` followed by anything else never
//     runs one. That is a real pre-existing gap and nothing to do with threads;
//     nginx and postgres never met it because both wait in epoll. See TODO.
//   - execve from a non-leader thread. `liftOne` builds a SINGLE-program module
//     with no exec map, so `execv(argv[0])` has nothing to resolve. It belongs
//     in the multi-program harness of `crossprog_test.go`. The runtime side is
//     implemented (`exec_into` retires the group and moves the leader pid) but
//     is therefore UNVERIFIED. See TODO.
const threadprocGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <pthread.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

static int failures = 0;
#define CHECK(c, what) do { if (!(c)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } } while (0)

static volatile sig_atomic_t got_usr1 = 0;
static void on_usr1(int s) { (void)s; got_usr1 = 1; }

static pid_t main_pid;
static volatile int worker_ran = 0;

/* Forks from inside a thread. The child must be a single-threaded process whose
   parent is the GROUP, and it must be reapable by the main thread. */
static void *forker(void *unused)
{
	(void)unused;
	worker_ran = 1;
	pid_t p = fork();
	if (p == 0) {
		/* In the child: single-threaded by definition, ppid is the group. */
		_exit(getppid() == main_pid ? 42 : 43);
	}
	return (void *)(long)p;
}

int main(void)
{
	main_pid = getpid();

	printf("check fork-from-thread\n");
	fflush(stdout);
	pthread_t th;
	void *ret = NULL;
	CHECK(pthread_create(&th, NULL, forker, NULL) == 0, "create forker");
	CHECK(pthread_join(th, &ret) == 0, "join forker");
	CHECK(worker_ran, "the forking thread actually ran");
	pid_t child = (pid_t)(long)ret;
	CHECK(child > 0, "fork from a thread succeeded");
	int st = 0;
	CHECK(waitpid(child, &st, 0) == child, "the main thread reaps a thread's child");
	CHECK(WIFEXITED(st), "child exited normally");
	CHECK(WEXITSTATUS(st) == 42, "the child's getppid() is the thread GROUP");

	if (failures == 0) {
		printf("THREADPROC-OK\n");
	}
	return failures == 0 ? 0 : 1;
}
`

// TestThreadProcessModelUnderEcvisor guards the thread/process interactions.
func TestThreadProcessModelUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	elf := compileGuest(t, ctx, dir, "threadproc", threadprocGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "threadproc")
	assertThreadProcGuestPassed(t, runWasm(t, ctx, wasm))
}

// TestThreadProcessModelNativeBaseline pins the expectations to Linux.
func TestThreadProcessModelNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "threadproc", threadprocGuestSrc)
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx, []string{"-v", abs + ":/w"}, "/w/threadproc")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertThreadProcGuestPassed(t, out)
}

func assertThreadProcGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "THREADPROC-OK") {
		t.Errorf("guest did not reach THREADPROC-OK; full output:\n%s", out)
	}
}
