package e2e

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// mmapFailGuestSrc drives every `mmap` argument `NR_MMAP` used to answer wrong.
//
// Cases 1-6 are the `fatal!` audit. Four sites in `NR_MMAP` aborted the module
// for arguments Linux answers with an errno -- or, in one case, does not fail at
// all. An abort is not just a lost error message: under ecvisor it takes down
// EVERY process in the module, so a postmaster and its backends die for one
// backend's refused mapping, and the probing that guests do around memory
// (PostgreSQL walks shared_buffers downward and tries its DSM implementations in
// order) is exactly the behaviour an abort removes.
//
// Cases 7-8 are the opposite failure and were left out of that audit for exactly
// that reason: a length `do_mmap` refuses, which ecvisor SUCCEEDED at. Nothing
// aborts and nothing is printed; the guest is handed an address for a mapping of
// nothing. A test is the only way to see it.
//
// Five of the eight cases below expect the SAME answer natively and under
// ecvisor, which is what makes the native baseline load-bearing rather than
// decorative -- it certifies the expectation instead of ecvisor describing
// itself. The other three cannot: Linux serves a file-backed MAP_FIXED and we
// cannot, and the two out-of-range fixed addresses fail on both sides for
// different reasons (Linux: the address is above TASK_SIZE; ecvisor: it is
// outside the 384 MiB arena). Those three are asserted by the GO side, per
// environment, from the printed errno.
const mmapFailGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/syscall.h>
#include <unistd.h>

static int failures = 0;
#define CHECK(cond, what) do { \
	if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } \
} while (0)

/* An anonymous mapping that is actually written and read back. This is the
   continuity probe: it runs before and after every refusal, so "the module is
   still here and its memory still works" is checked rather than assumed. */
static int anon_works(size_t n, unsigned char tag) {
	unsigned char *p = mmap(NULL, n, PROT_READ | PROT_WRITE,
	                        MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
	if (p == MAP_FAILED) {
		return 0;
	}
	memset(p, tag, n);
	int ok = p[0] == tag && p[n / 2] == tag && p[n - 1] == tag;
	munmap(p, n);
	return ok;
}

/* Reports one case as "<name> rc=<ok|fail> errno=<n>". A refusal is the
   interesting outcome, so errno is printed for the failure case only -- errno
   is not defined after a successful call. */
static void report(const char *name, void *rc) {
	if (rc == MAP_FAILED) {
		printf("mmapfail: %s rc=fail errno=%d\n", name, errno);
	} else {
		printf("mmapfail: %s rc=ok errno=0\n", name);
	}
	fflush(stdout);
}

int main(void) {
	CHECK(anon_works(64 * 1024, 0x5a), "anonymous mmap before the refusals");

	/* A page-aligned region we own, so the MAP_FIXED calls below have a legal
	   target natively as well -- MAP_FIXED over someone else's mapping is how a
	   native baseline corrupts itself. */
	void *base = mmap(NULL, 64 * 1024, PROT_READ | PROT_WRITE,
	                  MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
	CHECK(base != MAP_FAILED, "reserving a region for the MAP_FIXED cases");

	int fd = open("/tmp/mmapfail.dat", O_RDWR | O_CREAT | O_TRUNC, 0644);
	CHECK(fd >= 0, "creating the backing file");
	if (fd >= 0) {
		char buf[4096];
		memset(buf, 0x77, sizeof buf);
		CHECK(write(fd, buf, sizeof buf) == (ssize_t) sizeof buf, "writing the backing file");
	}

	/* 1. File-backed MAP_FIXED. Linux serves it; ecvisor has no demand paging
	      and refuses with ENODEV, the answer it already gives for a descriptor
	      it cannot map. Either way the module must survive. */
	errno = 0;
	report("filefixed", mmap(base, 4096, PROT_READ, MAP_PRIVATE | MAP_FIXED, fd, 0));

	/* 2. MAP_FIXED at an address above every aarch64 user address space.
	      Fails on both sides, for different reasons. */
	errno = 0;
	report("fixedwild", mmap((void *) 0xffffff0000000000ULL, 4096,
	                         PROT_READ | PROT_WRITE,
	                         MAP_PRIVATE | MAP_ANONYMOUS | MAP_FIXED, -1, 0));

	/* 3. The same, one page below the top of the address space, so that
	      addr + len OVERFLOWS. This is the case an unchecked bounds test let
	      through with a small wrapped sum, reaching the arena with an address
	      it then panicked on. */
	errno = 0;
	report("fixedwrap", mmap((void *) 0xfffffffffffff000ULL, 4096,
	                         PROT_READ | PROT_WRITE,
	                         MAP_PRIVATE | MAP_ANONYMOUS | MAP_FIXED, -1, 0));

	CHECK(anon_works(64 * 1024, 0xa5), "anonymous mmap after the MAP_FIXED refusals");

	/* 4. mmap of a descriptor with no file operations. ENODEV on Linux, and
	      ENODEV here -- the one case where this used to abort while its own
	      MAP_SHARED sibling returned the errno. */
	int p[2];
	CHECK(pipe(p) == 0, "creating a pipe");
	errno = 0;
	void *pm = mmap(NULL, 4096, PROT_READ, MAP_PRIVATE, p[0], 0);
	report("pipefd", pm);
	CHECK(pm == MAP_FAILED, "mmap of a pipe fails");
	CHECK(pm != MAP_FAILED || errno == ENODEV, "mmap of a pipe reports ENODEV");

	/* 5. Anonymous mapping with a page-aligned, non-zero offset. Linux does not
	      fail this: the offset is meaningless for an anonymous VMA and is simply
	      recorded. Aborting was the one answer it definitely does not give.

	      ⚠️ RAW SYSCALL, here and in case 6, and it is not a stylistic choice.
	      glibc's mmap wrapper rejects a misaligned offset itself and never
	      issues the syscall, so the library-level version of case 6 passed
	      identically with the runtime's own check DELETED -- it was testing
	      glibc. syscall() puts the arguments in front of the kernel, or in front
	      of us. */
	errno = 0;
	void *ao = (void *) syscall(SYS_mmap, (void *) 0, (size_t) 4096,
	                            PROT_READ | PROT_WRITE,
	                            MAP_PRIVATE | MAP_ANONYMOUS, -1, (off_t) 4096);
	report("anonoff", ao);
	CHECK(ao != MAP_FAILED, "anonymous mmap with a page-aligned offset succeeds");
	if (ao != MAP_FAILED) {
		memset(ao, 0x3c, 4096);
		CHECK(((unsigned char *) ao)[4095] == 0x3c, "that mapping is usable");
		munmap(ao, 4096);
	}

	/* 6. A MISALIGNED offset, which arm64's sys_mmap rejects before it looks at
	      anything else. EINVAL on both sides. */
	errno = 0;
	void *bo = (void *) syscall(SYS_mmap, (void *) 0, (size_t) 4096,
	                            PROT_READ | PROT_WRITE,
	                            MAP_PRIVATE | MAP_ANONYMOUS, -1, (off_t) 1);
	report("badoff", bo);
	CHECK(bo == MAP_FAILED, "a misaligned mmap offset fails");
	CHECK(bo != MAP_FAILED || errno == EINVAL, "a misaligned mmap offset reports EINVAL");

	/* 7. A ZERO length. do_mmap's very first statement is
	      "if (!len) return -EINVAL", ahead of everything else -- so this is
	      EINVAL on both sides. ecvisor used to SUCCEED here: the length was
	      rounded up (0 stays 0), a zero-byte slot was reserved, and the guest
	      got back an address for a mapping of nothing. Silent, which is why it
	      outlived the fatal! audit.

	      Raw syscall for the same reason as cases 5 and 6: the point is to put
	      the argument in front of the kernel, not in front of a libc that may
	      have opinions about it. */
	errno = 0;
	void *zl = (void *) syscall(SYS_mmap, (void *) 0, (size_t) 0,
	                            PROT_READ | PROT_WRITE,
	                            MAP_PRIVATE | MAP_ANONYMOUS, -1, (off_t) 0);
	report("zerolen", zl);
	CHECK(zl == MAP_FAILED, "a zero-length mmap fails");
	CHECK(zl != MAP_FAILED || errno == EINVAL, "a zero-length mmap reports EINVAL");

	/* 8. A length so large that page-aligning it OVERFLOWS. Linux answers
	      ENOMEM -- "Careful about overflows.." is the kernel's own comment on
	      the line, and it is ENOMEM and not EINVAL because the mapping cannot
	      be placed rather than the arguments being malformed. Unchecked,
	      ecvisor's len + GUEST_PAGE_MASK wrapped below the mask and the mask
	      cleared what was left, so SIZE_MAX became a zero-length SUCCESS.

	      SIZE_MAX specifically, so both sides take the overflow path: ecvisor
	      aligns to 64 KiB and Linux to 4 KiB, and only a length in the top 4 KiB
	      wraps for both. A merely enormous length would also be ENOMEM
	      natively, but by exhausting the address space instead, which is a
	      different claim.

	      musl's mmap rejects len >= PTRDIFF_MAX itself and never issues the
	      syscall, so this MUST be the raw syscall to mean anything on Alpine. */
	errno = 0;
	void *hl = (void *) syscall(SYS_mmap, (void *) 0, (size_t) -1,
	                            PROT_READ | PROT_WRITE,
	                            MAP_PRIVATE | MAP_ANONYMOUS, -1, (off_t) 0);
	report("hugelen", hl);
	CHECK(hl == MAP_FAILED, "an mmap length that overflows page alignment fails");
	CHECK(hl != MAP_FAILED || errno == ENOMEM,
	      "an mmap length that overflows page alignment reports ENOMEM");

	CHECK(anon_works(1 << 20, 0x3c), "larger anonymous mmap after every refusal");

	if (failures == 0) printf("MMAPFAIL-OK\n");
	return failures == 0 ? 0 : 1;
}
`

// TestMmapRefusalsDoNotKillTheModule is the ecvisor half. It asserts the shared
// expectations through the guest's own checks, and the three ecvisor-specific
// errnos here, where the native answer is necessarily different.
func TestMmapRefusalsDoNotKillTheModule(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "mmapfail", mmapFailGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "mmapfail")
	out := runWasm(t, ctx, wasm)

	assertMmapFailGuestPassed(t, out)

	const (
		enodev = 19
		enomem = 12
	)
	// Each of these three used to be a `fatal!`. Asserting the errno and not
	// merely "it failed" is what distinguishes the fix from the module dying in
	// some other way that also produces no MAP_FAILED line.
	assertMmapCase(t, out, "filefixed", "fail", enodev)
	assertMmapCase(t, out, "fixedwild", "fail", enomem)
	assertMmapCase(t, out, "fixedwrap", "fail", enomem)
}

// TestMmapRefusalsNativeBaseline runs the same guest on Linux. It is what makes
// the five shared cases evidence about mmap rather than about ecvisor: pipefd,
// anonoff, badoff, zerolen and hugelen are asserted by the guest itself, so this
// run failing means the expectation was wrong, not the implementation.
func TestMmapRefusalsNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "mmapfail", mmapFailGuestSrc)

	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/mmapfail")
	if !strings.Contains(out, "MMAPFAIL-OK") {
		t.Errorf("native run did not reach MMAPFAIL-OK: %v\n%s", err, out)
	}
	// Linux serves a file-backed MAP_FIXED. Stating it here is what keeps the
	// ecvisor-side ENODEV honest: it is a documented divergence, not a shared
	// expectation that happens to hold.
	assertMmapCase(t, out, "filefixed", "ok", 0)
	// Both out-of-range fixed addresses fail natively too, so ecvisor's ENOMEM
	// differs in the errno but not in the outcome.
	for _, c := range []string{"fixedwild", "fixedwrap"} {
		if got := mmapCase(t, out, c); got.rc != "fail" {
			t.Errorf("native %s: rc=%s, want fail -- the address was expected to be "+
				"outside every aarch64 user address space", c, got.rc)
		}
	}
}

type mmapOutcome struct {
	rc    string
	errno int
}

var mmapCaseRe = regexp.MustCompile(`mmapfail: (\S+) rc=(\S+) errno=(-?\d+)`)

func mmapCase(t *testing.T, out, name string) mmapOutcome {
	t.Helper()
	for _, m := range mmapCaseRe.FindAllStringSubmatch(out, -1) {
		if m[1] != name {
			continue
		}
		var e int
		if _, err := fmt.Sscanf(m[3], "%d", &e); err != nil {
			t.Fatalf("unparseable errno in %q: %v", m[0], err)
		}
		return mmapOutcome{rc: m[2], errno: e}
	}
	t.Fatalf("no %q line in the guest output; full output:\n%s", name, out)
	return mmapOutcome{}
}

func assertMmapCase(t *testing.T, out, name, wantRC string, wantErrno int) {
	t.Helper()
	got := mmapCase(t, out, name)
	if got.rc != wantRC || got.errno != wantErrno {
		t.Errorf("%s: rc=%s errno=%d, want rc=%s errno=%d",
			name, got.rc, got.errno, wantRC, wantErrno)
	}
}

func assertMmapFailGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "MMAPFAIL-OK") {
		t.Errorf("guest did not reach MMAPFAIL-OK; full output:\n%s", out)
	}
}
