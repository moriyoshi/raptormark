package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"raptormark/internal/fuse"
)

// The guard for ld.so's allocator hooks, restored on 2026-08-15 by CALLING
// ld.so's own installer at bring-up (internal/fuse.rtldHookInitVMA, word 12 of
// .ecv.stacklists).
//
// It is a separate guest from tlsdesc_test.go on purpose. That one needs a
// TLSDESC-carrying shared object and answers a different question; this one
// needs nothing but threads, so when it fails the answer is unambiguous.
//
// NEUTRALIZED 2026-08-15 against the builder image from before the fix
// (raptormark-builder:pattr2), where it dies immediately -- the fused ELF is
// identical, so that run is an object-cache hit and costs a relink, not a
// translate. See the test body.
const rtldHooksGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <pthread.h>
#include <stdio.h>
#include <string.h>
#include <sys/syscall.h>
#include <unistd.h>

#define ROUNDS 200

static int failures = 0;
#define CHECK(c, what) do { if (!(c)) { printf("FAIL %s\n", what); failures++; } } while (0)

/* Per-thread state, deliberately more than one word and deliberately aligned:
   a mismatched hook hands back a block that is not what the caller asked for,
   and a single int can be right by accident in memory that is wrong. */
static __thread unsigned char tls_pad[256] __attribute__((aligned(64)));
static __thread int tls_round = -1;

struct slot {
	int round;
	pid_t tid;
	void *block;
	int content_ok;
	int align_ok;
	int initial_ok;
};

static void *worker(void *p)
{
	struct slot *s = (struct slot *)p;
	s->tid = (pid_t)syscall(SYS_gettid);
	s->block = (void *)tls_pad;
	s->align_ok = (((unsigned long)tls_pad & 63UL) == 0);
	/* A FRESH thread's TLS must start from the image's initialiser. Two
	   different defects land here and the ROUND NUMBER tells them apart: at
	   round 0 there is no previous thread, so a failure means the block was
	   never initialised from .tdata at all; at a later round it means a
	   recycled block was handed back without being re-initialised.
	   Measured 2026-08-15: it fires at round 0 under ecvisor, because
	   _dl_allocate_tls_init copies from link_map->l_tls_initimage and a fused
	   image never ran the ld.so code that fills that in. */
	s->initial_ok = (tls_round == -1);

	tls_round = s->round;
	memset(tls_pad, (unsigned char)s->round, sizeof tls_pad);
	/* Yield so the other rounds' bookkeeping cannot be mistaken for ours. */
	sched_yield();
	s->content_ok = (tls_round == s->round &&
			 tls_pad[0] == (unsigned char)s->round &&
			 tls_pad[255] == (unsigned char)s->round);
	return NULL;
}

int main(void)
{
	printf("check rtld-hooks-thread-churn\n");
	fflush(stdout);

	void *first_block = NULL;
	int reused = 0;

	for (int r = 0; r < ROUNDS; r++) {
		pthread_t th;
		struct slot s;
		memset(&s, 0, sizeof s);
		s.round = r;

		int rc = pthread_create(&th, NULL, worker, &s);
		if (rc != 0) {
			/* THE failure this program is sized for. Report the round:
			   dying at round 0 is "no hooks at all", dying around 90 is
			   "allocation works, release does not". */
			printf("FAIL pthread_create at round %d (rc=%d %s)\n",
			       r, rc, strerror(rc));
			failures++;
			break;
		}
		if (pthread_join(th, NULL) != 0) {
			printf("FAIL pthread_join at round %d\n", r);
			failures++;
			break;
		}
		if (!s.content_ok) {
			printf("FAIL thread-local content at round %d\n", r);
			failures++;
			break;
		}
		if (!s.align_ok) {
			printf("FAIL thread-local alignment at round %d\n", r);
			failures++;
			break;
		}
		if (!s.initial_ok) {
			printf("FAIL thread-local not initialised from the image at round %d "
			       "(round 0 means never initialised; later means a recycled block)\n", r);
			failures++;
			break;
		}
		if (r == 0) {
			first_block = s.block;
		} else if (s.block == first_block) {
			reused++;
		}
	}

	/* The main thread's own thread-local must be untouched by all of that. */
	CHECK(tls_round == -1, "the initial thread's __thread value survived");

	/* Reuse is REPORTED, not asserted: which address a fresh block lands on is
	   the allocator's business. The load-bearing evidence is that 200 rounds
	   completed at all, which they cannot without release. */
	printf("rounds=%d block-reuse-hits=%d\n", ROUNDS, reused);

	if (failures == 0) {
		printf("RTLDHOOKS-OK\n");
	}
	return failures == 0 ? 0 : 1;
}
`

const rtldHooksFixture = "raptormark-tmp-rtldhooks:latest"

// buildRTLDHooksFixture stages a DYNAMIC glibc guest onto debian:trixie-slim,
// compiled in the builder image (aarch64 native, older glibc than trixie, which
// is the safe direction). Both images are local; nothing is fetched.
//
// ⚠️ Dynamic and fused, or the test is vacuous: a static glibc guest performs
// this setup in its own __libc_start_main and passes with the defect fully
// present. The same trap the musl reproducer documents.
//
// ⚠️ /usr/bin, not /bin: on a merged-usr image a /bin path resolves to no seeds
// at all. See the discovery entry in .agents/docs/TODO.md.
func buildRTLDHooksFixture(t *testing.T, ctx context.Context, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "rtldhooks.c"), []byte(rtldHooksGuestSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	df := "FROM " + builderImage() + ` AS build
COPY rtldhooks.c /tmp/rtldhooks.c
RUN gcc -O2 -o /tmp/rtldhooks /tmp/rtldhooks.c
FROM debian:trixie-slim
COPY --from=build /tmp/rtldhooks /usr/bin/rtldhooks
CMD ["/usr/bin/rtldhooks"]
`
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := dockerBuild(ctx, abs, rtldHooksFixture); err != nil {
		t.Skipf("cannot build the rtldhooks fixture (needs debian:trixie-slim locally): %v\n%s", err, out)
	}
}

// TestRTLDHooksNativeBaseline is the oracle. Measured on the host: 200 rounds,
// 199 block-reuse hits -- glibc hands the same TLS block back every round,
// which is what makes the "a fresh thread starts from the image initialiser"
// check in the guest meaningful rather than decorative.
func TestRTLDHooksNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	buildRTLDHooksFixture(t, ctx, t.TempDir())

	out, err := dockerRunImage(ctx, rtldHooksFixture)
	if err != nil {
		t.Errorf("native run failed: %v", err)
	}
	assertRTLDHooksPassed(t, out)
}

// TestRTLDHooksUnderEcvisor is the regression guard: a fused dynamic glibc
// image must be able to create and destroy 200 threads.
//
// The round count is the argument that RELEASE works, not just allocation. The
// guest mmap window is 96 MiB and the seeded default thread stack is 1 MiB, so
// ~90 threads' stacks fill it: 200 sequential rounds cannot complete unless the
// memory is being freed and reused. A five-round version of this test would
// pass with the free hook entirely absent.
func TestRTLDHooksUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	buildRTLDHooksFixture(t, ctx, dir)

	root, entry := discoverImage(t, ctx, rtldHooksFixture, "/usr/bin/rtldhooks")
	fused, err := fuse.Fuse(entry.HostPath, fuse.Options{
		LibraryPaths: []string{
			filepath.Join(root, "lib"),
			filepath.Join(root, "usr/lib"),
			filepath.Join(root, "lib/aarch64-linux-gnu"),
			filepath.Join(root, "usr/lib/aarch64-linux-gnu"),
		},
	})
	if err != nil {
		t.Fatalf("fusing the rtldhooks guest: %v", err)
	}
	fusedPath := filepath.Join(dir, "rtldhooks.fused")
	if err := os.WriteFile(fusedPath, fused, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Logf("fused rtldhooks guest: %d bytes", len(fused))

	wasm := liftOne(t, ctx, img, dir, fusedPath, "rtldhooks")
	out := runThreadGapsPhase(t, ctx, wasm)
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if strings.Contains(l, "hook installer") || strings.HasPrefix(l, "rounds=") {
			t.Log(l)
		}
	}
	assertRTLDHooksPassed(t, out)
}

func assertRTLDHooksPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "RTLDHOOKS-OK") {
		t.Errorf("guest did not reach RTLDHOOKS-OK; full output:\n%s", out)
	}
}
