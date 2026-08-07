package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// prctlMMGuestSrc exercises the two prctls every ruby run reaches and which
// ecvisor used to refuse with EINVAL from the NR_PRCTL catch-all:
//
//   - PR_SET_THP_DISABLE / PR_GET_THP_DISABLE (41/42), called once per ruby
//     startup from ruby_setup.
//   - PR_SET_VMA + PR_SET_VMA_ANON_NAME (0x53564d41/0), called for every GC
//     heap page from heap_page_allocate_and_initialize and ruby_annotate_mmap.
//
// Linux returns 0 for both, so the refusal was a divergence. Every expectation
// is an assertion INSIDE the guest and the same source runs under ecvisor and
// natively (…NativeBaseline), so if any line below is wrong about Linux the
// native run says so rather than this file quietly encoding a model of it.
//
// ⚠️ WHAT IS DELIBERATELY NOT CHECKED, and why each would be a check that
// cannot mean the same thing on both sides:
//
//   - /proc/<pid>/maps. On Linux the anonymous-VMA name's ONLY observable
//     effect is the `[anon:NAME]` it prints there. ecvisor has no /proc at all,
//     which is precisely why accepting the call without storing the name is
//     truthful -- so the test checks the syscall contract and nothing about
//     maps, the same way dumpable_test.go checks no core-dump behaviour.
//   - Naming an unmapped range INSIDE the guest's own address space. Linux
//     answers ENOMEM; ecvisor's bump arena cannot tell a live mapping from a
//     hole (the limit NR_MREMAP states for growing in place), so it answers 0.
//     A stated divergence, not an accident. The out-of-arena case IS checked
//     below, because that one ecvisor can answer honestly.
//   - Naming a FILE-BACKED mapping. Linux answers EBADF; the arena has no
//     file-backed mappings to distinguish, so ecvisor answers 0.
//   - A bad name POINTER of 1. Linux faults; ecvisor's arena starts at guest
//     address 0, so address 1 is ordinary readable memory there. The EFAULT
//     check below uses addresses outside the arena, which both sides refuse.
//   - The INITIAL value of PR_GET_THP_DISABLE. It is inherited across fork AND
//     execve (MMF_INIT_MASK), so on a real system it is whatever the session
//     handed down -- measured 1 on this host's shell. Every check below sets it
//     first.
//   - execve. liftOne builds a single-program module with nothing to exec into,
//     the same limitation threadproc_test.go records.
const prctlMMGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <unistd.h>

#define PR_SET_THP_DISABLE 41
#define PR_GET_THP_DISABLE 42
#define PR_SET_VMA 0x53564d41
#define PR_SET_VMA_ANON_NAME 0

static int failures = 0;
#define CHECK(c, what) do { if (!(c)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } } while (0)

/* The raw syscall throughout: prctl(3) is variadic and we need to control all
   four trailing arguments exactly, including values wider than 32 bits. */
static long pr(long op, unsigned long a2, unsigned long a3, unsigned long a4,
               unsigned long a5)
{
	errno = 0;
	return syscall(SYS_prctl, op, a2, a3, a4, a5);
}

static long setname(unsigned long addr, unsigned long len, const void *name)
{
	return pr(PR_SET_VMA, PR_SET_VMA_ANON_NAME, addr, len, (unsigned long)name);
}

static void *thp_worker(void *unused)
{
	(void)unused;
	/* Per-MM in Linux, so a thread's SET is what its siblings GET. */
	CHECK(pr(PR_SET_THP_DISABLE, 1, 0, 0, 0) == 0, "worker thread PR_SET_THP_DISABLE(1)");
	return NULL;
}

static void thp_checks(void)
{
	/* The plain accept/read-back pair. This is the whole of what ruby needs. */
	CHECK(pr(PR_SET_THP_DISABLE, 1, 0, 0, 0) == 0, "PR_SET_THP_DISABLE(1) succeeds");
	CHECK(pr(PR_GET_THP_DISABLE, 0, 0, 0, 0) == 1, "PR_GET_THP_DISABLE reads back 1");
	CHECK(pr(PR_SET_THP_DISABLE, 0, 0, 0, 0) == 0, "PR_SET_THP_DISABLE(0) succeeds");
	CHECK(pr(PR_GET_THP_DISABLE, 0, 0, 0, 0) == 0, "PR_GET_THP_DISABLE reads back 0");

	/* NOT PR_SET_DUMPABLE's shape: arg2 is a truthiness, not a validated
	   value, so 2 and -1 are accepted and both store the bit. */
	CHECK(pr(PR_SET_THP_DISABLE, 2, 0, 0, 0) == 0, "PR_SET_THP_DISABLE(2) succeeds");
	CHECK(pr(PR_GET_THP_DISABLE, 0, 0, 0, 0) == 1, "PR_SET_THP_DISABLE(2) stored 1");
	CHECK(pr(PR_SET_THP_DISABLE, 0, 0, 0, 0) == 0, "back to 0");
	CHECK(pr(PR_SET_THP_DISABLE, (unsigned long)-1, 0, 0, 0) == 0,
	      "PR_SET_THP_DISABLE(-1) succeeds");
	CHECK(pr(PR_GET_THP_DISABLE, 0, 0, 0, 0) == 1, "PR_SET_THP_DISABLE(-1) stored 1");

	/* The 64-bit form of zero. Truncating arg2 to 32 bits would CLEAR the
	   flag on a call that sets it -- the failure is a wrong value, not a
	   wrong errno, which is why the GET is the assertion. */
	CHECK(pr(PR_SET_THP_DISABLE, 0, 0, 0, 0) == 0, "back to 0 again");
	CHECK(pr(PR_SET_THP_DISABLE, 0x100000000UL, 0, 0, 0) == 0,
	      "PR_SET_THP_DISABLE(0x100000000) succeeds");
	CHECK(pr(PR_GET_THP_DISABLE, 0, 0, 0, 0) == 1,
	      "PR_SET_THP_DISABLE(0x100000000) set the bit, not cleared it");

	/* arg3..arg5 are RESERVED by the setter and must be zero, and a refused
	   call must not have stored anything. */
	CHECK(pr(PR_SET_THP_DISABLE, 0, 0, 0, 0) == 0, "back to 0 for the reserved-arg checks");
	errno = 0;
	CHECK(pr(PR_SET_THP_DISABLE, 1, 1, 0, 0) == -1 && errno == EINVAL,
	      "PR_SET_THP_DISABLE(1) with arg3 set is EINVAL");
	errno = 0;
	CHECK(pr(PR_SET_THP_DISABLE, 1, 0, 1, 0) == -1 && errno == EINVAL,
	      "PR_SET_THP_DISABLE(1) with arg4 set is EINVAL");
	errno = 0;
	CHECK(pr(PR_SET_THP_DISABLE, 1, 0, 0, 1) == -1 && errno == EINVAL,
	      "PR_SET_THP_DISABLE(1) with arg5 set is EINVAL");
	CHECK(pr(PR_GET_THP_DISABLE, 0, 0, 0, 0) == 0, "a refused set left the flag alone");

	/* The GETTER reserves arg2 as well -- a different rule from the setter's. */
	errno = 0;
	CHECK(pr(PR_GET_THP_DISABLE, 1, 0, 0, 0) == -1 && errno == EINVAL,
	      "PR_GET_THP_DISABLE with arg2 set is EINVAL");
	errno = 0;
	CHECK(pr(PR_GET_THP_DISABLE, 0, 1, 0, 0) == -1 && errno == EINVAL,
	      "PR_GET_THP_DISABLE with arg3 set is EINVAL");

	/* fork copies the mm's flag. */
	CHECK(pr(PR_SET_THP_DISABLE, 1, 0, 0, 0) == 0, "PR_SET_THP_DISABLE(1) before fork");
	pid_t child = fork();
	if (child == 0) {
		_exit(pr(PR_GET_THP_DISABLE, 0, 0, 0, 0) == 1 ? 42 : 43);
	}
	CHECK(child > 0, "fork");
	int st = 0;
	CHECK(waitpid(child, &st, 0) == child, "reap the child");
	CHECK(WIFEXITED(st) && WEXITSTATUS(st) == 42, "the child inherited thp_disable=1");

	/* Back to 0, so the thread check observes a CHANGE rather than a value
	   that was already 1. */
	CHECK(pr(PR_SET_THP_DISABLE, 0, 0, 0, 0) == 0, "PR_SET_THP_DISABLE(0) after fork");
	pthread_t th;
	CHECK(pthread_create(&th, NULL, thp_worker, NULL) == 0, "create worker");
	CHECK(pthread_join(th, NULL) == 0, "join worker");
	CHECK(pr(PR_GET_THP_DISABLE, 0, 0, 0, 0) == 1,
	      "a worker's PR_SET_THP_DISABLE reached the group");
	CHECK(pr(PR_SET_THP_DISABLE, 0, 0, 0, 0) == 0, "leave THP as we found it");
}

static void vma_name_checks(void)
{
	long pg = sysconf(_SC_PAGESIZE);
	char *a = mmap(NULL, 4 * pg, PROT_READ | PROT_WRITE,
	               MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
	CHECK(a != MAP_FAILED, "mmap an anonymous region to name");
	if (a == MAP_FAILED) {
		return;
	}
	unsigned long base = (unsigned long)a;

	/* What ruby actually calls, with the name it actually passes. */
	CHECK(setname(base, pg, "Ruby:GC:default:heap_page_body_allocate") == 0,
	      "PR_SET_VMA_ANON_NAME with ruby's own name");
	CHECK(setname(base, pg, "Ruby:GC:default:heap_page_body_allocate") == 0,
	      "naming the same range twice");
	CHECK(setname(base, pg, NULL) == 0, "a NULL name clears the label");
	CHECK(setname(base, pg, "") == 0, "the empty name is accepted");

	/* Length: the kernel's buffer is 80 bytes INCLUDING the NUL, so 79
	   characters is the longest name that works. Both sides of the boundary,
	   because an off-by-one is the whole risk. */
	char n[128];
	memset(n, 'a', sizeof n);
	n[79] = 0;
	CHECK(setname(base, pg, n) == 0, "a 79-character name is accepted");
	n[79] = 'a';
	n[80] = 0;
	errno = 0;
	CHECK(setname(base, pg, n) == -1 && errno == EINVAL,
	      "an 80-character name is EINVAL");

	/* Characters. SPACE is ALLOWED and DEL is not, which is the opposite of
	   the natural guess for both; the five excluded punctuation characters
	   are excluded because the name is printed as [anon:NAME]. */
	CHECK(setname(base, pg, "a b") == 0, "a name containing a space is accepted");
	CHECK(setname(base, pg, "!#%&*+,-./:;<=>?@^_{|}~") == 0,
	      "ordinary punctuation is accepted");
	errno = 0;
	CHECK(setname(base, pg, "a\tb") == -1 && errno == EINVAL, "a tab in the name is EINVAL");
	errno = 0;
	CHECK(setname(base, pg, "a\x7f" "b") == -1 && errno == EINVAL, "DEL in the name is EINVAL");
	errno = 0;
	CHECK(setname(base, pg, "a\x1f" "b") == -1 && errno == EINVAL, "0x1f in the name is EINVAL");
	errno = 0;
	CHECK(setname(base, pg, "a\x80" "b") == -1 && errno == EINVAL,
	      "a high byte in the name is EINVAL");
	errno = 0;
	CHECK(setname(base, pg, "a[b") == -1 && errno == EINVAL, "'[' in the name is EINVAL");
	errno = 0;
	CHECK(setname(base, pg, "a]b") == -1 && errno == EINVAL, "']' in the name is EINVAL");
	errno = 0;
	CHECK(setname(base, pg, "a\\b") == -1 && errno == EINVAL, "'\\' in the name is EINVAL");
	errno = 0;
	CHECK(setname(base, pg, "a\x60" "b") == -1 && errno == EINVAL,
	      "a backtick in the name is EINVAL");
	errno = 0;
	CHECK(setname(base, pg, "a$b") == -1 && errno == EINVAL, "'$' in the name is EINVAL");

	/* The name is validated BEFORE the range, so a bad name at an address
	   the range check would reject still reports the NAME's error. */
	errno = 0;
	CHECK(setname(0x10000000000UL, pg, "a$b") == -1 && errno == EINVAL,
	      "a bad name at an unmapped address is EINVAL, not ENOMEM");
	errno = 0;
	CHECK(setname(base, 0, "a$b") == -1 && errno == EINVAL,
	      "a bad name with a zero length is still EINVAL");

	/* A name pointer outside every mapping faults. (A pointer of 1 is NOT
	   used: see the note on this test in prctlmm_test.go.) */
	errno = 0;
	CHECK(setname(base, pg, (const void *)-1L) == -1 && errno == EFAULT,
	      "a name pointer of -1 is EFAULT");
	errno = 0;
	CHECK(setname(base, pg, (const void *)0x10000000000UL) == -1 && errno == EFAULT,
	      "a name pointer far outside any mapping is EFAULT");

	/* Address and length. The length is ROUNDED UP to a page, not refused;
	   a zero length succeeds without consulting any mapping at all. */
	errno = 0;
	CHECK(setname(base + 1, pg, "x") == -1 && errno == EINVAL,
	      "an unaligned address is EINVAL");
	CHECK(setname(base, 1, "x") == 0, "a sub-page length names a whole page");
	CHECK(setname(base, pg + 1, "x") == 0, "a length past a page boundary is rounded up");
	CHECK(setname(base, 0, "x") == 0, "a zero length succeeds");
	CHECK(setname(0x10000000000UL, 0, "x") == 0,
	      "a zero length at an unmapped address still succeeds");
	errno = 0;
	CHECK(setname(base, ~0UL, "x") == -1 && errno == EINVAL,
	      "a length whose page rounding overflows is EINVAL");

	/* An address nothing could have mapped. This is the one range refusal
	   ecvisor can answer honestly, because everything a guest can address
	   there is the arena. */
	errno = 0;
	CHECK(setname(0x10000000000UL, pg, "x") == -1 && errno == ENOMEM,
	      "naming a page at an unmapped address is ENOMEM");

	/* The sub-option in arg2 must be exactly PR_SET_VMA_ANON_NAME, compared
	   over the full 64 bits: truncating to 32 would fold 0x100000000 onto the
	   valid 0. */
	errno = 0;
	CHECK(pr(PR_SET_VMA, 1, base, pg, (unsigned long)"x") == -1 && errno == EINVAL,
	      "an unknown PR_SET_VMA sub-option is EINVAL");
	errno = 0;
	CHECK(pr(PR_SET_VMA, 0x100000000UL, base, pg, (unsigned long)"x") == -1 && errno == EINVAL,
	      "a 64-bit PR_SET_VMA sub-option is not truncated onto 0");

	/* There is no getter, on either side. */
	errno = 0;
	CHECK(pr(0x53564d42, 0, 0, 0, 0) == -1 && errno == EINVAL,
	      "PR_SET_VMA+1 is not a prctl");
}

int main(void)
{
	thp_checks();
	vma_name_checks();
	if (failures == 0) {
		printf("PRCTLMM-OK\n");
	}
	return failures == 0 ? 0 : 1;
}
`

// TestPrctlMMUnderEcvisor guards the PR_SET_THP_DISABLE and
// PR_SET_VMA_ANON_NAME rulings: accepted, validated the way Linux validates
// them, and -- for the THP flag alone -- readable back.
func TestPrctlMMUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	elf := compileGuest(t, ctx, dir, "prctlmm", prctlMMGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "prctlmm")
	assertPrctlMMGuestPassed(t, runWasm(t, ctx, wasm))
}

// TestPrctlMMNativeBaseline pins the expectations to Linux.
func TestPrctlMMNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "prctlmm", prctlMMGuestSrc)
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx, []string{"-v", abs + ":/w"}, "/w/prctlmm")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertPrctlMMGuestPassed(t, out)
}

func assertPrctlMMGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "PRCTLMM-OK") {
		t.Errorf("guest did not reach PRCTLMM-OK; full output:\n%s", out)
	}
}
