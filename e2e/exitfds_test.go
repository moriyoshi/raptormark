package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// exitFdsGuestSrc checks that a process exiting releases its descriptors.
//
// Linux closes every fd at exit, and that is what gives a pipe reader EOF once
// its last writer dies. ecvisor marked the process a zombie, woke any wait4 and
// posted SIGCHLD, but never touched the fd table, so the pipe's writer count
// never reached zero.
//
// The failure is not subtle once it happens, but it is very hard to attribute:
// initdb's popen() child exited 127, the parent stayed in read() forever, and
// the module died with
//
//	[ecvisor] deadlock: every process is blocked and nothing can wake them: [(1, PipeRead(0))]
//
// naming a writer that was already a zombie. Pre-fix this guest hangs until the
// scheduler declares that deadlock and exits 111, so a regression is loud.
//
// The parent must close its OWN write end first; holding it would block forever
// on Linux too, and the test would then prove nothing.
const exitFdsGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <stdio.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

static int failures = 0;
#define CHECK(cond, what) do { if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } } while (0)

int main(void) {
	/* 1. A child that exits WITHOUT closing the write end must still give the
	      reader EOF, because exiting closes it. */
	int fd[2];
	CHECK(pipe(fd) == 0, "pipe");
	pid_t p = fork();
	CHECK(p >= 0, "fork");
	if (p == 0) {
		/* Deliberately leave fd[1] open. Exit must close it. */
		_exit(0);
	}
	CHECK(close(fd[1]) == 0, "parent closes its own write end");

	printf("check eof-after-child-exit\n");
	fflush(stdout);
	char buf[16];
	ssize_t n = read(fd[0], buf, sizeof buf);
	CHECK(n == 0, "read should report EOF once the last writer exits");
	close(fd[0]);

	int st = 0;
	CHECK(waitpid(p, &st, 0) == p, "waitpid");
	CHECK(WIFEXITED(st) && WEXITSTATUS(st) == 0, "child exit status");

	/* 2. Data written before exiting must still arrive: releasing the fd must
	      not discard buffered bytes. */
	printf("check data-then-exit\n");
	fflush(stdout);
	CHECK(pipe(fd) == 0, "pipe 2");
	p = fork();
	CHECK(p >= 0, "fork 2");
	if (p == 0) {
		ssize_t w = write(fd[1], "hello", 5);
		_exit(w == 5 ? 0 : 1);
	}
	CHECK(close(fd[1]) == 0, "parent closes write end 2");
	memset(buf, 0, sizeof buf);
	n = read(fd[0], buf, sizeof buf);
	CHECK(n == 5 && memcmp(buf, "hello", 5) == 0, "buffered data survives the writer exiting");
	n = read(fd[0], buf, sizeof buf);
	CHECK(n == 0, "EOF after the buffered data");
	close(fd[0]);
	CHECK(waitpid(p, &st, 0) == p, "waitpid 2");

	if (failures == 0) {
		printf("EXITFDS-OK\n");
	}
	return failures == 0 ? 0 : 1;
}
`

// TestExitClosesFdsUnderEcvisor guards the exit path against leaking a pipe
// writer. See exitFdsGuestSrc for why the failure is a module-wide deadlock
// rather than a wrong value.
func TestExitClosesFdsUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "exitfds", exitFdsGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "exitfds")

	out := runWasm(t, ctx, wasm)
	assertExitFdsGuestPassed(t, out)
}

// TestExitClosesFdsNativeBaseline runs the same guest on Linux, pinning the
// expectations to kernel behaviour rather than to ecvisor's.
func TestExitClosesFdsNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "exitfds", exitFdsGuestSrc)

	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx, []string{"-v", abs + ":/w"}, "/w/exitfds")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertExitFdsGuestPassed(t, out)
}

func assertExitFdsGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "EXITFDS-OK") {
		t.Errorf("guest did not reach EXITFDS-OK; full output:\n%s", out)
	}
}
