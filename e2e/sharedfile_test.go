package e2e

import (
	"strings"
	"testing"
)

// sharedFileGuestSrc has two processes write to ONE file and checks that
// neither loses the other's data.
//
// An open file used to be a private per-process copy of the contents, flushed
// back to the tmpfs on close, so the last close won. It was unreachable until
// bounded arena snapshots allowed two backends to run at once, and then
// PostgreSQL hit it immediately:
//
//	ERROR:  unexpected data beyond EOF in block 0 of relation base/5/16384
//
// -- one backend had extended a relation while the other held a shorter copy.
//
// The parent and child append distinct records at distinct offsets and
// ping-pong so each write is separated by a context switch. Both records must
// survive, and the file's LENGTH must reflect both -- the postgres symptom was
// a length/content disagreement, not garbled bytes.
const sharedFileGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

#define PATH "/tmp/shared-file"
#define ROUNDS 4

static int failures = 0;
#define CHECK(cond, what) do { \
	if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } \
} while (0)

int main(void) {
	int up[2], down[2];
	CHECK(pipe(up) == 0 && pipe(down) == 0, "pipe");
	unlink(PATH);
	int seed = open(PATH, O_RDWR | O_CREAT | O_TRUNC, 0644);
	CHECK(seed >= 0, "create");
	close(seed);

	pid_t kid = fork();
	CHECK(kid >= 0, "fork");

	/* Both hold the file open for the whole exchange, which is the case that
	   broke: two descriptors, two processes, one path. */
	int fd = open(PATH, O_RDWR);
	if (fd < 0) { if (kid == 0) { _exit(60); } CHECK(0, "open"); return 1; }

	for (int r = 0; r < ROUNDS; r++) {
		/* Distinct, non-overlapping slots so a lost write is unambiguous. */
		off_t slot = (off_t)((r * 2 + (kid == 0 ? 0 : 1)) * 8);
		char rec[8];
		memset(rec, kid == 0 ? 'C' : 'P', 8);
		if (pwrite(fd, rec, 8, slot) != 8) { if (kid == 0) { _exit(61); } CHECK(0, "pwrite"); }

		char tok = 'x';
		if (kid == 0) {
			if (write(down[1], &tok, 1) != 1) { _exit(62); }
			if (read(up[0], &tok, 1) != 1) { _exit(63); }
		} else {
			if (read(down[0], &tok, 1) != 1) { CHECK(0, "parent read"); break; }
			if (write(up[1], &tok, 1) != 1) { CHECK(0, "parent write"); break; }
		}
	}
	close(fd);
	if (kid == 0) { _exit(0); }

	int st = 0;
	CHECK(waitpid(kid, &st, 0) == kid, "waitpid");
	CHECK(WIFEXITED(st) && WEXITSTATUS(st) == 0, "child completed");
	if (WIFEXITED(st) && WEXITSTATUS(st) != 0) { printf("child exit %d\n", WEXITSTATUS(st)); }

	/* Re-open and check EVERY slot. A per-fd copy loses one process's writes
	   entirely; a short file loses the tail. */
	int v = open(PATH, O_RDONLY);
	CHECK(v >= 0, "reopen");
	char all[ROUNDS * 2 * 8];
	memset(all, 0, sizeof all);
	ssize_t got = read(v, all, sizeof all);
	CHECK(got == (ssize_t)sizeof all, "the file is as long as both processes made it");
	if (got == (ssize_t)sizeof all) {
		for (int r = 0; r < ROUNDS; r++) {
			for (int who = 0; who < 2; who++) {
				char want = who == 0 ? 'C' : 'P';
				const char *p = all + (r * 2 + who) * 8;
				int ok = 1;
				for (int i = 0; i < 8; i++) { if (p[i] != want) { ok = 0; } }
				CHECK(ok, who == 0 ? "the child's records survived" : "the parent's records survived");
			}
		}
	}
	close(v);
	if (failures == 0) { printf("SHAREDFILE-OK\n"); }
	return failures == 0 ? 0 : 1;
}
`

func TestSharedFileWritesNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "sharedfileg", sharedFileGuestSrc)
	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/sharedfileg")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertSharedFilePassed(t, out)
}

func TestSharedFileWritesUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	elf := compileGuest(t, ctx, dir, "sharedfileg", sharedFileGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "sharedfileg")
	assertSharedFilePassed(t, runWasm(t, ctx, wasm))
}

func assertSharedFilePassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "SHAREDFILE-OK") {
		t.Errorf("guest did not reach SHAREDFILE-OK; full output:\n%s", out)
	}
}
