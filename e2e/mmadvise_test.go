package e2e

import (
	"strings"
	"testing"
)

// mmAdviseGuestSrc exercises the four syscalls that used to return ENOSYS.
//
// Every check here must hold on Linux too, which is what makes the native
// baseline load-bearing: these are kernel behaviours being reproduced, not
// ecvisor conventions being asserted.
//
// The decisive one is MADV_DONTNEED. It is the single piece of `madvise` that is
// not advice: on an anonymous private mapping Linux guarantees the range reads
// as ZEROES afterwards, and allocators use exactly that to release a large block
// without unmapping it. A runtime that answers 0 without zeroing hands the guest
// its own stale bytes where it is entitled to zeroes -- which no status code
// reveals, and which the guest will read as data.
const mmAdviseGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/syscall.h>
#include <sys/sysinfo.h>
#include <unistd.h>

static int failures = 0;
#define CHECK(cond, what) do { \
	if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } \
} while (0)

#define LEN (256 * 1024)

int main(void) {
	/* --- madvise(MADV_DONTNEED) must leave zeroes ---------------------- */
	unsigned char *p = mmap(NULL, LEN, PROT_READ | PROT_WRITE,
	                        MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
	CHECK(p != MAP_FAILED, "mmap for the madvise check");
	if (p != MAP_FAILED) {
		memset(p, 0xa5, LEN);
		CHECK(p[0] == 0xa5 && p[LEN - 1] == 0xa5, "the region really was dirtied");
		errno = 0;
		int rc = madvise(p, LEN, MADV_DONTNEED);
		printf("madvise(MADV_DONTNEED) rc=%d errno=%d\n", rc, rc == 0 ? 0 : errno);
		CHECK(rc == 0, "MADV_DONTNEED succeeds");
		if (rc == 0) {
			int zeroed = 1;
			for (int i = 0; i < LEN; i++) if (p[i] != 0) { zeroed = 0; break; }
			CHECK(zeroed, "MADV_DONTNEED leaves the range reading as zeroes");
		}
		/* An ordinary hint may be ignored, but must not fail. */
		CHECK(madvise(p, LEN, MADV_WILLNEED) == 0, "MADV_WILLNEED succeeds");
		munmap(p, LEN);
	}

	/* --- mremap: shrink keeps the address, grow moves and preserves ---- */
	unsigned char *q = mmap(NULL, LEN, PROT_READ | PROT_WRITE,
	                        MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
	CHECK(q != MAP_FAILED, "mmap for the mremap check");
	if (q != MAP_FAILED) {
		memset(q, 0x3c, LEN);
		void *shrunk = mremap(q, LEN, LEN / 2, 0);
		printf("mremap shrink rc=%s\n", shrunk == MAP_FAILED ? "fail" : "ok");
		CHECK(shrunk != MAP_FAILED, "mremap can shrink");
		if (shrunk != MAP_FAILED) {
			unsigned char *s = shrunk;
			CHECK(s[0] == 0x3c && s[LEN / 2 - 1] == 0x3c, "a shrink preserves the head");
			/* Growing needs MREMAP_MAYMOVE; the contents must survive the move. */
			void *grown = mremap(shrunk, LEN / 2, LEN * 2, MREMAP_MAYMOVE);
			printf("mremap grow rc=%s\n", grown == MAP_FAILED ? "fail" : "ok");
			CHECK(grown != MAP_FAILED, "mremap can grow with MAYMOVE");
			if (grown != MAP_FAILED) {
				unsigned char *g = grown;
				CHECK(g[0] == 0x3c && g[LEN / 2 - 1] == 0x3c,
				      "a grow preserves the old contents");
				/* Write the whole new extent: if the runtime returned an address
				   it had not actually reserved, this is where it shows. */
				memset(g, 0x5a, LEN * 2);
				CHECK(g[LEN * 2 - 1] == 0x5a, "the grown region is fully usable");
				munmap(grown, LEN * 2);
			}
		}
	}

	/* --- sysinfo reports a plausible total ----------------------------- */
	struct sysinfo si;
	memset(&si, 0, sizeof si);
	errno = 0;
	int rc = sysinfo(&si);
	printf("sysinfo rc=%d totalram=%lu mem_unit=%u\n",
	       rc, (unsigned long) si.totalram, si.mem_unit);
	CHECK(rc == 0, "sysinfo succeeds");
	CHECK(si.totalram > 0, "sysinfo reports some memory");
	CHECK(si.mem_unit > 0, "sysinfo reports a usable mem_unit");

	/* --- membarrier: querying is always safe --------------------------- */
	errno = 0;
	long mb = syscall(SYS_membarrier, 0 /* MEMBARRIER_CMD_QUERY */, 0, 0);
	printf("membarrier query rc=%ld errno=%d\n", mb, mb < 0 ? errno : 0);
	CHECK(mb >= 0, "membarrier query does not fail");

	if (failures == 0) printf("MMADVISE-OK\n");
	return failures == 0 ? 0 : 1;
}
`

// TestMemorySyscallsMatchLinux is the ecvisor half.
func TestMemorySyscallsMatchLinux(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "mmadvise", mmAdviseGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "mmadvise")
	assertMMAdvisePassed(t, runWasm(t, ctx, wasm))
}

// TestMemorySyscallsNativeBaseline runs the same guest on Linux. Nothing here is
// allowed to differ: every assertion is a kernel behaviour, so a failure means
// the expectation is wrong rather than the implementation.
func TestMemorySyscallsNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "mmadvise", mmAdviseGuestSrc)

	out, _ := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/mmadvise")
	assertMMAdvisePassed(t, out)
}

func assertMMAdvisePassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "MMADVISE-OK") {
		t.Errorf("guest did not reach MMADVISE-OK; full output:\n%s", out)
	}
}
