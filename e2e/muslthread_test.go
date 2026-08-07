package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"raptormark/internal/fuse"
	"raptormark/internal/image"
)

// dockerBuild builds `dir` and tags it. Separate from `dockerRun`, which runs a
// command inside the BUILDER image; this builds a guest fixture.
func dockerBuild(ctx context.Context, dir, tag string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "build", "-q", "-t", tag, dir).CombinedOutput()
	return string(out), err
}

// dockerRunImage runs a fixture image's own default command.
func dockerRunImage(ctx context.Context, tag string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", tag).CombinedOutput()
	return string(out), err
}

// A DYNAMIC musl guest that creates a thread, fused and lifted -- the exact
// shape redis:7-alpine dies in, reduced to something that runs in minutes.
//
// Why it has to be dynamic AND fused, which is the whole reason this file
// exists: for a STATIC musl binary `__libc_start_main` calls `__init_tls`
// itself, so `libc.tls_size`/`tls_align`/`tls_head` are populated and threads
// work. For a DYNAMIC one that work happens inside ld.so -- and a fused image
// enters the executable's `_start` and never runs ld.so, so those fields stay
// zero, `__copy_tls` computes a thread's TLS block relative to NULL, and the
// first `pthread_create` never returns. A static musl guest compiled with
// `gcc -static` would pass this test with the defect fully present.
//
// The fixture is built from LOCAL images only (a compile stage that has gcc and
// musl-dev, then a small alpine runtime stage), so it needs no network: `apk
// add` inside a fresh image would.
const muslThreadGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <pthread.h>
#include <stdio.h>
#include <string.h>
#include <sys/syscall.h>
#include <unistd.h>

#define NTHREADS 3

static int failures = 0;
#define CHECK(c, what) do { if (!(c)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } } while (0)

static __thread int tls_scalar = -1;
static __thread char tls_aligned[128] __attribute__((aligned(64)));

struct slot {
	int idx;
	pid_t tid;
	void *block;
	int scalar_ok;
	int align_ok;
	pthread_barrier_t *all;
};

static void *worker(void *p)
{
	struct slot *s = (struct slot *)p;
	s->tid = (pid_t)syscall(SYS_gettid);
	tls_scalar = s->idx;
	memset(tls_aligned, s->idx, sizeof tls_aligned);
	s->block = (void *)&tls_scalar;
	s->align_ok = (((unsigned long)tls_aligned & 63UL) == 0);
	/* All three in flight at once, or a shared thread pointer is
	   indistinguishable from a private one -- each would write and read the
	   same word between switches. */
	pthread_barrier_wait(s->all);
	s->scalar_ok = (tls_scalar == s->idx);
	return NULL;
}

int main(void)
{
	printf("check musl-pthread-create\n");
	fflush(stdout);

	pthread_barrier_t all;
	pthread_barrier_init(&all, NULL, NTHREADS);
	pthread_t th[NTHREADS];
	struct slot slots[NTHREADS];
	memset(slots, 0, sizeof slots);

	for (int i = 0; i < NTHREADS; i++) {
		slots[i].idx = i;
		slots[i].all = &all;
		int rc = pthread_create(&th[i], NULL, worker, &slots[i]);
		if (rc != 0) {
			/* THE defect: musl's __copy_tls sizes the new thread's TLS
			   block from libc.tls_size/tls_align, which a fused image
			   never populated. Report rc, not errno -- musl returns the
			   error, and reading errno instead is what made redis print
			   an unrelated message. */
			printf("FAIL pthread_create %d (rc=%d %s)\n", i, rc, strerror(rc));
			failures++;
			printf("MUSLTHREAD-FAILURES %d\n", failures);
			return 1;
		}
	}
	for (int i = 0; i < NTHREADS; i++) {
		CHECK(pthread_join(th[i], NULL) == 0, "join");
	}

	printf("check musl-thread-tls\n");
	fflush(stdout);
	for (int i = 0; i < NTHREADS; i++) {
		CHECK(slots[i].scalar_ok, "a thread read back its own __thread value");
		CHECK(slots[i].align_ok, "the aligned __thread block honours alignof(64)");
		for (int j = i + 1; j < NTHREADS; j++) {
			CHECK(slots[i].block != slots[j].block,
			      "two threads have DIFFERENT TLS blocks");
			CHECK(slots[i].tid != slots[j].tid, "each thread has its own tid");
		}
	}
	CHECK(tls_scalar == -1, "the initial thread's __thread value survived");
	pthread_barrier_destroy(&all);

	if (failures == 0) {
		printf("MUSLTHREAD-OK\n");
	}
	return failures == 0 ? 0 : 1;
}
`

// muslThreadFixture is the image tag this test builds and reuses.
const muslThreadFixture = "raptormark-tmp-muslthread:latest"

// buildMuslThreadFixture produces a small Alpine image containing a DYNAMICALLY
// linked musl guest. Two stages, both from images already on this machine:
// rust:1-alpine carries gcc and musl-dev, alpine:3 is the runtime rootfs. No
// network, no apk.
func buildMuslThreadFixture(t *testing.T, ctx context.Context, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "mthread.c"), []byte(muslThreadGuestSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	df := `FROM rust:1-alpine AS build
COPY mthread.c /tmp/mthread.c
RUN gcc -O2 -o /tmp/mthread /tmp/mthread.c && ldd /tmp/mthread || true
FROM alpine:3
COPY --from=build /tmp/mthread /bin/mthread
CMD ["/bin/mthread"]
`
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dockerBuild(ctx, abs, muslThreadFixture)
	if err != nil {
		t.Skipf("cannot build the musl fixture (needs rust:1-alpine and alpine:3 locally): %v\n%s", err, out)
	}
}

// TestMuslThreadNativeBaseline runs the fixture as an ordinary container, which
// is the oracle: musl creates threads perfectly well when ld.so runs.
func TestMuslThreadNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	buildMuslThreadFixture(t, ctx, t.TempDir())

	out, err := dockerRunImage(ctx, muslThreadFixture)
	if err != nil {
		t.Errorf("native run failed: %v", err)
	}
	assertMuslThreadPassed(t, out)
}

// TestMuslThreadUnderEcvisor was the reproducer for the last open threading
// gap and is now its regression guard. It went from
// `range end index 4294967140 out of range` -- a TLS pointer computed relative
// to NULL -- to green when `libc.tls_head`/`tls_size`/`tls_align`/`tls_cnt`
// were seeded at bring-up on 2026-08-15; the gate came off with the fix.
//
// It is cheap for what it covers: ~20 s including discovery, fusing and a cold
// translate, against a defect whose only other trigger was redis:7-alpine.
func TestMuslThreadUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	buildMuslThreadFixture(t, ctx, dir)

	root, entry := discoverImage(t, ctx, muslThreadFixture, "/bin/mthread")
	fused, err := fuse.Fuse(entry.HostPath, fuse.Options{
		LibraryPaths: []string{filepath.Join(root, "lib"), filepath.Join(root, "usr/lib")},
	})
	if err != nil {
		t.Fatalf("fusing the musl guest: %v", err)
	}
	fusedPath := filepath.Join(dir, "mthread.fused")
	if err := os.WriteFile(fusedPath, fused, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Logf("fused musl guest: %d bytes", len(fused))

	wasm := liftOne(t, ctx, img, dir, fusedPath, "mthread")
	out := runThreadGapsPhase(t, ctx, wasm)
	// The musl bring-up lines, on success as well as failure. This test exists
	// because a fused image needs state ld.so would have written, and "which of
	// those seeds actually ran" is invisible from a green marker alone.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "[ecvisor] musl") {
			t.Log(strings.TrimSpace(line))
		}
	}
	assertMuslThreadPassed(t, out)
}

// discoverImage exports an image's rootfs and returns it with the named
// program's inventory entry. The OpenSSL fixture has its own copy of this with
// its own assertions; this one is parameterised because a second image now
// needs it.
func discoverImage(t *testing.T, ctx context.Context, tag, entryPath string) (string, image.Executable) {
	t.Helper()
	cfg, err := image.Inspect(ctx, tag)
	if err != nil {
		t.Fatalf("inspecting %s: %v", tag, err)
	}
	root := t.TempDir()
	if err := image.ExportRootfs(ctx, tag, root); err != nil {
		t.Fatalf("exporting rootfs: %v", err)
	}
	inv, err := image.Scan(root)
	if err != nil {
		t.Fatalf("scanning rootfs: %v", err)
	}
	closure, err := image.Closure(inv, image.ClosureOptions{
		Seeds: image.EntrypointSeeds(cfg, inv), PathDirs: image.PathDirs(cfg.Env), Max: 10000,
	})
	if err != nil {
		t.Fatalf("closure: %v", err)
	}
	if !slices.Contains(closure, entryPath) {
		t.Fatalf("closure does not contain %s: %v", entryPath, closure)
	}
	entry, ok := inv.Programs[entryPath]
	if !ok {
		t.Fatalf("%s is not an inventoried program", entryPath)
	}
	t.Logf("discovery: %d programs in the closure, entry %s", len(closure), entryPath)
	return root, entry
}

func assertMuslThreadPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "MUSLTHREAD-OK") {
		t.Errorf("guest did not reach MUSLTHREAD-OK; full output:\n%s", out)
	}
}
