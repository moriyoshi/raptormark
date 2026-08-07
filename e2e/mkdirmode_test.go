package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"raptormark/internal/rootfs"
)

// mkdirModeGuestSrc covers the attributes a guest-created directory carries.
//
// `sys_mkdirat` used to discard its `mode` argument outright, which is
// invisible until something CHECKS -- and PostgreSQL does:
//
//	initdb -D /tmp/pgdata
//	initdb: error: data directory "/tmp/pgdata" has invalid permissions
//	Permissions should be u=rwx (0700) or u=rwx,g=rx (0750).
//
// on a directory initdb had just created with exactly 0700. The workaround was
// to pre-create PGDATA in the image so the mode came from the LOWER layer,
// which is why this went unnoticed: every postgres run so far had been handed a
// directory it did not have to make.
//
// `umask(0)` first. The runtime models no umask (`umask` reports a conventional
// 022 and ignores its argument), so the requested mode is recorded verbatim;
// clearing the mask is what makes the native baseline describe the same thing
// rather than something 022 happens not to touch.
//
// The paths are pid-unique because the native baseline shares a container /tmp
// with whatever else ran there, and an EEXIST would read as a mode failure.
const mkdirModeGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

static int failures = 0;
#define CHECK(cond, what) do { \
	if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } \
} while (0)

/* Returns 1 for a directory, 0 for anything else, -1 if the stat failed. */
static int attrs(const char *p, mode_t *m, uid_t *u, gid_t *g) {
	struct stat st;
	if (stat(p, &st) != 0) return -1;
	*m = st.st_mode & 07777;
	*u = st.st_uid;
	*g = st.st_gid;
	return S_ISDIR(st.st_mode) ? 1 : 0;
}

int main(void) {
	umask(0);
	char base[64], d700[96], d750[96], reuse[96];
	snprintf(base, sizeof base, "/tmp/mkm%d", (int)getpid());
	CHECK(mkdir(base, 0755) == 0, "mkdir the test base");
	snprintf(d700, sizeof d700, "%s/pgdata", base);
	snprintf(d750, sizeof d750, "%s/grp", base);
	snprintf(reuse, sizeof reuse, "%s/reused", base);

	mode_t m; uid_t u; gid_t g;

	/* PGDATA's mode, the case that failed. */
	errno = 0; CHECK(mkdir(d700, 0700) == 0, "mkdir 0700");
	CHECK(attrs(d700, &m, &u, &g) == 1, "stat the 0700 directory");
	CHECK(m == 0700, "mkdir(0700) reads back as 0700");
	printf("MODE=%04o UID=%u GID=%u EUID=%u\n", (unsigned)m, (unsigned)u,
	       (unsigned)g, (unsigned)geteuid());

	/* PostgreSQL accepts 0750 too, so a handler that forced 0700 would pass
	   the check above and still be wrong. */
	errno = 0; CHECK(mkdir(d750, 0750) == 0, "mkdir 0750");
	CHECK(attrs(d750, &m, &u, &g) == 1, "stat the 0750 directory");
	CHECK(m == 0750, "mkdir(0750) reads back as 0750");

	/* The other half of checkDataDir: st_uid must be the caller's. This half
	   already worked; it is here because a check that passes only when the
	   guest runs as root is not a check, which is why the sidecar variant of
	   this test sets a non-zero boot uid. */
	CHECK(attrs(d700, &m, &u, &g) == 1, "re-stat the 0700 directory");
	CHECK(u == geteuid(), "the new directory is owned by the caller");
	CHECK(g == getegid(), "the new directory's group is the caller's");

	/* A removed directory must not lend its mode to whatever reuses the path.
	   The overlay records the mode per PATH, so this is the failure that
	   recording one for every mkdir would otherwise introduce. */
	errno = 0; CHECK(mkdir(reuse, 0700) == 0, "mkdir the path to be reused");
	errno = 0; CHECK(rmdir(reuse) == 0, "rmdir it");
	int fd = open(reuse, O_RDWR | O_CREAT | O_TRUNC, 0644);
	CHECK(fd >= 0, "recreate the same path as a file");
	if (fd >= 0) close(fd);
	CHECK(attrs(reuse, &m, &u, &g) == 0, "the replacement is a file");
	CHECK(m == 0644, "the replacement does not inherit the directory's 0700");

	if (failures == 0) printf("MKDIR-OK\n");
	return failures == 0 ? 0 : 1;
}
`

// TestMkdirModeUnderEcvisor is the mode half. It runs without a boot record, so
// the guest is uid 0 and the ownership checks inside are trivially satisfied --
// TestMkdirModeUnderANonRootBootUID is what makes those mean anything.
func TestMkdirModeUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "mkdirmode", mkdirModeGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "mkdirmode")
	assertMkdirGuestPassed(t, runWasm(t, ctx, wasm))
}

// TestMkdirModeNativeBaseline runs the same guest on Linux, so the expectations
// describe mkdir rather than ecvisor's implementation of it.
func TestMkdirModeNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "mkdirmode", mkdirModeGuestSrc)

	out, err := dockerRun(ctx, []string{"-v", mustAbs(t, dir) + ":/w"}, "/w/mkdirmode")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertMkdirGuestPassed(t, out)
}

// TestMkdirModeUnderANonRootBootUID gives the module a boot record with a
// NON-ZERO uid -- the only configuration in which the guest's ownership checks
// are not vacuous, since without one the guest is uid 0 and `st_uid ==
// geteuid()` holds because both sides are zero.
//
// What it establishes: the mode survives under a boot record too, and a
// non-root guest sees itself as the owner of what it creates. What it does NOT
// establish is that the second part was ever broken -- it was not.
// `sys_newfstatat` has always reported `ctx.uid` rather than the overlay's
// placeholder, and measured against the pre-fix runtime this guest printed
// `MODE=0755 UID=1000 GID=1000 EUID=1000`: wrong mode, right owner. The test is
// kept because nothing else runs a lifted guest under a non-root boot uid, and
// that contract has a live consumer in PostgreSQL's checkDataDir.
//
// The asserted `UID=1000` is not decoration: it distinguishes "a non-root guest
// owns what it creates" from "the boot record was ignored and both sides are
// still zero".
func TestMkdirModeUnderANonRootBootUID(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "mkdirmode", mkdirModeGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "mkdirmode")

	// A near-empty rootfs: the sidecar is here for the boot record, not for a
	// filesystem. /tmp is real so the guest's directories land under something
	// the image also has, as they would in a real container.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0o777); err != nil {
		t.Fatal(err)
	}
	image, _, err := rootfs.Build(root, rootfs.Options{Boot: &rootfs.Boot{
		Argv: []string{"mkdirmode"},
		Cwd:  "/",
		UID:  1000,
		GID:  1000,
	}})
	if err != nil {
		t.Fatalf("building the rfs sidecar: %v", err)
	}
	// runWasmIn mounts the module's directory at /out and maps it to /, so the
	// sidecar has to sit beside the module.
	if err := os.WriteFile(filepath.Join(filepath.Dir(wasm), "rootfs.img"), image, 0o644); err != nil {
		t.Fatal(err)
	}

	out := runWasmIn(t, ctx, wasm, nil,
		[]string{"RAPTORMARK_ROOTFS=/rootfs.img"}, "/:/out")
	if !strings.Contains(out, "UID=1000") {
		t.Errorf("the guest did not run as the boot record's uid; full output:\n%s", out)
	}
	assertMkdirGuestPassed(t, out)
}

func assertMkdirGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "MKDIR-OK") {
		t.Errorf("guest did not reach MKDIR-OK; full output:\n%s", out)
	}
}
