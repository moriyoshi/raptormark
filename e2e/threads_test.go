package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// threadsGuestSrc exercises `clone(CLONE_THREAD)` -- real shared-VM threads.
//
// Before this existed, `sys_clone` returned ENOSYS for CLONE_THREAD and any
// guest that created a thread died at its first `pthread_create`. That is not a
// niche path: redis:7-alpine's `bioInit()` starts three background threads
// unconditionally and `exit(1)`s if any of them fails, with no configuration
// that turns them off.
//
// The checks are chosen so that each one fails for a DIFFERENT defect, because
// "threads work" decomposes into several independent claims that a single
// happy-path smoke test would conflate:
//
//   - create/join at all. A join that never returns is the CLONE_CHILD_CLEARTID
//     path: the joiner parks on a futex over the dying thread's tid word, and
//     only the runtime ever zeroes it. Skip that and the guest hangs rather than
//     printing a wrong answer.
//
//   - one address space. A counter bumped by every thread must reach N. If the
//     scheduler snapshots and restores the arena on an intra-group switch --
//     which is exactly what it does for a process -- each thread gets a private
//     copy and the counter reads 1.
//
//   - one fd table, in BOTH directions. A worker writes through a descriptor
//     main opened, and main reads through a descriptor a worker opened. The
//     second direction is the one that catches a table that was copied at
//     create time instead of shared.
//
//   - separate thread pointers. Each worker stores its index in a `__thread`
//     variable, waits at a barrier so that all of them are in flight at once,
//     then reads it back. A runtime that leaves every thread on the initial
//     tpidr_el0 passes every other check here and fails only this one -- and in
//     a real guest it corrupts errno.
//
//     The barrier is load-bearing and was not the first thing tried. With a
//     `usleep` in its place the workers still ran one at a time, so each wrote
//     and read the same shared word between switches and the check passed even
//     with CLONE_SETTLS deliberately ignored. It only became a real observation
//     once every thread was forced to be mid-flight simultaneously -- which is
//     also the only configuration in which a shared thread pointer is
//     observably wrong.
//
//   - getpid vs gettid. Every thread shares one pid and has a distinct tid.
//     Getting this wrong makes `gettid() == getpid()` -- the standard "am I the
//     main thread" test -- true in every worker.
//
//   - exit(2) of the last thread. main returns normally after joining, so the
//     process's own exit still has to close descriptors and report status.
const threadsGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <stdio.h>
#include <string.h>
#include <sys/syscall.h>
#include <unistd.h>

#define NTHREADS 3

static int failures = 0;
#define CHECK(cond, what) do { if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } } while (0)

/* Shared across the group: if the arena is snapshotted per thread these stay
   at their initial values in main's copy. */
static volatile int counter = 0;
static int main_opened_fd = -1;
static int worker_opened_fd = -1;
static pthread_barrier_t all_running;

/* Per thread: if tpidr_el0 is not switched, every worker reads back the value
   the last one wrote. */
static __thread int my_index = -1;

struct slot {
	int idx;
	pid_t tid;
	pid_t pid;
	int wrote;
	int tls_ok;
};

static void *worker(void *p)
{
	struct slot *s = (struct slot *)p;
	my_index = s->idx;
	s->tid = (pid_t)syscall(SYS_gettid);
	s->pid = getpid();

	/* Write through a descriptor MAIN opened. */
	char line[32];
	int n = snprintf(line, sizeof line, "w%d\n", s->idx);
	s->wrote = (write(main_opened_fd, line, n) == n);

	counter++;

	/* Every worker must be in flight at once before any of them re-reads its
	   thread-local, or a shared thread pointer is indistinguishable from a
	   private one: run them one at a time and each writes and reads the same
	   word between switches. A sleep is NOT enough -- it was tried, and the
	   cooperative scheduler still ran each worker to completion. */
	pthread_barrier_wait(&all_running);

	s->tls_ok = (my_index == s->idx);
	return (void *)(long)(s->idx + 100);
}

int main(void)
{
	pid_t main_pid = getpid();
	pid_t main_tid = (pid_t)syscall(SYS_gettid);
	CHECK(main_pid == main_tid, "the initial thread's tid equals its pid");

	main_opened_fd = open("/tmp/threads_main.txt", O_RDWR | O_CREAT | O_TRUNC, 0644);
	CHECK(main_opened_fd >= 0, "main opens a file");

	CHECK(pthread_barrier_init(&all_running, NULL, NTHREADS) == 0, "barrier init");

	printf("check create-join\n");
	fflush(stdout);

	pthread_t th[NTHREADS];
	struct slot slots[NTHREADS];
	memset(slots, 0, sizeof slots);
	for (int i = 0; i < NTHREADS; i++) {
		slots[i].idx = i;
		int rc = pthread_create(&th[i], NULL, worker, &slots[i]);
		if (rc != 0) {
			printf("FAIL pthread_create %d (rc=%d %s)\n", i, rc, strerror(rc));
			failures++;
			return 1; /* nothing below is meaningful */
		}
	}
	for (int i = 0; i < NTHREADS; i++) {
		void *ret = NULL;
		int rc = pthread_join(th[i], &ret);
		CHECK(rc == 0, "pthread_join");
		CHECK((long)ret == i + 100, "the joined thread's return value");
	}

	printf("check shared-memory\n");
	fflush(stdout);
	CHECK(counter == NTHREADS, "every thread incremented the SAME counter");

	printf("check shared-fds\n");
	fflush(stdout);
	for (int i = 0; i < NTHREADS; i++) {
		CHECK(slots[i].wrote, "a worker wrote through main's descriptor");
	}
	/* Read back what the workers wrote, through main's own descriptor. */
	CHECK(lseek(main_opened_fd, 0, SEEK_SET) == 0, "rewind");
	char buf[256];
	memset(buf, 0, sizeof buf);
	ssize_t got = read(main_opened_fd, buf, sizeof buf - 1);
	CHECK(got > 0, "main reads the workers' bytes back");
	for (int i = 0; i < NTHREADS; i++) {
		char want[8];
		snprintf(want, sizeof want, "w%d\n", i);
		CHECK(strstr(buf, want) != NULL, "each worker's line is present");
	}

	printf("check tls-per-thread\n");
	fflush(stdout);
	for (int i = 0; i < NTHREADS; i++) {
		CHECK(slots[i].tls_ok, "a thread read back its OWN __thread value");
	}
	CHECK(my_index == -1, "the initial thread's __thread value was not clobbered");

	printf("check pid-and-tid\n");
	fflush(stdout);
	for (int i = 0; i < NTHREADS; i++) {
		CHECK(slots[i].pid == main_pid, "getpid() is the same in every thread");
		CHECK(slots[i].tid != main_tid, "gettid() differs from the main thread's");
		for (int j = i + 1; j < NTHREADS; j++) {
			CHECK(slots[i].tid != slots[j].tid, "each thread has a distinct tid");
		}
	}

	/* A descriptor opened by a thread must be usable by main after the join --
	   the direction that a copied-at-create fd table gets wrong. */
	printf("check fd-opened-by-thread\n");
	fflush(stdout);
	pthread_t opener;
	extern void *open_in_thread(void *);
	CHECK(pthread_create(&opener, NULL, open_in_thread, NULL) == 0, "create opener");
	CHECK(pthread_join(opener, NULL) == 0, "join opener");
	CHECK(worker_opened_fd >= 0, "the thread's open() succeeded");
	if (worker_opened_fd >= 0) {
		CHECK(write(worker_opened_fd, "main\n", 5) == 5,
		      "main writes through a descriptor a THREAD opened");
		close(worker_opened_fd);
	}
	close(main_opened_fd);
	pthread_barrier_destroy(&all_running);

	if (failures == 0) {
		printf("THREADS-OK\n");
	}
	return failures == 0 ? 0 : 1;
}

void *open_in_thread(void *unused)
{
	(void)unused;
	worker_opened_fd = open("/tmp/threads_worker.txt", O_RDWR | O_CREAT | O_TRUNC, 0644);
	return NULL;
}
`

// TestThreadsUnderEcvisor is the regression guard for CLONE_THREAD. See
// threadsGuestSrc for what each check isolates; a runtime that never switches
// between threads passes nothing past create-join, and one that treats a thread
// like a forked process passes create-join and fails shared-memory.
func TestThreadsUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "threads", threadsGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "threads")

	out := runWasm(t, ctx, wasm)
	assertThreadsGuestPassed(t, out)
}

// TestThreadsNativeBaseline runs the same guest on Linux, so the expectations
// above are pinned to kernel behaviour rather than to ecvisor's.
func TestThreadsNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "threads", threadsGuestSrc)

	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx, []string{"-v", abs + ":/w"}, "/w/threads")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertThreadsGuestPassed(t, out)
}

func assertThreadsGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "THREADS-OK") {
		t.Errorf("guest did not reach THREADS-OK; full output:\n%s", out)
	}
}
