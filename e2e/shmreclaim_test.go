package e2e

import (
	"strings"
	"testing"
)

// shmReclaimGuestSrc maps and gives up anonymous shared memory far more times
// than the shared window can hold at once.
//
// ecvisor has ONE arena, so a MAP_SHARED region is a VMA range exempted from the
// per-process arena swap, carved out of a 96 MiB window at the top of the mmap
// area. That window was a pure bump: nothing was ever returned, not by munmap
// and not by exit. `initdb` runs `postgres --boot` several times in sequence and
// each run maps its own shared_buffers, so the third run died with
//
//	FATAL: could not map anonymous shared memory: Cannot allocate memory
//
// with the two before it long dead.
//
// ROUNDS * BIG is deliberately several times the window (12 * 16 MiB = 192 MiB
// against 96 MiB), so the guest CANNOT complete unless regions are actually
// recycled -- that is what makes a pass mean something rather than merely
// recording that the window is large.
//
// Two ways of giving a region up are exercised, because they are separate code
// paths and only one of them is what postgres does:
//
//   - an odd round has the CHILD map the region and _exit without unmapping it,
//     which is the postgres case and the one a munmap-only fix would miss;
//   - an even round has the parent map it, share it with a child, and munmap.
//
// The canary matters as much as the count. Recycling a region that somebody
// still maps would also let all 12 rounds pass -- and would be far worse than
// the leak -- so a mailbox region stays mapped from the first line to the last
// and its bytes are re-checked every round. If a recycled 16 MiB region is ever
// handed out overlapping it, the canary is what says so.
const shmReclaimGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/wait.h>
#include <unistd.h>

#define ROUNDS  12
#define BIG     (16 * 1024 * 1024)
#define MAILBOX 65536
#define CANARY  0xA5

static int failures = 0;
#define CHECK(cond, what) do { \
	if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } \
} while (0)

static void *map_shared(size_t n) {
	return mmap(NULL, n, PROT_READ | PROT_WRITE, MAP_SHARED | MAP_ANONYMOUS, -1, 0);
}

/* The mailbox past its first 16 bytes must read as CANARY for the whole run. */
static int canary_intact(const unsigned char *mb) {
	for (size_t i = 16; i < MAILBOX; i++) {
		if (mb[i] != CANARY) {
			printf("FAIL canary clobbered at +%zu: %02x\n", i, mb[i]);
			return 0;
		}
	}
	return 1;
}

int main(void) {
	unsigned char *mb = map_shared(MAILBOX);
	CHECK(mb != MAP_FAILED, "mmap mailbox");
	if (mb == MAP_FAILED) {
		return 1;
	}
	memset(mb, CANARY, MAILBOX);
	mb[0] = 0;

	for (int i = 0; i < ROUNDS && !failures; i++) {
		if (i % 2 == 1) {
			/* Child maps and exits WITHOUT unmapping: only exit can reclaim it. */
			pid_t p = fork();
			CHECK(p >= 0, "fork (child-owned round)");
			if (p == 0) {
				unsigned char *big = map_shared(BIG);
				if (big == MAP_FAILED) {
					printf("FAIL child mmap %d bytes (errno=%d)\n", BIG, errno);
					_exit(2);
				}
				/* Touch both ends: a region that overlaps something live shows up
				   as a clobbered canary, not as a failed mmap. */
				big[0] = (unsigned char)i;
				big[BIG - 1] = (unsigned char)i;
				if (!canary_intact(mb)) {
					_exit(3);
				}
				mb[0] = (unsigned char)i;
				_exit(0);
			}
			int st = 0;
			CHECK(waitpid(p, &st, 0) == p, "waitpid (child-owned round)");
			CHECK(WIFEXITED(st) && WEXITSTATUS(st) == 0, "child-owned round exit status");
			CHECK(mb[0] == (unsigned char)i, "child wrote the mailbox");
		} else {
			/* Parent maps, shares with a child, then unmaps. */
			unsigned char *big = map_shared(BIG);
			CHECK(big != MAP_FAILED, "parent mmap");
			if (big == MAP_FAILED) {
				break;
			}
			memset(big, 0, 4096);
			pid_t p = fork();
			CHECK(p >= 0, "fork (parent-owned round)");
			if (p == 0) {
				/* Writing through the inherited mapping is what proves the region
				   is genuinely shared and not a private copy. */
				big[0] = (unsigned char)(i + 100);
				big[BIG - 1] = (unsigned char)(i + 100);
				_exit(canary_intact(mb) ? 0 : 3);
			}
			int st = 0;
			CHECK(waitpid(p, &st, 0) == p, "waitpid (parent-owned round)");
			CHECK(WIFEXITED(st) && WEXITSTATUS(st) == 0, "parent-owned round exit status");
			CHECK(big[0] == (unsigned char)(i + 100), "child's write is visible to the parent");
			CHECK(big[BIG - 1] == (unsigned char)(i + 100), "shared to the far end");
			CHECK(munmap(big, BIG) == 0, "munmap");
		}
		CHECK(canary_intact(mb), "mailbox canary after the round");
		/* Progress on its own line: on a regression the last number printed is
		   how many regions the window held before it ran out. */
		printf("round %d ok\n", i);
		fflush(stdout);
	}

	CHECK(canary_intact(mb), "mailbox canary at the end");
	if (failures == 0) {
		printf("SHMRECLAIM-OK\n");
	}
	return failures == 0 ? 0 : 1;
}
`

// TestSharedMemoryIsReclaimedUnderEcvisor is the regression guard for shared
// regions never being returned to the window. See shmReclaimGuestSrc.
func TestSharedMemoryIsReclaimedUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "shmreclaim", shmReclaimGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "shmreclaim")

	out := runWasm(t, ctx, wasm)
	assertShmReclaimGuestPassed(t, out)
}

// TestSharedMemoryIsReclaimedNativeBaseline runs the same guest on Linux, whose
// mmap is the oracle: it fixes what "shared" and "reclaimed" have to mean here.
func TestSharedMemoryIsReclaimedNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "shmreclaim", shmReclaimGuestSrc)

	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/shmreclaim")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertShmReclaimGuestPassed(t, out)
}

func assertShmReclaimGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "SHMRECLAIM-OK") {
		last := "none"
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "round ") {
				last = strings.TrimSpace(line)
			}
		}
		t.Errorf("guest did not reach SHMRECLAIM-OK (last progress: %s); full output:\n%s", last, out)
	}
}

// shmFileReclaimGuestSrc is the file-backed half of the same window.
//
// `mmap(MAP_SHARED)` of a FILE is registered as its own kind of shared region,
// because the file's PATH outlives the mapping: POSIX shm is shared by name, so
// a later mapper of the same name has to be handed the same region and find the
// bytes the earlier one wrote. That rule was implemented as "while the path
// still resolves, never reclaim" -- and glibc's locale and iconv loaders
// `MAP_SHARED` ordinary rootfs files that nobody ever unlinks
// (/usr/lib/locale/locale-archive, /etc/locale.alias, the gconv modules cache).
// So any guest that touched a locale pinned them for the life of the module.
//
// The cost is not their size. The shared window is carved DOWNWARD from the top
// of the mmap area and the private mmap bump is bounded by its floor, so a
// pinned region does not lose its own bytes -- it loses everything below it. In
// the five-program postgres module this froze the window at 0x10fd0000 and cut
// the private mmap area from 96 MiB to 15.8 MiB, and the postmaster died with
//
//	FATAL:  could not map anonymous shared memory ... (currently 78618624 bytes)
//
// Both directions are checked here, because each is the other's failure mode:
//
//   - PHASE A pins PIN_N * PIN_MB of READ-ONLY file mappings in a child that
//     exits without unmapping (the glibc shape), leaves the files in place, and
//     then asks for a private mapping LARGER than what would be left if they
//     were still held. Sized so that a pass cannot be luck: 24 MiB pinned
//     against a 96 MiB window, then an 80 MiB private request -- 72 MiB is all
//     that survives the leak, so the request fails by 8 MiB if a single one of
//     the three is still held.
//
//   - PHASE B is the rule's PURPOSE, and it must not be traded away. A writable
//     MAP_SHARED region is written through, unmapped by its last mapper with the
//     path still in place, and mapped again by name. The marker has to still be
//     there. This is exactly PostgreSQL's `posix` DSM backend, and it is checked
//     by CONTENT rather than by address because recycling that region is silent:
//     the second mapping succeeds either way and simply reads the file's zeroes.
const shmFileReclaimGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/wait.h>
#include <unistd.h>

#define MB       (1024 * 1024)
#define PIN_N    3
#define PIN_MB   8
#define PROBE_MB 80
#define DSM_LEN  65536
#define MARKER   "ecvisor-dsm-marker"

static int failures = 0;
#define CHECK(cond, what) do { \
	if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } \
} while (0)

static const char *pin_path(int i) {
	static char buf[64];
	snprintf(buf, sizeof buf, "/tmp/ro-shm-%d", i);
	return buf;
}

/* A real file, written in chunks so nothing needs an 8 MiB buffer. */
static int make_file(const char *path, size_t bytes) {
	int fd = open(path, O_RDWR | O_CREAT | O_TRUNC, 0644);
	if (fd < 0) { return -1; }
	static char chunk[65536];
	memset(chunk, 0x5A, sizeof chunk);
	for (size_t done = 0; done < bytes; done += sizeof chunk) {
		if (write(fd, chunk, sizeof chunk) != (ssize_t)sizeof chunk) { close(fd); return -1; }
	}
	close(fd);
	return 0;
}

int main(void) {
	/* ---- PHASE A: read-only file mappings must not pin the window ---- */
	for (int i = 0; i < PIN_N; i++) {
		CHECK(make_file(pin_path(i), (size_t)PIN_MB * MB) == 0, "create pin file");
	}
	if (failures) { return 1; }

	pid_t p = fork();
	CHECK(p >= 0, "fork (pin round)");
	if (p == 0) {
		/* Map all of them PROT_READ and _exit WITHOUT unmapping: that is the
		   shape glibc leaves behind, and only process teardown can reclaim it. */
		for (int i = 0; i < PIN_N; i++) {
			int fd = open(pin_path(i), O_RDONLY);
			if (fd < 0) { _exit(10 + i); }
			unsigned char *m = mmap(NULL, (size_t)PIN_MB * MB, PROT_READ,
			                        MAP_SHARED, fd, 0);
			close(fd);
			if (m == MAP_FAILED) { _exit(20 + i); }
			/* Read both ends: a mapping that did not materialize the file would
			   pass a pointer check and fail here. */
			if (m[0] != 0x5A || m[(size_t)PIN_MB * MB - 1] != 0x5A) { _exit(30 + i); }
		}
		_exit(0);
	}
	int st = 0;
	CHECK(waitpid(p, &st, 0) == p, "waitpid (pin round)");
	CHECK(WIFEXITED(st) && WEXITSTATUS(st) == 0, "child mapped every pin file read-only");
	if (WIFEXITED(st) && WEXITSTATUS(st) != 0) { printf("pin child exit %d\n", WEXITSTATUS(st)); }

	/* The files are STILL THERE. That is the whole point: under the old rule the
	   three regions are unreachable and unreclaimable at the same time. */
	for (int i = 0; i < PIN_N; i++) {
		CHECK(access(pin_path(i), F_OK) == 0, "pin file still present");
	}

	/* Largest private mapping still available. Reported either way, so a failure
	   says HOW MUCH was lost rather than only that something was. */
	int got = 0;
	for (int mb = PROBE_MB + 8; mb >= 8; mb -= 8) {
		void *q = mmap(NULL, (size_t)mb * MB, PROT_READ | PROT_WRITE,
		               MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
		if (q != MAP_FAILED) {
			((char *)q)[0] = 1;
			((char *)q)[(size_t)mb * MB - 1] = 1;
			munmap(q, (size_t)mb * MB);
			got = mb;
			break;
		}
	}
	printf("private-mmap-max=%d\n", got);
	CHECK(got >= PROBE_MB, "private mmap area survived the read-only file mappings");

	/* ---- PHASE B: a writable file region keeps its bytes across a remap ---- */
	const char *dsm = "/tmp/dsm-shm";
	int fd = open(dsm, O_RDWR | O_CREAT | O_TRUNC, 0644);
	CHECK(fd >= 0, "create dsm file");
	CHECK(ftruncate(fd, DSM_LEN) == 0, "ftruncate dsm file");
	char *a = mmap(NULL, DSM_LEN, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
	CHECK(a != MAP_FAILED, "map dsm writable");
	close(fd);
	if (a == MAP_FAILED) { return 1; }
	memcpy(a, MARKER, sizeof MARKER);
	memcpy(a + DSM_LEN - sizeof MARKER, MARKER, sizeof MARKER);
	CHECK(munmap(a, DSM_LEN) == 0, "unmap dsm (last mapper)");

	/* Same NAME, new descriptor, no mapper in between -- the sequence a second
	   postgres backend performs. */
	fd = open(dsm, O_RDWR);
	CHECK(fd >= 0, "reopen dsm file");
	char *b = mmap(NULL, DSM_LEN, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
	CHECK(b != MAP_FAILED, "remap dsm by name");
	close(fd);
	if (b == MAP_FAILED) { return 1; }
	CHECK(memcmp(b, MARKER, sizeof MARKER) == 0, "marker survived the remap (head)");
	CHECK(memcmp(b + DSM_LEN - sizeof MARKER, MARKER, sizeof MARKER) == 0,
	      "marker survived the remap (tail)");
	munmap(b, DSM_LEN);

	if (failures == 0) {
		printf("SHMFILE-OK\n");
	}
	return failures == 0 ? 0 : 1;
}
`

// TestSharedFileMappingsDoNotPinTheWindowUnderEcvisor is the regression guard
// for read-only file-backed MAP_SHARED regions being unreclaimable. See
// shmFileReclaimGuestSrc.
func TestSharedFileMappingsDoNotPinTheWindowUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "shmfile", shmFileReclaimGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "shmfile")

	out := runWasm(t, ctx, wasm)
	assertShmFileGuestPassed(t, out)
}

// TestSharedFileMappingsNativeBaseline pins what the two phases MEAN. Linux
// gives phase A trivially (there is no window to pin) and phase B through
// write-back, which is a different mechanism reaching the same observable --
// and the observable is what a guest is entitled to.
func TestSharedFileMappingsNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "shmfile", shmFileReclaimGuestSrc)

	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/shmfile")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertShmFileGuestPassed(t, out)
}

func assertShmFileGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "FAIL ") {
			t.Errorf("guest check failed: %s", s)
		}
		if strings.HasPrefix(s, "private-mmap-max=") {
			t.Logf("largest private mapping after the read-only file maps: %s", s)
		}
	}
	if !strings.Contains(out, "SHMFILE-OK") {
		t.Errorf("guest did not reach SHMFILE-OK; full output:\n%s", out)
	}
}
