package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// forkLeakGuestSrc forks and reaps a child, over and over, doing nothing else.
//
// Each process owns a MEMORY_ARENA_SIZE (384 MiB) arena. A process that exits
// keeps nothing on Linux, but ecvisor's context switch files the outgoing
// buffer under `live_owner` -- and after an exit that IS the zombie, so every
// dead process kept a full arena forever. wasm linear memory only grows, so the
// module walked up 384 MiB per fork until an allocation failed:
//
//	[mem] fork -> 5 procs, linear memory 2257 MiB
//	[mem] exit pid=5 -> linear memory 2271 MiB     <- nothing returned
//	[mem] fork -> 9 procs, linear memory 3793 MiB
//	memory allocation of 402653184 bytes failed
//
// It surfaced on initdb, which forks repeatedly to probe max_connections and
// shared_buffers, but nothing about it is postgres-specific: any guest that
// spawns more than a handful of short-lived children hits it. Found by printing
// `memory.size` at fork and exit rather than by reasoning about the arena, which
// had wrongly suggested a capacity limit ("four processes at 384 MiB should fit
// in 4 GiB") when the real fault was a leak.
//
// ROUNDS is chosen well past the observed failure point (9 processes). The
// failure mode is an abort, not a wrong answer, so a regression is loud: the
// guest stops printing and the module exits 134.
const forkLeakGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <stdio.h>
#include <sys/wait.h>
#include <unistd.h>

#define ROUNDS 24

static int failures = 0;
#define CHECK(cond, what) do { if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } } while (0)

int main(void) {
	for (int i = 0; i < ROUNDS; i++) {
		pid_t p = fork();
		CHECK(p >= 0, "fork");
		if (p == 0) {
			_exit(7);
		}
		int st = 0;
		CHECK(waitpid(p, &st, 0) == p, "waitpid");
		CHECK(WIFEXITED(st) && WEXITSTATUS(st) == 7, "child exit status");
		/* Progress on its own line: if a regression aborts the module, the last
		   number printed says how many arenas it took to run out. */
		printf("round %d ok\n", i);
		fflush(stdout);
		if (failures) {
			break;
		}
	}
	if (failures == 0) {
		printf("FORKLEAK-OK\n");
	}
	return failures == 0 ? 0 : 1;
}
`

// TestForkDoesNotLeakArenasUnderEcvisor is the regression guard for a zombie
// retaining its arena. See forkLeakGuestSrc for the measurement behind it.
func TestForkDoesNotLeakArenasUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "forkleak", forkLeakGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "forkleak")

	out := runWasm(t, ctx, wasm)
	assertForkLeakGuestPassed(t, out)
}

// TestForkDoesNotLeakArenasNativeBaseline runs the same guest on Linux, so a
// failure under ecvisor cannot be blamed on the guest.
func TestForkDoesNotLeakArenasNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "forkleak", forkLeakGuestSrc)

	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/forkleak")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertForkLeakGuestPassed(t, out)
}

// mustAbs is the absolute path of dir, for a docker -v mount.
func mustAbs(t *testing.T, dir string) string {
	t.Helper()
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func assertForkLeakGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "FORKLEAK-OK") {
		// Name the last round reached: it is the arena count the module died on.
		last := "none"
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "round ") {
				last = strings.TrimSpace(line)
			}
		}
		t.Errorf("guest did not reach FORKLEAK-OK (last progress: %s); full output:\n%s", last, out)
	}
}
