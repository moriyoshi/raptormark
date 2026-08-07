package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// threadgapsGuestSrc is the BOUNDED half of the threadgaps probe: signal
// delivery and thread TLS allocation. Its siblings cover threads on their own
// (threads_test.go: create/join, one address space, one fd table, distinct
// thread pointers, getpid vs gettid) and threads meeting the process model
// (threadproc_test.go: fork from a thread). This file is the third thing:
// what a threaded guest does while it is WAITING, and what its TLS has to be
// for it to have started at all.
//
// Three phases of the full probe are deliberately NOT here, because this
// harness cannot express them and pretending otherwise would produce a test
// that fails for the harness's reasons rather than the runtime's:
//
//   - exec-from-a-non-leader-thread needs the multi-program module of
//     crossprog_test.go; liftOne builds one program with no exec map.
//   - a thread-local read through a shared object (the only thing that
//     actually executes a TLSDESC descriptor) needs a dynamic build and a
//     fused plugin, not a static guest.
//   - pause, whose failure mode is an unbounded hang rather than a report.
//
// They live in .agents-workspace/drivers/threadgaps/, which runs all five
// natively. Everything below is bounded: every wait has a timeout, so a
// runtime that never delivers a signal FAILS with a diagnostic instead of
// consuming ctxFor's 45 minutes and localising nothing.
const threadgapsGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/syscall.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

/* How long a poster waits before signalling, against waits of 300 ms. This is a
   knob because it is the NEUTRALIZATION lever: rebuild with
   -DPOSTER_DELAY_MS=400 and every signal arrives after its wait has already
   expired, which must make the EINTR and elapsed-time checks fail while the
   "a handler ran" flags still pass. Measured: 16 failures at 400, and the
   condvar phase failed ONLY "a handler ran" -- so the timing checks observe
   what they claim. */
#ifndef POSTER_DELAY_MS
#define POSTER_DELAY_MS 50
#endif

static int failures = 0;

#define CHECK(cond, what)                                                      \
	do {                                                                   \
		if (!(cond)) {                                                 \
			printf("FAIL %s (errno=%d %s)\n", (what), errno,       \
			       strerror(errno));                               \
			failures++;                                            \
		}                                                              \
	} while (0)

static pid_t self_tid(void)
{
	return (pid_t)syscall(SYS_gettid);
}

static long now_ms(void)
{
	struct timespec ts;
	clock_gettime(CLOCK_MONOTONIC, &ts);
	return ts.tv_sec * 1000L + ts.tv_nsec / 1000000L;
}

/* Sleeps the whole interval even if a handler interrupts it. Used only where
   the sleep is scaffolding (a poster's delay), never where it is the thing
   under test. */
static void msleep(long ms)
{
	struct timespec ts = { ms / 1000, (ms % 1000) * 1000000L };
	while (nanosleep(&ts, &ts) == -1 && errno == EINTR)
		;
}

/* ------------------------------------------------------------------ signals */

/* What the handler saw. tid is the discriminator that separates "a signal was
   delivered" from "it was delivered to the right task" -- a runtime that posts
   to the group leader unconditionally passes every count check here. */
struct sigrec {
	volatile sig_atomic_t count;
	volatile pid_t tid;
};
static struct sigrec usr1, usr2;

static void on_sig(int s)
{
	struct sigrec *r = (s == SIGUSR1) ? &usr1 : &usr2;
	r->tid = self_tid();
	r->count++;
}

static void reset_sigrecs(void)
{
	usr1.count = usr2.count = 0;
	usr1.tid = usr2.tid = 0;
}

/* sa_flags is 0 ON PURPOSE. With SA_RESTART the kernel restarts an interrupted
   nanosleep, the wait runs its full timeout anyway, and the "was the wait cut
   short" check below could no longer tell a correct runtime from one that
   delivers only at the wait's own expiry -- which is the most likely shape of a
   partial fix. */
static void install_handlers(void)
{
	struct sigaction sa;
	memset(&sa, 0, sizeof sa);
	sa.sa_handler = on_sig;
	sigemptyset(&sa.sa_mask);
	sa.sa_flags = 0;
	CHECK(sigaction(SIGUSR1, &sa, NULL) == 0, "install SIGUSR1 handler");
	CHECK(sigaction(SIGUSR2, &sa, NULL) == 0, "install SIGUSR2 handler");
}

struct poster {
	int sig;
	long delay_ms;
	pthread_t target;   /* used when to_thread */
	int to_thread;      /* pthread_kill vs kill(getpid()) */
	int block_own;      /* block sig in the poster itself */
	/* Set by the phase to call the whole thing off before it fires. Only the
	   sigsuspend backstop uses it -- see phase_sigsuspend_pending. */
	volatile int cancel;
};

/* Posts one signal after a delay. The poster's own sleep parks in the same
   futex path the phases under test use, which is fine: what is under test is
   handler DELIVERY, not sleeping. */
static void *poster_fn(void *p)
{
	struct poster *q = (struct poster *)p;
	if (q->block_own) {
		/* A group-directed signal must land on a thread that has it
		   unblocked. Blocking it here is what makes the target's tid a
		   real observation rather than a coincidence of scheduling. */
		sigset_t s;
		sigemptyset(&s);
		sigaddset(&s, q->sig);
		pthread_sigmask(SIG_BLOCK, &s, NULL);
	}
	msleep(q->delay_ms);
	if (q->cancel)
		return NULL;
	if (q->to_thread)
		pthread_kill(q->target, q->sig);
	else
		kill(getpid(), q->sig);
	return NULL;
}

static pthread_t spawn_poster(struct poster *q, const char *what)
{
	pthread_t th = 0;
	int rc = pthread_create(&th, NULL, poster_fn, q);
	if (rc != 0) {
		printf("FAIL create poster for %s (rc=%d %s)\n", what, rc,
		       strerror(rc));
		failures++;
	}
	return th;
}

/* PHASE signals.0 -- THE SECOND CONTROL, and it was added because the first
   ecvisor run passed a check it should have failed. Every "the wait was cut
   short" assertion below infers delivery from ELAPSED TIME, and that inference
   is only valid if an uninterrupted sleep sleeps. Measured 2026-08-14: ecvisor
   has no nanosleep (101) and no clock_nanosleep (115) in its dispatch, so a
   static glibc guest's nanosleep returns ENOSYS immediately -- and every
   elapsed-time check silently passed, for a reason that had nothing to do with
   signals. This control turns that into a named failure instead.

   (The runtime comment at sys.rs:3604 says glibc routes clock_nanosleep through
   FUTEX_WAIT_BITSET. That holds for the condvar and semaphore paths; the plain
   nanosleep/clock_nanosleep libc call issues the syscall directly and lands in
   the ENOSYS default.) */
static void phase_sleep_works(void)
{
	printf("check nanosleep-actually-sleeps (control)\n");
	fflush(stdout);

	struct timespec ts = { 0, 100 * 1000000L };
	long t0 = now_ms();
	int rc = nanosleep(&ts, NULL);
	long elapsed = now_ms() - t0;

	CHECK(rc == 0, "an uninterrupted nanosleep succeeded");
	CHECK(elapsed >= 90, "nanosleep actually consumed its interval");
	if (rc != 0 || elapsed < 90)
		printf("  (nanosleep returned %d after %ld ms -- every elapsed-time "
		       "check below is now meaningless)\n", rc, elapsed);
}

/* PHASE signals.0b -- a sleep interrupted by a signal from a CHILD PROCESS.
   Single-threaded on both sides, and that is the entire point: every other
   delivery check here involves a thread, and ecvisor keeps the blocked-signal
   MASK per thread group rather than per thread, so glibc's own pthread_create
   (which blocks every signal but SIGSETXID while the new thread starts) leaves
   the whole process with signals blocked. Measured: pending=0x200 against
   blocked=0xfffffffeffffffff. That defect masks this one, so without a
   thread-free case there is no way to tell "delivery from a sleep is broken"
   from "the mask was wrong before the sleep began".

   fork gives a signal source that never calls pthread_create. */
static void phase_sleep_interrupt_by_child(pid_t main_tid)
{
	printf("check signal-during-nanosleep-from-a-child\n");
	fflush(stdout);
	reset_sigrecs();

	pid_t kid = fork();
	if (kid == 0) {
		msleep(POSTER_DELAY_MS);
		kill(getppid(), SIGUSR1);
		_exit(0);
	}
	CHECK(kid > 0, "fork a signalling child");
	if (kid < 0)
		return;

	struct timespec ts = { 0, 300 * 1000000L }, rem = { 0, 0 };
	long t0 = now_ms();
	int rc = nanosleep(&ts, &rem);
	long elapsed = now_ms() - t0;

	CHECK(usr1.count == 1, "a child's signal ran a handler during nanosleep");
	CHECK(rc == -1 && errno == EINTR, "nanosleep reported EINTR");
	CHECK(elapsed < 250, "nanosleep was cut short rather than running out");
	CHECK(usr1.tid == main_tid, "the handler ran on the sleeping thread");
	/* The REMAINDER is what glibc's sleep() loops on: too large and an
	   interrupted sleep sleeps longer in total than was asked for. */
	long left_ms = rem.tv_sec * 1000L + rem.tv_nsec / 1000000L;
	CHECK(rem.tv_nsec < 1000000000L, "the remainder's nsec field is normalised");
	CHECK(left_ms > 0 && left_ms < 300,
	      "the remainder is what was actually left of the interval");
	if (elapsed >= 250 || left_ms <= 0 || left_ms >= 300)
		printf("  (slept %ld ms, remainder %ld ms)\n", elapsed, left_ms);

	int st = 0;
	CHECK(waitpid(kid, &st, 0) == kid, "reap the signalling child");
}

/* PHASE signals.1 -- a handler must run while the thread is parked in a plain
   sleep. This is the commonest shape of the gap: a watchdog signals a worker
   that is between two polls.

   Four independent claims, four separate checks:
     count == 1        delivery happened at all
     rc/EINTR          the wait reported the interruption to the guest
     elapsed < 250     it happened DURING the sleep, not at its expiry
     tid == main       it ran on the signalled thread */
static void phase_sleep_interrupt(pid_t main_tid)
{
	printf("check signal-during-nanosleep\n");
	fflush(stdout);
	reset_sigrecs();

	struct poster q = { SIGUSR1, POSTER_DELAY_MS, pthread_self(), 1, 0, 0 };
	pthread_t th = spawn_poster(&q, "nanosleep");

	struct timespec ts = { 0, 300 * 1000000L }, rem = { 0, 0 };
	long t0 = now_ms();
	int rc = nanosleep(&ts, &rem);
	long elapsed = now_ms() - t0;

	CHECK(usr1.count == 1, "a handler ran during nanosleep");
	CHECK(rc == -1 && errno == EINTR, "nanosleep reported EINTR");
	CHECK(elapsed < 250, "nanosleep was cut short rather than running out");
	CHECK(usr1.tid == main_tid, "the handler ran on the signalled thread");
	if (elapsed >= 250)
		printf("  (nanosleep ran %ld ms of its 300 ms budget)\n", elapsed);
	if (th)
		pthread_join(th, NULL);
}

/* PHASE signals.2 -- a synchronously ACCEPTED signal. sigtimedwait is how a
   signal-driven program avoids handlers entirely, and it is a different runtime
   path from handler delivery.

   The last check is the one a naive implementation fails: accepting a signal
   must NOT also run its handler. */
static void phase_sigtimedwait(void)
{
	printf("check sigtimedwait\n");
	fflush(stdout);
	reset_sigrecs();

	sigset_t set, old;
	sigemptyset(&set);
	sigaddset(&set, SIGUSR2);
	pthread_sigmask(SIG_BLOCK, &set, &old);

	struct poster q = { SIGUSR2, POSTER_DELAY_MS, pthread_self(), 1, 0, 0 };
	pthread_t th = spawn_poster(&q, "sigtimedwait");

	struct timespec to = { 0, 300 * 1000000L };
	siginfo_t info;
	memset(&info, 0, sizeof info);
	long t0 = now_ms();
	int rc = sigtimedwait(&set, &info, &to);
	long elapsed = now_ms() - t0;

	CHECK(rc == SIGUSR2, "sigtimedwait returned the signal it waited for");
	CHECK(info.si_signo == SIGUSR2, "siginfo names the signal");
	CHECK(elapsed < 250, "sigtimedwait returned before its timeout");
	CHECK(usr2.count == 0, "an ACCEPTED signal did not also run its handler");
	if (th)
		pthread_join(th, NULL);
	pthread_sigmask(SIG_SETMASK, &old, NULL);
}

/* PHASE signals.3 -- THE POSITIVE CONTROL, and the reason the rest of this
   guest can be trusted. rt_sigsuspend is the one wait ecvisor already delivers
   from, and a signal made pending BEFORE the wait begins is the easiest case.
   If this fails, the harness is wrong and no failure below means what it says.

   The SIGUSR2 backstop keeps the phase bounded: if SIGUSR1 delivery were
   missing, sigsuspend would otherwise never return and this guest would hang
   exactly where it is supposed to report. */
static void phase_sigsuspend_pending(pid_t main_tid)
{
	printf("check sigsuspend-with-pending (control)\n");
	fflush(stdout);
	reset_sigrecs();

	sigset_t block, old, empty;
	sigemptyset(&block);
	sigaddset(&block, SIGUSR1);
	pthread_sigmask(SIG_BLOCK, &block, &old);

	CHECK(raise(SIGUSR1) == 0, "raise a blocked signal");
	CHECK(usr1.count == 0, "a blocked signal stays pending");

	struct poster q = { SIGUSR2, 500, pthread_self(), 1, 0, 0 };
	pthread_t th = spawn_poster(&q, "sigsuspend backstop");

	sigemptyset(&empty);
	long t0 = now_ms();
	int rc = sigsuspend(&empty);
	long elapsed = now_ms() - t0;

	CHECK(rc == -1 && errno == EINTR, "sigsuspend returned EINTR");
	CHECK(usr1.count == 1, "the pending signal was delivered on unblock");
	CHECK(usr1.tid == main_tid, "it ran on the thread that raised it");
	CHECK(elapsed < 400, "it did not need the backstop signal to return");
	/* CANCEL the backstop rather than letting it fire into the next phase.
	   It fires 500 ms in, which is longer than any single phase below, so a
	   backstop that posts anyway arrives while some LATER phase is waiting --
	   and a delivered handler ends a sleep with EINTR no matter which signal
	   it was. Measured under ecvisor: the leaked SIGUSR2 interrupted the very
	   next nanosleep, and the phase reported "no handler ran" against an EINTR
	   it had not caused. Natively this never showed, because delivery at the
	   join consumed it inside this phase. */
	q.cancel = 1;
	if (th)
		pthread_join(th, NULL);
	pthread_sigmask(SIG_SETMASK, &old, NULL);
}

/* PHASE signals.4 -- a timed CONDVAR wait, which is where a real threaded
   server sits. Two claims that pull in opposite directions, which is what makes
   the pair worth writing:
     - the handler must run DURING the wait;
     - and yet the wait must NOT return early, because POSIX says a condvar wait
       never reports EINTR. A runtime that "fixes" delivery by aborting the
       futex wait breaks the second one, and every existing test stays green. */
static void phase_cond_timedwait(void)
{
	printf("check signal-during-cond_timedwait\n");
	fflush(stdout);
	reset_sigrecs();

	pthread_mutex_t m = PTHREAD_MUTEX_INITIALIZER;
	pthread_cond_t c;
	pthread_condattr_t ca;
	pthread_condattr_init(&ca);
	pthread_condattr_setclock(&ca, CLOCK_MONOTONIC);
	pthread_cond_init(&c, &ca);

	struct poster q = { SIGUSR1, POSTER_DELAY_MS, pthread_self(), 1, 0, 0 };
	pthread_t th = spawn_poster(&q, "cond_timedwait");

	struct timespec deadline;
	clock_gettime(CLOCK_MONOTONIC, &deadline);
	deadline.tv_nsec += 300 * 1000000L;
	if (deadline.tv_nsec >= 1000000000L) {
		deadline.tv_nsec -= 1000000000L;
		deadline.tv_sec++;
	}

	long t0 = now_ms();
	pthread_mutex_lock(&m);
	int rc = pthread_cond_timedwait(&c, &m, &deadline);
	pthread_mutex_unlock(&m);
	long elapsed = now_ms() - t0;

	CHECK(rc == ETIMEDOUT, "a condvar wait is not interrupted by a signal");
	CHECK(usr1.count == 1, "a handler ran during a condvar wait");
	CHECK(elapsed >= 250, "the condvar wait still ran its full timeout");
	if (th)
		pthread_join(th, NULL);
	pthread_cond_destroy(&c);
	pthread_condattr_destroy(&ca);
}

/* PHASE signals.5 -- a GROUP-directed signal. kill(getpid()) from a worker that
   has the signal blocked must be handled by the main thread. This is a
   different runtime path from pthread_kill: posting resolves the group's single
   shared signal table and then has to CHOOSE an eligible task. A runtime that
   posts to the calling task loses the signal; one that posts to the leader
   unconditionally passes here and fails phase 6. */
static void phase_group_directed(pid_t main_tid)
{
	printf("check group-directed-kill\n");
	fflush(stdout);
	reset_sigrecs();

	struct poster q = { SIGUSR1, POSTER_DELAY_MS, 0, 0, 1, 0 };
	pthread_t th = spawn_poster(&q, "group-directed");

	struct timespec ts = { 0, 300 * 1000000L };
	long t0 = now_ms();
	int rc = nanosleep(&ts, &ts);
	long elapsed = now_ms() - t0;

	CHECK(usr1.count == 1, "a group-directed signal was delivered");
	CHECK(usr1.tid == main_tid,
	      "it chose the thread that does NOT have it blocked");
	CHECK(rc == -1 && errno == EINTR, "it interrupted the leader's sleep");
	CHECK(elapsed < 250, "the leader's sleep was cut short");
	if (th)
		pthread_join(th, NULL);
}

/* PHASE signals.6 -- delivery to a NON-LEADER that is parked. Everything above
   signals the main thread; a runtime whose delivery is wired to "the current
   process" rather than to a task passes all of it and fails only this.

   The sleeper reports its own EINTR through the slot, because the main thread
   cannot observe another thread's syscall return. */
struct sleeper {
	pid_t tid;
	int rc;
	int err;
	long elapsed;
	pthread_barrier_t *ready;
};

static void *sleeper_fn(void *p)
{
	struct sleeper *s = (struct sleeper *)p;
	s->tid = self_tid();
	/* A new thread INHERITS the creator's mask, and the creator blocked
	   SIGUSR1 so that a mis-targeted delivery has nowhere to land. Unblock
	   it here or the signal stays pending on this thread forever -- which
	   is what the native baseline reported the first time this ran, and it
	   was the test that was wrong, not the kernel. */
	sigset_t un;
	sigemptyset(&un);
	sigaddset(&un, SIGUSR1);
	pthread_sigmask(SIG_UNBLOCK, &un, NULL);
	pthread_barrier_wait(s->ready);
	struct timespec ts = { 0, 300 * 1000000L };
	long t0 = now_ms();
	s->rc = nanosleep(&ts, &ts);
	s->err = errno;
	s->elapsed = now_ms() - t0;
	return NULL;
}

static void phase_thread_directed(void)
{
	printf("check thread-directed-kill\n");
	fflush(stdout);
	reset_sigrecs();

	/* Blocked in MAIN, so a delivery that ignores the target lands nowhere
	   and usr1.count stays 0 -- rather than quietly landing here. */
	sigset_t block, old;
	sigemptyset(&block);
	sigaddset(&block, SIGUSR1);
	pthread_sigmask(SIG_BLOCK, &block, &old);

	pthread_barrier_t ready;
	pthread_barrier_init(&ready, NULL, 2);
	struct sleeper s;
	memset(&s, 0, sizeof s);
	s.ready = &ready;

	pthread_t th;
	int rc = pthread_create(&th, NULL, sleeper_fn, &s);
	CHECK(rc == 0, "create the sleeping worker");
	if (rc == 0) {
		pthread_barrier_wait(&ready);
		msleep(POSTER_DELAY_MS);
		CHECK(pthread_kill(th, SIGUSR1) == 0, "signal a worker thread");
		pthread_join(th, NULL);

		CHECK(usr1.count == 1, "the worker's handler ran");
		CHECK(usr1.tid == s.tid, "it ran on the WORKER, not the leader");
		CHECK(s.rc == -1 && s.err == EINTR,
		      "the worker's own nanosleep reported EINTR");
		CHECK(s.elapsed < 250, "the worker's sleep was cut short");
	}
	pthread_barrier_destroy(&ready);
	pthread_sigmask(SIG_SETMASK, &old, NULL);
}

/* ---------------------------------------------------------------------- tls */

/* On musl this whole phase is reached only if libc.tls_size / tls_align /
   tls_head are populated: __copy_tls sizes a new thread's block from them, and
   with them zero it computes a pointer relative to NULL and the first
   pthread_create never returns. On glibc it passes today, which is the point --
   the guest is the same either way, so the DIFFERENCE between an Alpine and a
   Debian run is the measurement.

   The 64-byte alignment is load-bearing, not decoration: tls_align is a
   separate field from tls_size, and a layout that satisfies the size but not
   the alignment gives a block that works for a scalar and is misaligned for
   anything a vector instruction touches. A __thread int alone cannot see it. */
static __thread int tls_scalar = -1;
static __thread char tls_aligned[128] __attribute__((aligned(64)));
static pthread_key_t tsd_key;
static volatile int dtors_run = 0;

struct tlsslot {
	int idx;
	pid_t tid;
	void *scalar_addr;
	void *aligned_addr;
	int scalar_ok;
	int aligned_ok;
	int align_ok;
	int errno_ok;
	int tsd_ok;
	pthread_barrier_t *all;
};

static void tsd_dtor(void *v)
{
	(void)v;
	__atomic_fetch_add(&dtors_run, 1, __ATOMIC_SEQ_CST);
}

static void *tls_worker(void *p)
{
	struct tlsslot *s = (struct tlsslot *)p;
	s->tid = self_tid();
	tls_scalar = s->idx;
	memset(tls_aligned, s->idx, sizeof tls_aligned);
	s->scalar_addr = (void *)&tls_scalar;
	s->aligned_addr = (void *)tls_aligned;
	s->align_ok = (((unsigned long)tls_aligned & 63UL) == 0);

	pthread_setspecific(tsd_key, (void *)(long)(s->idx + 1));

	/* errno is thread-local, and it is the field a shared thread pointer
	   corrupts first. Each worker provokes a DIFFERENT errno and they must
	   all be in flight at once -- run them one at a time and each writes
	   and reads the same word between switches, which is exactly how a
	   shared thread pointer passes. */
	if (s->idx == 0) {
		close(-1);                                       /* EBADF */
	} else if (s->idx == 1) {
		(void)open("/nonexistent/threadgaps", O_RDONLY); /* ENOENT */
	} else {
		(void)!read(-1, NULL, 0);                        /* EBADF */
	}
	int want = (s->idx == 1) ? ENOENT : EBADF;

	pthread_barrier_wait(s->all);

	s->errno_ok = (errno == want);
	s->scalar_ok = (tls_scalar == s->idx);
	s->aligned_ok = ((unsigned char)tls_aligned[0] == (unsigned char)s->idx &&
			 (unsigned char)tls_aligned[127] == (unsigned char)s->idx);
	s->tsd_ok = ((long)pthread_getspecific(tsd_key) == s->idx + 1);
	return NULL;
}

#define NTLS 3

static void phase_tls(void)
{
	printf("check thread-tls-allocation\n");
	fflush(stdout);

	CHECK(pthread_key_create(&tsd_key, tsd_dtor) == 0, "create a TSD key");

	pthread_barrier_t all;
	pthread_barrier_init(&all, NULL, NTLS);

	pthread_t th[NTLS];
	struct tlsslot slots[NTLS];
	memset(slots, 0, sizeof slots);

	int created = 0;
	for (int i = 0; i < NTLS; i++) {
		slots[i].idx = i;
		slots[i].all = &all;
		int rc = pthread_create(&th[i], NULL, tls_worker, &slots[i]);
		if (rc != 0) {
			/* This is the musl failure. Report it as itself: a
			   thread whose TLS block could not be allocated. */
			printf("FAIL pthread_create %d (rc=%d %s) -- "
			       "on musl this is libc.tls_size/tls_align\n",
			       i, rc, strerror(rc));
			failures++;
			break;
		}
		created++;
	}
	if (created != NTLS) {
		/* Nothing below is meaningful, and the barrier would deadlock
		   waiting for threads that do not exist. */
		pthread_barrier_destroy(&all);
		return;
	}
	for (int i = 0; i < NTLS; i++)
		CHECK(pthread_join(th[i], NULL) == 0, "join a TLS worker");

	for (int i = 0; i < NTLS; i++) {
		CHECK(slots[i].scalar_ok, "a thread read back its own __thread scalar");
		CHECK(slots[i].aligned_ok, "a thread read back its own aligned block");
		CHECK(slots[i].align_ok, "the aligned __thread block honours alignof(64)");
		CHECK(slots[i].errno_ok, "each thread kept its OWN errno concurrently");
		CHECK(slots[i].tsd_ok, "pthread_getspecific returned this thread's value");
		for (int j = i + 1; j < NTLS; j++) {
			CHECK(slots[i].scalar_addr != slots[j].scalar_addr,
			      "two threads have DIFFERENT TLS blocks");
			CHECK(slots[i].aligned_addr != slots[j].aligned_addr,
			      "two threads have different aligned blocks");
		}
	}
	CHECK(tls_scalar == -1, "the initial thread's __thread value survived");
	CHECK(dtors_run == NTLS, "a TSD destructor ran for every exited thread");
	pthread_barrier_destroy(&all);
	pthread_key_delete(tsd_key);
}

/* --------------------------------------------------------------------- main */

static void phase_signals(pid_t main_tid)
{
	install_handlers();
	phase_sleep_works();                  /* the controls run FIRST */
	phase_sigsuspend_pending(main_tid);
	phase_sleep_interrupt_by_child(main_tid);
	phase_sleep_interrupt(main_tid);
	phase_sigtimedwait();
	phase_cond_timedwait();
	phase_group_directed(main_tid);
	phase_thread_directed();
}

int main(int argc, char **argv)
{
	const char *what = (argc > 1) ? argv[1] : "default";
	pid_t main_tid = self_tid();

	setvbuf(stdout, NULL, _IOLBF, 0);

	if (strcmp(what, "signals") == 0) {
		phase_signals(main_tid);
	} else if (strcmp(what, "tls") == 0) {
		phase_tls();
	} else if (strcmp(what, "default") == 0) {
		phase_signals(main_tid);
		phase_tls();
	} else {
		printf("FAIL unknown phase %s\n", what);
		return 2;
	}

	if (failures == 0)
		printf("THREADGAPS-OK\n");
	else
		printf("THREADGAPS-FAILURES %d\n", failures);
	return failures == 0 ? 0 : 1;
}
`

// TestThreadGapsUnderEcvisor lifts the probe once and runs each phase as its
// own subtest. One module, two runs: the phases are selected by guest argv, so
// the ~minutes of lifting are not paid twice.
//
// Both subtests are REGRESSION GUARDS and both pass. `signals` was a gap probe
// behind RAPTORMARK_E2E_GAPS=1 while the defects it names were open; the gate
// came off on 2026-08-15 when the last of them closed, which is the whole
// lifecycle a probe is supposed to have. It went 14 failures -> 11 -> 3 -> 0
// across four fixes: the missing sleep syscalls, a same-group wake that never
// fired, the per-thread signal mask (with its selection half), and finally
// rt_sigtimedwait plus a delivery boundary on the futex path.
//
// A failure here now is a new defect, not a known one.
func TestThreadGapsUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "threadgaps", threadgapsGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "threadgaps")

	t.Run("tls", func(t *testing.T) {
		assertThreadGapsPassed(t, runThreadGapsPhase(t, ctx, wasm, "tls"))
	})

	t.Run("signals", func(t *testing.T) {
		assertThreadGapsPassed(t, runThreadGapsPhase(t, ctx, wasm, "signals"))
	})
}

// TestThreadGapsNativeBaseline runs the same guest on Linux. Every expectation
// in the source is pinned to kernel behaviour rather than to ecvisor's, and
// this is what pins it: both phases print THREADGAPS-OK natively on glibc and
// on musl, so a failure under ecvisor is ecvisor's.
func TestThreadGapsNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "threadgaps", threadgapsGuestSrc)

	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"tls", "signals"} {
		t.Run(phase, func(t *testing.T) {
			out, err := dockerRun(ctx, []string{"-v", abs + ":/w"}, "/w/threadgaps "+phase)
			if err != nil {
				t.Errorf("native run failed: %v", err)
			}
			assertThreadGapsPassed(t, out)
		})
	}
}

// runThreadGapsPhase runs one phase and returns its output WITHOUT failing on a
// non-zero exit. The guest exits 1 when a check fails, and its own FAIL lines
// say which one; letting runWasmIn t.Fatalf on the exit status would replace
// six named failures with one "exit status 1".
func runThreadGapsPhase(t *testing.T, ctx context.Context, wasmPath string, args ...string) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Dir(wasmPath))
	if err != nil {
		t.Fatal(err)
	}
	cmd := "wasmedge --enable-all"
	// wasmedge does not inherit the host environment, so a diagnostic variable
	// set on the `go test` command line reaches the guest only if it is forwarded
	// explicitly -- and it must come BEFORE the module path, where wasmedge stops
	// parsing its own flags. Without this, turning on the runtime's tracing for a
	// failing phase means editing this file.
	for _, v := range []string{"RAPTORMARK_ECV_DEBUG", "RAPTORMARK_ECV_TRACE"} {
		if s := os.Getenv(v); s != "" {
			cmd += " --env " + v + "=" + s
		}
	}
	cmd += " /out/" + filepath.Base(wasmPath)
	for _, a := range args {
		cmd += " " + a
	}
	out, _ := dockerRun(ctx, []string{"-v", dir + ":/out"}, cmd)
	return out
}

func assertThreadGapsPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "THREADGAPS-OK") {
		t.Errorf("guest did not reach THREADGAPS-OK; full output:\n%s", out)
	}
}
