package e2e

import (
	"strings"
	"testing"
)

// brkFailGuestSrc asks for a break far outside the arena's brk region and then
// carries on.
//
// The brk region is 96 MiB ([0x0A000000, 0x10000000)). A request past it used to
// be `fatal!`, which killed the module -- the fifth instance of aborting where
// the kernel returns an errno. It stopped initdb's post-bootstrap backend at
// `brk(0x157d0000)`, a request for roughly 190 MiB of heap.
//
// Linux does not fail brk() with an error code; it answers with the break it
// ENDED UP AT, and glibc's wrapper turns "not what I asked for" into ENOMEM.
// malloc's response is to switch to mmap, so the guest keeps running on a heap
// it allocates a different way. That whole recovery is what an abort removes.
//
// The assertions are therefore about SURVIVAL and CONTINUITY, not about the
// large allocation succeeding -- the arena genuinely cannot serve 190 MiB, and
// pretending otherwise would be the wrong fix:
//
//   - the oversized sbrk reports failure rather than killing the module;
//   - the break is unchanged afterwards, since a refused request must not move
//     it (a guest that believed a bogus break would write outside the arena);
//   - ordinary allocation still works on both sides of the refusal.
const brkFailGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

static int failures = 0;
#define CHECK(cond, what) do { \
	if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } \
} while (0)

/* Exercises malloc/free for real: an allocation the compiler cannot elide, and
   contents that are checked. */
static int heap_works(size_t n, unsigned char tag) {
	unsigned char *p = malloc(n);
	if (!p) {
		return 0;
	}
	memset(p, tag, n);
	int ok = p[0] == tag && p[n / 2] == tag && p[n - 1] == tag;
	free(p);
	return ok;
}

int main(void) {
	CHECK(heap_works(1 << 20, 0x5a), "malloc before the oversized brk");

	void *before = sbrk(0);
	CHECK(before != (void *) -1, "sbrk(0) reports the current break");
	printf("break starts at %p\n", before);

	/* Well past the 96 MiB brk region, and past the arena. This is the call
	   that used to end the module. */
	errno = 0;
	void *got = sbrk(300L * 1024 * 1024);
	if (got != (void *) -1) {
		/* Linux has the memory and grants it; the arena does not. Only the
		   refusal path has a stable break to check, so the assertion below is
		   guarded rather than unconditional -- otherwise the native baseline
		   fails a check that is simply inapplicable there. */
		printf("FAIL oversized sbrk was granted: %p\n", got);
		failures++;
	} else {
		printf("oversized sbrk refused, errno=%d\n", errno);
		void *after = sbrk(0);
		CHECK(after == before, "a refused sbrk leaves the break where it was");
	}

	/* The point of returning an errno instead of aborting: the guest is still
	   here, and its allocator still works. */
	CHECK(heap_works(1 << 20, 0xa5), "malloc after the refused brk");
	CHECK(heap_works(8 << 20, 0x3c), "larger malloc after the refused brk");

	if (failures == 0) printf("BRKFAIL-OK\n");
	return failures == 0 ? 0 : 1;
}
`

// TestOversizedBrkDoesNotKillTheModule guards brk refusal. See brkFailGuestSrc.
func TestOversizedBrkDoesNotKillTheModule(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "brkfail", brkFailGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "brkfail")

	assertBrkFailGuestPassed(t, runWasm(t, ctx, wasm))
}

// TestOversizedBrkNativeBaseline runs the same guest on Linux. It is what says
// the expectations describe brk, not ecvisor: a 300 MiB sbrk succeeds natively,
// so the baseline asserts only the parts that must hold either way.
func TestOversizedBrkNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "brkfail", brkFailGuestSrc)

	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/brkfail")
	// Natively the big sbrk is granted, so the guest exits 1 on the
	// "was granted" line. Everything else must still pass, which is what makes
	// the malloc and break-stability checks meaningful rather than ecvisor
	// describing itself.
	for _, line := range strings.Split(out, "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "FAIL ") && !strings.Contains(s, "oversized sbrk was granted") {
			t.Errorf("native run failed a check that should hold everywhere: %s", s)
		}
	}
	if !strings.Contains(out, "break starts at") {
		t.Errorf("native run did not get as far as sbrk(0): %v\n%s", err, out)
	}
}

func assertBrkFailGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "BRKFAIL-OK") {
		t.Errorf("guest did not reach BRKFAIL-OK; full output:\n%s", out)
	}
}
