package e2e

import (
	"strings"
	"testing"
)

// fsyncGuestSrc covers the durability syscalls.
//
// ENOSYS here is not a harmless gap. It is the LAST thing initdb does, and its
// response to a failed flush is to delete the cluster it just finished
// building:
//
//	performing post-bootstrap initialization ... ok
//	syncing data to disk ... initdb: error: could not fsync file
//	  ".../PG_VERSION": Function not implemented
//	initdb: removing contents of data directory "/var/lib/postgresql/data"
//
// Twenty minutes of work discarded on the final step, because a database that
// cannot promise durability is right to refuse.
//
// Succeeding is honest rather than a shortcut: the guest filesystem is the
// in-memory rfs plus its tmpfs overlay, so there is no storage beneath it and
// nothing a flush could make more durable. What the guest actually depends on
// -- that its writes are visible to every later read -- is unconditionally
// true, and that is what this asserts rather than asserting a bare `== 0`.
//
// The DIRECTORY fsync is the case that failed in practice, and it is easy to
// miss: a handler that validates the fd as a regular file would pass every
// check here except that one.
const fsyncGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

static int failures = 0;
#define CHECK(cond, what) do { \
	if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } \
} while (0)

int main(void) {
	int fd = open("/tmp/durable", O_RDWR | O_CREAT | O_TRUNC, 0644);
	CHECK(fd >= 0, "open");
	CHECK(write(fd, "durable", 7) == 7, "write");

	errno = 0; CHECK(fsync(fd) == 0, "fsync a written file");
	errno = 0; CHECK(fdatasync(fd) == 0, "fdatasync");
	errno = 0; CHECK(sync_file_range(fd, 0, 7, 0) == 0, "sync_file_range");

	/* The promise fsync stands in for. Asserting only "returned 0" would pass
	   for a handler that succeeded and lost the data. */
	char buf[8] = {0};
	CHECK(pread(fd, buf, 7, 0) == 7 && memcmp(buf, "durable", 7) == 0,
	      "content is readable after the sync");
	close(fd);

	/* initdb fsyncs DIRECTORIES too -- the call that actually failed. */
	int dfd = open("/tmp", O_RDONLY | O_DIRECTORY);
	CHECK(dfd >= 0, "open a directory");
	errno = 0; CHECK(fsync(dfd) == 0, "fsync a directory");
	close(dfd);

	/* NO bare sync() here. It takes no fd and cannot fail, so it would add
	   nothing -- and the native baseline runs it on the HOST, where sync()
	   flushes every mounted filesystem. On a build machine that has just
	   written hundreds of GB of lift artifacts that blocks for tens of
	   minutes; it timed this test out at 2700s once. syncfs() is avoided for
	   the same reason. The ecvisor side treats all five identically, so the
	   three exercised above cover the handler. */

	if (failures == 0) printf("FSYNC-OK\n");
	return failures == 0 ? 0 : 1;
}
`

// TestDurabilitySyscallsUnderEcvisor guards fsync and friends. See
// fsyncGuestSrc for why ENOSYS here costs a whole initdb run.
func TestDurabilitySyscallsUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "fsyncg", fsyncGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "fsyncg")
	assertFsyncGuestPassed(t, runWasm(t, ctx, wasm))
}

// TestDurabilitySyscallsNativeBaseline runs the same guest on Linux, where all
// of these genuinely flush -- so the expectations describe the syscalls rather
// than ecvisor's implementation of them.
func TestDurabilitySyscallsNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "fsyncg", fsyncGuestSrc)

	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/fsyncg")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertFsyncGuestPassed(t, out)
}

func assertFsyncGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "FSYNC-OK") {
		t.Errorf("guest did not reach FSYNC-OK; full output:\n%s", out)
	}
}
