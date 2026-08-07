package e2e

import (
	"strings"
	"testing"
)

// fdShareGuestSrc checks that a file offset belongs to the open file
// DESCRIPTION, not to the descriptor.
//
// Linux gives `dup`, `dup2` and `fork` a shared `struct file`, so they share the
// read/write position; a second `open` of the same path gets its own. ecvisor
// kept the position in the descriptor, so every one of those shared it with
// nobody -- `dup2(fd, 7)` then reading from both re-read the same bytes, and a
// shell's `read` in a forked child left the parent's offset where it was.
//
// EVERY check here must hold on Linux too, which is what makes the native
// baseline worth running: there is no case where ecvisor is entitled to a
// different answer, so a failure in the baseline means the expectation is wrong
// rather than the implementation.
const fdShareGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/uio.h>
#include <sys/wait.h>
#include <unistd.h>

static int failures = 0;
#define CHECK(cond, what) do { \
	if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } \
} while (0)
#define CHECK_EQ(got, want, what) do { \
	long g = (long)(got), w = (long)(want); \
	if (g != w) { printf("FAIL %s: got %ld want %ld\n", what, g, w); failures++; } \
} while (0)

#define PATH "/tmp/fdshare.dat"
#define N 64

/* Byte i is i, so a read's CONTENT says which offset it came from -- a length
   alone cannot distinguish "read from 0" from "read from 4". */
static void make_file(void) {
	unsigned char buf[N];
	for (int i = 0; i < N; i++) buf[i] = (unsigned char) i;
	int fd = open(PATH, O_RDWR | O_CREAT | O_TRUNC, 0644);
	CHECK(fd >= 0, "creating the fixture");
	CHECK(write(fd, buf, N) == N, "writing the fixture");
	CHECK(close(fd) == 0, "closing the fixture");
}

static int first_byte(int fd, int n, const char *what) {
	unsigned char buf[N];
	ssize_t got = read(fd, buf, n);
	if (got != n) { printf("FAIL %s: short read %zd\n", what, got); failures++; return -1; }
	return buf[0];
}

int main(void) {
	make_file();

	/* --- dup2 shares the position ----------------------------------- */
	int a = open(PATH, O_RDONLY);
	CHECK(a >= 0, "open for dup2");
	CHECK_EQ(first_byte(a, 4, "first read on the opener"), 0, "opener reads from 0");
	CHECK(dup2(a, 7) == 7, "dup2 onto fd 7");
	/* The whole point: fd 7 continues where fd a left off. With a per-fd
	   position this read starts at 0 again. */
	CHECK_EQ(first_byte(7, 4, "read on the dup2"), 4, "the dup2 continues from 4");
	CHECK_EQ(lseek(a, 0, SEEK_CUR), 8, "the opener sees the dup2's advance");
	CHECK_EQ(lseek(7, 0, SEEK_CUR), 8, "both descriptors report one position");

	/* A seek through EITHER descriptor moves the shared position. */
	CHECK_EQ(lseek(7, 16, SEEK_SET), 16, "seek on the dup2");
	CHECK_EQ(lseek(a, 0, SEEK_CUR), 16, "the opener sees the dup2's seek");
	CHECK_EQ(first_byte(a, 4, "read after the shared seek"), 16, "reads from 16");

	/* --- a second open does NOT share -------------------------------- */
	int b = open(PATH, O_RDONLY);
	CHECK(b >= 0, "second open");
	CHECK_EQ(lseek(b, 0, SEEK_CUR), 0, "a fresh open starts at 0");
	CHECK_EQ(first_byte(b, 4, "read on the second open"), 0, "the second open reads from 0");
	CHECK_EQ(lseek(a, 0, SEEK_CUR), 20, "the second open did not move the first");

	/* --- closing one of a shared pair leaves the other intact -------- */
	/* If the description's reference count were wrong, its slot would be free
	   here and the next open could recycle it under the survivor. */
	CHECK(close(7) == 0, "closing the dup2");
	CHECK_EQ(lseek(a, 0, SEEK_CUR), 20, "the survivor keeps its position");
	int c = open(PATH, O_RDONLY);
	CHECK(c >= 0, "open after the close, to force slot reuse");
	CHECK_EQ(lseek(c, 32, SEEK_SET), 32, "the recycled slot is the new open's");
	CHECK_EQ(lseek(a, 0, SEEK_CUR), 20, "and it did not land on the survivor");
	CHECK(close(c) == 0, "closing the recycler");
	CHECK(close(b) == 0, "closing the second open");

	/* --- a positional read does not disturb the shared position ------ */
	unsigned char pbuf[8];
	CHECK(pread(a, pbuf, 8, 40) == 8, "pread");
	CHECK_EQ(pbuf[0], 40, "pread reads from where it was told");
	CHECK_EQ(lseek(a, 0, SEEK_CUR), 20, "pread left the position alone");

	/* preadv is a DIFFERENT path in the runtime: it seeks, runs the ordinary
	   iovec loop, and seeks back. That save/restore now goes through the shared
	   description, so getting it wrong moves the position of every descriptor
	   that shares it -- which a dup makes observable. */
	int d = dup(a);
	CHECK(d >= 0, "dup for the preadv check");
	struct iovec iov = { pbuf, 8 };
	CHECK(preadv(a, &iov, 1, 48) == 8, "preadv");
	CHECK_EQ(pbuf[0], 48, "preadv reads from where it was told");
	CHECK_EQ(lseek(a, 0, SEEK_CUR), 20, "preadv left the position alone");
	CHECK_EQ(lseek(d, 0, SEEK_CUR), 20, "and left the dup's view of it alone");
	CHECK(close(d) == 0, "closing the preadv dup");

	/* --- fork shares the position ------------------------------------ */
	CHECK_EQ(lseek(a, 0, SEEK_SET), 0, "rewinding before the fork");
	fflush(stdout);
	pid_t pid = fork();
	CHECK(pid >= 0, "fork");
	if (pid == 0) {
		/* The child reads 8 bytes. On Linux the parent's position moves too,
		   because both descriptors point at one description. */
		unsigned char buf[N];
		_exit(read(a, buf, 8) == 8 && buf[0] == 0 ? 0 : 1);
	}
	int status = 0;
	CHECK(waitpid(pid, &status, 0) == pid, "waitpid");
	CHECK(WIFEXITED(status) && WEXITSTATUS(status) == 0, "the child read its 8 bytes");
	CHECK_EQ(lseek(a, 0, SEEK_CUR), 8, "the parent sees the child's advance");
	CHECK_EQ(first_byte(a, 4, "parent read after the fork"), 8, "the parent continues from 8");

	CHECK(close(a) == 0, "closing the opener");

	/* --- the file itself is unharmed --------------------------------- */
	int v = open(PATH, O_RDONLY);
	CHECK(v >= 0, "reopening to verify the contents");
	unsigned char buf[N];
	CHECK(read(v, buf, N) == N, "reading the whole file back");
	int intact = 1;
	for (int i = 0; i < N; i++) if (buf[i] != (unsigned char) i) intact = 0;
	CHECK(intact, "the file's bytes are unchanged");
	CHECK(close(v) == 0, "closing the verifier");

	if (failures == 0) printf("FDSHARE-OK\n");
	return failures == 0 ? 0 : 1;
}
`

// TestFileOffsetIsSharedAcrossDupAndFork guards README item 4 under ecvisor.
func TestFileOffsetIsSharedAcrossDupAndFork(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "fdshare", fdShareGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "fdshare")

	assertFdShareGuestPassed(t, runWasm(t, ctx, wasm))
}

// TestFileOffsetSharingNativeBaseline runs the same guest on Linux. Unlike the
// mmap refusals, nothing here is allowed to differ: this is the definition of
// the behaviour, so the baseline asserts exactly what the ecvisor run does.
func TestFileOffsetSharingNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "fdshare", fdShareGuestSrc)

	out, _ := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/fdshare")
	assertFdShareGuestPassed(t, out)
}

func assertFdShareGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "FDSHARE-OK") {
		t.Errorf("guest did not reach FDSHARE-OK; full output:\n%s", out)
	}
}
