package e2e

import (
	"strings"
	"testing"
)

// dupFdGuestSrc covers what a duplicated descriptor to an in-memory file must
// survive.
//
// The overlay keeps one shared buffer per open path in an `open_files` table and
// hands descriptors an INDEX into it, refcounted so a slot may be recycled once
// nothing references it. `dup_entry` bumped a pipe end's reference count and not
// a mem file's, so `dup` produced a second descriptor on the same index with no
// reference behind it. Closing either one dropped the count to zero, the slot was
// flushed and marked free while the survivor still pointed at it, and the next
// unrelated `open` took it over -- after which the survivor was reading a
// different file entirely, with no error anywhere.
//
// It is not a corner case. A file trace of one PostgreSQL boot recorded 5,265
// slot recycles, index 0 passing through /boot.sh, /etc/nsswitch.conf,
// /etc/passwd, PG_VERSION, postgresql.conf, /dev/shm/PostgreSQL.*, and
// /etc/ssl/openssl.cnf. The observable symptom was the autovacuum launcher
// reading a full 8 KiB page from a relation only one page long and reporting
// `invalid page in block 1 of relation global/1262`.
//
// The churn loop is the trigger and the sizes are chosen to make a retarget
// loud: the marker file is 4 KiB of 'A', every churn file is 16 KiB of 'B'. A
// descriptor that has been taken over reads 'B', or reads a length it cannot
// have. No PRNG is needed -- this data is not standing in for randomness, it is
// standing in for "recognisably not the other file".
const dupFdGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

static int failures = 0;
#define CHECK(cond, what) do { \
	if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } \
} while (0)

#define MARK 4096
#define CHURN 16384

int main(void) {
	umask(0);
	char dir[64], marker[128], other[128];
	snprintf(dir, sizeof dir, "/tmp/dupfd%d", (int)getpid());
	CHECK(mkdir(dir, 0755) == 0, "mkdir the test base");
	snprintf(marker, sizeof marker, "%s/marker", dir);

	char *a = malloc(MARK), *b = malloc(CHURN), *got = malloc(CHURN);
	memset(a, 'A', MARK);
	memset(b, 'B', CHURN);

	int fd = open(marker, O_RDWR | O_CREAT | O_TRUNC, 0644);
	CHECK(fd >= 0, "create the marker file");
	CHECK(write(fd, a, MARK) == MARK, "write the marker file");

	/* The dup is the whole point. After it, two descriptors name one buffer. */
	int dupd = dup(fd);
	CHECK(dupd >= 0, "dup the descriptor");

	/* Closing the ORIGINAL must not free the buffer: dupd still names it. */
	CHECK(close(fd) == 0, "close the original descriptor");

	/* Churn. Each of these is an open that may claim a slot that was wrongly
	   marked free -- and if it does, it takes dupd with it. */
	for (int i = 0; i < 24; i++) {
		snprintf(other, sizeof other, "%s/churn%d", dir, i);
		int c = open(other, O_RDWR | O_CREAT | O_TRUNC, 0644);
		CHECK(c >= 0, "create a churn file");
		CHECK(write(c, b, CHURN) == CHURN, "write a churn file");
		CHECK(close(c) == 0, "close a churn file");
	}

	/* Everything below asks one question: does dupd still name the marker? */
	struct stat st;
	CHECK(fstat(dupd, &st) == 0, "fstat through the dup");
	CHECK(st.st_size == MARK, "the dup still reports the marker's size");

	CHECK(lseek(dupd, 0, SEEK_SET) == 0, "rewind the dup");
	memset(got, 0, CHURN);
	ssize_t n = read(dupd, got, CHURN);
	CHECK(n == MARK, "the dup reads exactly the marker's length");
	CHECK(n > 0 && memcmp(got, a, n < MARK ? n : MARK) == 0,
	      "the dup reads the marker's CONTENT, not another file's");

	/* pread as well: it resolves the index by a different path in the runtime
	   than read does, and only one of the two was ever exercised here. */
	memset(got, 0, CHURN);
	CHECK(pread(dupd, got, MARK, 0) == MARK, "pread through the dup");
	CHECK(memcmp(got, a, MARK) == 0, "pread sees the marker's content");

	/* A write through the surviving descriptor must reach the file the NAME
	   refers to. If the buffer was flushed early and the slot recycled, this
	   lands somewhere else and the reopen below cannot see it. */
	CHECK(pwrite(dupd, "ZZZZ", 4, 0) == 4, "write through the dup");
	CHECK(close(dupd) == 0, "close the dup");

	int re = open(marker, O_RDONLY);
	CHECK(re >= 0, "reopen the marker by name");
	memset(got, 0, CHURN);
	n = read(re, got, CHURN);
	CHECK(n == MARK, "the reopened marker is still the marker's length");
	CHECK(n >= 4 && memcmp(got, "ZZZZ", 4) == 0,
	      "the write through the dup reached the file the name refers to");
	close(re);

	if (failures == 0) printf("DUPFD-OK\n");
	return failures == 0 ? 0 : 1;
}
`

// TestDupFdKeepsNamingItsFileUnderEcvisor is the regression guard for the
// `open_files` refcount. See dupFdGuestSrc.
func TestDupFdKeepsNamingItsFileUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "dupfd", dupFdGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "dupfd")
	assertDupFdGuestPassed(t, runWasm(t, ctx, wasm))
}

// TestDupFdNativeBaseline runs the same guest on Linux. Every assertion above is
// a plain POSIX property, and this is what says so rather than leaving them as
// claims about what this runtime happens to do.
func TestDupFdNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "dupfd", dupFdGuestSrc)

	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/dupfd")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertDupFdGuestPassed(t, out)
}

func assertDupFdGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "DUPFD-OK") {
		t.Errorf("guest did not reach DUPFD-OK; full output:\n%s", out)
	}
}

// scmFdGuestSrc is dupFdGuestSrc with a unix socket in the middle: the second
// descriptor arrives by SCM_RIGHTS instead of by `dup`.
//
// Same defect, second site. `parse_scm_rights` clones the sender's entries and
// says in its own comment that "the receiver gets an independent reference" --
// but it borrows the context immutably and cannot take one, and the install side
// (`recvmsg` -> `alloc_fd`) did not either. So a passed descriptor landed in the
// receiver with nothing holding its `open_files` slot, and the next unrelated
// `open` recycled the slot out from under it.
//
// The ordering is the trigger and is worth reading carefully: the sender closes
// its OWN copy while the right is still queued. At that moment the queue is the
// only thing that should be keeping the buffer alive, which is exactly the claim
// under test.
//
// nginx passes descriptors over its channel; PostgreSQL does not, which is why
// the postgres workload found the `dup` path first and left this one standing.
const scmFdGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <unistd.h>

static int failures = 0;
#define CHECK(cond, what) do { \
	if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } \
} while (0)

#define MARK 4096
#define CHURN 16384

static int send_fd(int sock, int fd) {
	struct msghdr msg;
	struct iovec io;
	union { struct cmsghdr align; char buf[CMSG_SPACE(sizeof(int))]; } u;
	memset(&msg, 0, sizeof msg);
	memset(&u, 0, sizeof u);
	io.iov_base = (void *)"x";
	io.iov_len = 1;
	msg.msg_iov = &io;
	msg.msg_iovlen = 1;
	msg.msg_control = u.buf;
	msg.msg_controllen = sizeof u.buf;
	struct cmsghdr *c = CMSG_FIRSTHDR(&msg);
	c->cmsg_level = SOL_SOCKET;
	c->cmsg_type = SCM_RIGHTS;
	c->cmsg_len = CMSG_LEN(sizeof(int));
	memcpy(CMSG_DATA(c), &fd, sizeof(int));
	return sendmsg(sock, &msg, 0) == 1 ? 0 : -1;
}

static int recv_fd(int sock) {
	struct msghdr msg;
	char b = 0;
	struct iovec io;
	union { struct cmsghdr align; char buf[CMSG_SPACE(sizeof(int))]; } u;
	memset(&msg, 0, sizeof msg);
	memset(&u, 0, sizeof u);
	io.iov_base = &b;
	io.iov_len = 1;
	msg.msg_iov = &io;
	msg.msg_iovlen = 1;
	msg.msg_control = u.buf;
	msg.msg_controllen = sizeof u.buf;
	if (recvmsg(sock, &msg, 0) != 1) return -1;
	struct cmsghdr *c = CMSG_FIRSTHDR(&msg);
	if (c == NULL || c->cmsg_level != SOL_SOCKET || c->cmsg_type != SCM_RIGHTS) return -1;
	int got = -1;
	memcpy(&got, CMSG_DATA(c), sizeof got);
	return got;
}

int main(void) {
	umask(0);
	char dir[64], marker[128], other[128];
	snprintf(dir, sizeof dir, "/tmp/scmfd%d", (int)getpid());
	CHECK(mkdir(dir, 0755) == 0, "mkdir the test base");
	snprintf(marker, sizeof marker, "%s/marker", dir);

	char *a = malloc(MARK), *b = malloc(CHURN), *got = malloc(CHURN);
	memset(a, 'A', MARK);
	memset(b, 'B', CHURN);

	int fd = open(marker, O_RDWR | O_CREAT | O_TRUNC, 0644);
	CHECK(fd >= 0, "create the marker file");
	CHECK(write(fd, a, MARK) == MARK, "write the marker file");

	int sv[2];
	CHECK(socketpair(AF_UNIX, SOCK_STREAM, 0, sv) == 0, "socketpair");
	CHECK(send_fd(sv[0], fd) == 0, "send the descriptor over the socket");

	/* The sender drops its copy while the right is still in flight. From here
	   until recvmsg claims it, the QUEUE is the only holder. */
	CHECK(close(fd) == 0, "close the sender's copy");

	int passed = recv_fd(sv[1]);
	CHECK(passed >= 0, "receive the descriptor");

	/* Rights that are sent and then ABANDONED -- both ends closed, nobody ever
	   calls recvmsg. A direction with no reader and no writer can never deliver
	   what is queued on it, so the runtime drops those batches and returns their
	   references. The leak this prevents is invisible from here (a pinned slot is
	   just memory), but the failure mode of getting the drop WRONG is not: an
	   over-release drives the marker's count to zero while the received fd is
	   still
	   open, and the churn below then recycles the slot out from under it. That is
	   the bug this whole file exists for, so the assertions after the churn are
	   what make this loop mean something. */
	for (int i = 0; i < 8; i++) {
		int ab[2];
		CHECK(socketpair(AF_UNIX, SOCK_STREAM, 0, ab) == 0, "socketpair to abandon");
		CHECK(send_fd(ab[0], passed) == 0, "send a right that is never received");
		CHECK(close(ab[0]) == 0, "close the abandoned sender");
		CHECK(close(ab[1]) == 0, "close the abandoned receiver");
	}

	for (int i = 0; i < 24; i++) {
		snprintf(other, sizeof other, "%s/churn%d", dir, i);
		int c = open(other, O_RDWR | O_CREAT | O_TRUNC, 0644);
		CHECK(c >= 0, "create a churn file");
		CHECK(write(c, b, CHURN) == CHURN, "write a churn file");
		CHECK(close(c) == 0, "close a churn file");
	}

	if (passed >= 0) {
		struct stat st;
		CHECK(fstat(passed, &st) == 0, "fstat the passed descriptor");
		CHECK(st.st_size == MARK, "the passed descriptor reports the marker's size");

		memset(got, 0, CHURN);
		CHECK(pread(passed, got, MARK, 0) == MARK, "pread the passed descriptor");
		CHECK(memcmp(got, a, MARK) == 0,
		      "the passed descriptor reads the marker's CONTENT, not another file's");
		close(passed);
	}
	close(sv[0]);
	close(sv[1]);

	if (failures == 0) printf("SCMFD-OK\n");
	return failures == 0 ? 0 : 1;
}
`

// TestPassedFdKeepsNamingItsFileUnderEcvisor is the regression guard for the
// SCM_RIGHTS half of the `open_files` refcount. See scmFdGuestSrc.
func TestPassedFdKeepsNamingItsFileUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "scmfd", scmFdGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "scmfd")
	assertScmFdGuestPassed(t, runWasm(t, ctx, wasm))
}

// TestPassedFdNativeBaseline runs the same guest on Linux, where passing a
// descriptor and closing the sender's copy is ordinary POSIX.
func TestPassedFdNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "scmfd", scmFdGuestSrc)

	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/scmfd")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertScmFdGuestPassed(t, out)
}

func assertScmFdGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "SCMFD-OK") {
		t.Errorf("guest did not reach SCMFD-OK; full output:\n%s", out)
	}
}
