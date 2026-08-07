package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"raptormark/internal/fuse"
)

// TLSDESC, executed. The prelinker has emitted the descriptors and the
// `_ecv_tlsdesc_return` stub since 2026-08-09, lifted them, and checked their
// two words -- but nothing had ever RUN one. The descriptors in the tree live
// in libicuuc and libsystemd, and reaching them meant ICU collation inside a
// real postgres backend, which in turn waited on a vector FCVTZS the lifter did
// not implement.
//
// This gets there directly: a shared library with a thread-local, read through
// its accessor from two threads. On aarch64 gcc emits TLSDESC for a
// general-dynamic access by default, so the ordinary way of writing this IS the
// case under test -- and the fixture build asserts the relocations exist rather
// than trusting that.
//
// The two-thread shape is the point. A descriptor that resolved to the initial
// thread's block would return the same address to everybody and satisfy any
// single-threaded check.
const tlsdescDSOSrc = `static __thread int dso_tls;
static __thread char dso_tls_pad[96] __attribute__((aligned(32)));

int dso_tls_set(int v)
{
	dso_tls = v;
	dso_tls_pad[0] = (char)v;
	dso_tls_pad[95] = (char)v;
	return dso_tls;
}

int *dso_tls_addr(void) { return &dso_tls; }
char *dso_tls_pad_addr(void) { return dso_tls_pad; }
`

const tlsdescMainSrc = `#define _GNU_SOURCE
#include <pthread.h>
#include <stdio.h>
#include <string.h>
#include <sys/syscall.h>
#include <unistd.h>

int dso_tls_set(int);
int *dso_tls_addr(void);
char *dso_tls_pad_addr(void);

static int failures = 0;
#define CHECK(c, what) do { if (!(c)) { printf("FAIL %s\n", what); failures++; } } while (0)

#define NTHREADS 2

struct slot {
	int idx;
	int set_ret;
	int readback;
	int *addr;
	char *pad;
	int pad_ok;
	pthread_barrier_t *all;
};

static void *worker(void *p)
{
	struct slot *s = (struct slot *)p;
	/* Every one of these calls goes through a TLSDESC descriptor: the
	   accessor is in a shared object, so the access is general-dynamic. */
	s->set_ret = dso_tls_set(s->idx);
	s->addr = dso_tls_addr();
	s->pad = dso_tls_pad_addr();
	pthread_barrier_wait(s->all);
	s->readback = *s->addr;
	s->pad_ok = (s->pad[0] == (char)s->idx && s->pad[95] == (char)s->idx &&
		     ((unsigned long)s->pad & 31UL) == 0);
	return NULL;
}

int main(int argc, char **argv)
{
	/* "explicit" passes an attr carrying its own stacksize, which takes the
	   DEFAULT-attr branch of glibc's allocate_stack out of the picture while
	   leaving everything downstream of it in. It is a probe, not a feature:
	   if a fused glibc still dies on "assertion failed: size != 0" with a
	   stacksize the guest supplied directly, the fault cannot be the default
	   attr the runtime seeds -- it is further along, in the TLS-geometry
	   state ld.so would have written (dl_tls_static_align, whose unset
	   value makes size &= ~(align-1) mask everything away).

	   Selected by argv so ONE fused image answers both questions: this guest
	   costs ~10 minutes to translate, and a second binary would double it. */
	int explicit_attr = (argc > 1 && strcmp(argv[1], "explicit") == 0);
	printf("check tlsdesc-through-a-shared-object%s\n",
	       explicit_attr ? " (explicit attr)" : "");
	fflush(stdout);

	pthread_attr_t attr;
	if (explicit_attr) {
		if (pthread_attr_init(&attr) != 0 ||
		    pthread_attr_setstacksize(&attr, 1024 * 1024) != 0) {
			printf("FAIL building an explicit attr\n");
			return 1;
		}
	}

	pthread_barrier_t all;
	pthread_barrier_init(&all, NULL, NTHREADS);
	pthread_t th[NTHREADS];
	struct slot slots[NTHREADS];
	memset(slots, 0, sizeof slots);

	for (int i = 0; i < NTHREADS; i++) {
		slots[i].idx = i + 7;
		slots[i].all = &all;
		int rc = pthread_create(&th[i], NULL, worker, &slots[i]);
		if (rc != 0) {
			printf("FAIL pthread_create %d (rc=%d %s)\n", i, rc, strerror(rc));
			return 1;
		}
	}
	for (int i = 0; i < NTHREADS; i++) {
		CHECK(pthread_join(th[i], NULL) == 0, "join");
	}

	for (int i = 0; i < NTHREADS; i++) {
		CHECK(slots[i].set_ret == slots[i].idx,
		      "the .so's setter returned what it stored");
		CHECK(slots[i].readback == slots[i].idx,
		      "a thread read back its OWN value through the descriptor");
		CHECK(slots[i].pad_ok,
		      "the aligned thread-local in the .so holds this thread's bytes");
		CHECK(slots[i].addr != NULL, "the descriptor returned an address");
	}
	/* THE discriminator: a descriptor that resolved to the initial thread's
	   block hands every thread the same pointer, and every check above still
	   passes because each thread wrote it last before the barrier. */
	CHECK(slots[0].addr != slots[1].addr,
	      "two threads got DIFFERENT dynamic TLS blocks");
	CHECK(slots[0].pad != slots[1].pad,
	      "two threads got different aligned blocks");
	pthread_barrier_destroy(&all);

	if (failures == 0) {
		printf("TLSDESC-OK\n");
	}
	return failures == 0 ? 0 : 1;
}
`

const tlsdescFixture = "raptormark-tmp-tlsdesc:latest"

// buildTLSDescFixture compiles the pair in the builder image (which has gcc and
// runs natively on aarch64) and stages them onto debian:trixie-slim, whose
// glibc is newer -- the safe direction. Both images are local; nothing is
// fetched.
//
// ⚠️ The guest is installed at /usr/bin, not /bin, and that is a WORKAROUND for
// a discovery defect rather than a preference: on a merged-usr image (`/bin ->
// usr/bin`, i.e. every current Debian and Ubuntu) a `CMD ["/bin/tdesc"]`
// resolves to nothing and discovery reports `no seed resolved to an exec target
// (seeds: [])`. `inv.Links` records file symlinks only, so `canon` cannot walk
// a symlinked path COMPONENT. See .agents/docs/TODO.md.
//
// The build FAILS if the shared object carries no R_AARCH64_TLSDESC
// relocations. Without that assertion a toolchain change to the traditional
// dialect would leave this test green and testing general-dynamic instead, and
// the thing it exists to cover would silently stop being covered.
func buildTLSDescFixture(t *testing.T, ctx context.Context, dir string) {
	t.Helper()
	for name, src := range map[string]string{
		"tdesc_dso.c":  tlsdescDSOSrc,
		"tdesc_main.c": tlsdescMainSrc,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	df := `FROM ` + builderImage() + ` AS build
COPY tdesc_dso.c tdesc_main.c /tmp/
RUN gcc -O2 -shared -fPIC -o /tmp/libtdesc.so /tmp/tdesc_dso.c && \
    readelf -rW /tmp/libtdesc.so | grep -q TLSDESC && \
    gcc -O2 -o /tmp/tdesc /tmp/tdesc_main.c -L/tmp -ltdesc -Wl,-rpath,/usr/lib
FROM debian:trixie-slim
COPY --from=build /tmp/libtdesc.so /usr/lib/libtdesc.so
COPY --from=build /tmp/tdesc /usr/bin/tdesc
CMD ["/usr/bin/tdesc"]
`
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := dockerBuild(ctx, abs, tlsdescFixture); err != nil {
		t.Skipf("cannot build the TLSDESC fixture (needs debian:trixie-slim locally, and a gcc "+
			"that emits TLSDESC): %v\n%s", err, out)
	}
}

// TestTLSDescNativeBaseline is the oracle: two threads, two blocks, on Linux.
func TestTLSDescNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	buildTLSDescFixture(t, ctx, t.TempDir())

	out, err := dockerRunImage(ctx, tlsdescFixture)
	if err != nil {
		t.Errorf("native run failed: %v", err)
	}
	assertTLSDescPassed(t, out)
}

// TestTLSDescUnderEcvisor executes a TLSDESC descriptor in a fused image, which
// nothing had done before -- structural verification only, from 2026-08-09
// until it first ran on 2026-08-15.
//
// Reaching it took six fixes, none of them about TLSDESC: a fused dynamic glibc
// image could not create a thread at all. The default pthread attr, then
// _dl_tls_static_size/align, then BOTH ld.so hook installers, then rseq
// acceptance, then a new thread's TLS being zeroed rather than initialised from
// the image. The gate came off when the last of them landed. See tlsdescDSOSrc for why the two-thread shape is
// what makes it an observation rather than a smoke test.
func TestTLSDescUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	buildTLSDescFixture(t, ctx, dir)

	root, entry := discoverImage(t, ctx, tlsdescFixture, "/usr/bin/tdesc")
	fused, err := fuse.Fuse(entry.HostPath, fuse.Options{
		LibraryPaths: []string{
			filepath.Join(root, "lib"),
			filepath.Join(root, "usr/lib"),
			filepath.Join(root, "lib/aarch64-linux-gnu"),
			filepath.Join(root, "usr/lib/aarch64-linux-gnu"),
		},
	})
	if err != nil {
		t.Fatalf("fusing the TLSDESC guest: %v", err)
	}
	fusedPath := filepath.Join(dir, "tdesc.fused")
	if err := os.WriteFile(fusedPath, fused, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Logf("fused TLSDESC guest: %d bytes", len(fused))

	wasm := liftOne(t, ctx, img, dir, fusedPath, "tdesc")
	assertTLSDescPassed(t, runThreadGapsPhase(t, ctx, wasm))

	// The probe described in tlsdescMainSrc, from the SAME module: a guest that
	// supplies its own stacksize. Reported rather than asserted -- it exists to
	// localise a failure of the check above, and says nothing on its own.
	t.Run("explicit-attr-probe", func(t *testing.T) {
		out := runThreadGapsPhase(t, ctx, wasm, "explicit")
		for _, line := range strings.Split(out, "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "FAIL ") || strings.Contains(l, "glibc error") ||
				strings.Contains(l, "TLSDESC-OK") {
				t.Log(l)
			}
		}
	})
}

func assertTLSDescPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "TLSDESC-OK") {
		t.Errorf("guest did not reach TLSDESC-OK; full output:\n%s", out)
	}
}
