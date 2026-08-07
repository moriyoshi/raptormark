package e2e

import (
	"strings"
	"testing"
)

// divergeGuestSrc makes two processes hold DIFFERENT contents at the same
// addresses, then forces context switches between them and checks that neither
// sees the other's bytes.
//
// This exists because the obvious test does not work. Running any forking guest
// under bounded snapshots looks like a check and is not one: fork leaves parent
// and child with IDENTICAL memory, so a range set that saves nothing at all
// still passes as long as neither process diverges. Omitting the whole heap
// from the range set was tried against the UDS guest and it passed.
//
// So every region is deliberately made to diverge AFTER the fork, and the
// ping-pong forces a switch between each write and its check:
//
//   - the HEAP (brk/malloc), the region a bounded snapshot must save
//   - a private mmap, saved as a live extent
//   - a deep STACK frame, saved from sp upward
//   - a writable global, in the image's writable PT_LOAD
//
// Each process writes its own pattern, hands the token to the peer, waits to
// get it back -- which guarantees the peer ran and the scheduler switched
// arenas at least twice -- and only then re-reads. A missing or short range
// shows up as the peer's pattern in the reader's own memory, which is the exact
// silent corruption the scheme risks.
const divergeGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/wait.h>
#include <unistd.h>

#define N 64
#define ROUNDS 6

static int failures = 0;
#define CHECK(cond, what) do { \
	if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } \
} while (0)

/* A writable global: lives in a writable PT_LOAD, which the range set covers
   via the program headers rather than via brk or mmap. */
static char global_buf[N];

static void fill(char *p, int n, char tag) { for (int i = 0; i < n; i++) { p[i] = (char)(tag + (i & 15)); } }
static int same(const char *p, int n, char tag) {
	for (int i = 0; i < n; i++) { if (p[i] != (char)(tag + (i & 15))) { return 0; } }
	return 1;
}

int main(void) {
	int up[2], down[2];
	CHECK(pipe(up) == 0 && pipe(down) == 0, "pipe");

	char *heap = malloc(N);
	CHECK(heap != NULL, "malloc");
	char *map = mmap(NULL, 4096, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
	CHECK(map != MAP_FAILED, "mmap");

	pid_t kid = fork();
	CHECK(kid >= 0, "fork");
	char tag = (kid == 0) ? 'A' : 'a';   /* distinct patterns per process */
	int rfd = (kid == 0) ? up[0] : down[0];
	int wfd = (kid == 0) ? down[1] : up[1];

	char deep[N];
	for (int r = 0; r < ROUNDS; r++) {
		fill(heap, N, tag);
		fill(map, N, tag);
		fill(deep, N, tag);
		fill(global_buf, N, tag);

		/* Hand the token over and wait for it back: the peer must have run,
		   so this process was saved and restored at least once in between. */
		char tok = 'x';
		if (kid == 0) {
			if (write(wfd, &tok, 1) != 1) { _exit(31); }
			if (read(rfd, &tok, 1) != 1) { _exit(32); }
		} else {
			if (read(rfd, &tok, 1) != 1) { CHECK(0, "parent read"); break; }
			if (write(wfd, &tok, 1) != 1) { CHECK(0, "parent write"); break; }
		}

		if (kid == 0) {
			if (!same(heap, N, tag)) { _exit(41); }
			if (!same(map, N, tag)) { _exit(42); }
			if (!same(deep, N, tag)) { _exit(43); }
			if (!same(global_buf, N, tag)) { _exit(44); }
		} else {
			CHECK(same(heap, N, tag), "heap survived the switch");
			CHECK(same(map, N, tag), "private mmap survived the switch");
			CHECK(same(deep, N, tag), "stack survived the switch");
			CHECK(same(global_buf, N, tag), "writable global survived the switch");
		}
	}

	if (kid == 0) { _exit(0); }
	int st = 0;
	CHECK(waitpid(kid, &st, 0) == kid, "waitpid");
	CHECK(WIFEXITED(st) && WEXITSTATUS(st) == 0, "the child saw its own memory throughout");
	if (WIFEXITED(st) && WEXITSTATUS(st) != 0) {
		printf("child exit code %d (4x = which region diverged)\n", WEXITSTATUS(st));
	}
	if (failures == 0) { printf("DIVERGE-OK\n"); }
	return failures == 0 ? 0 : 1;
}
`

func TestDivergentMemorySurvivesSwitchesNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "divergeg", divergeGuestSrc)
	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/divergeg")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertDivergePassed(t, out)
}

// Bounded snapshots, where each region is saved explicitly and a missing range
// means one process reads another's bytes. That is the only scheme there is, so
// this needs no environment to select it.
//
// It absorbed a twin. TestDivergentMemorySurvivesSwitchesUnderBoundedSnapshots
// existed alongside this one from the day bounded snapshots became the default,
// with this half pinned to the full-buffer scheme by an environment variable so
// that the two schemes stayed distinct and this one could serve as the other's
// oracle. That variable was REMOVED on 2026-08-22 and the full-buffer path with
// it, so the pair would now run the identical scheme twice -- two green results
// that read as a two-scheme comparison and are one measurement. The oracle is
// gone; duplicating the test does not bring it back, it only hides that it is
// gone. See `Arena::bytes_differing_outside`, which now returns `None` rather
// than a fabricated zero for the same reason.
func TestDivergentMemorySurvivesSwitchesUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	elf := compileGuest(t, ctx, dir, "divergeg", divergeGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "divergeg")
	out := runWasmIn(t, ctx, wasm, nil, nil, "")
	assertDivergePassed(t, out)
}

func assertDivergePassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "DIVERGE-OK") {
		t.Errorf("guest did not reach DIVERGE-OK; full output:\n%s", out)
	}
}
