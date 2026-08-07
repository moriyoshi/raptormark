package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"raptormark/internal/link"
	"raptormark/internal/translate"
)

// syscallGuestSrc exercises the syscalls added for nginx: socketpair with
// sendmsg/recvmsg fd passing, eventfd against epoll, mkdirat plus the ownership
// no-ops, sendfile, rt_sigsuspend as a signal-delivery boundary, and the
// setgroups/setgid/setuid privilege drop.
//
// It is one program rather than six because the interesting failures are in how
// they compose -- a passed fd has to work afterwards, an eventfd has to change
// what epoll reports, sigsuspend has to see a SIGCHLD raised by a fork. Each
// check names itself on failure, so a partial pass still says which one broke.
//
// The guest prints "SYSCALLS-OK" only when every check passed, and exits
// non-zero otherwise, so a silent regression cannot read as success.
const syscallGuestSrc = `#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <grp.h>
#include <signal.h>
#include <stdio.h>
#include <string.h>
#include <sys/epoll.h>
#include <sys/eventfd.h>
#include <sys/sendfile.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

static int failures = 0;
#define CHECK(cond, what) do { if (!(cond)) { printf("FAIL %s (errno=%d)\n", what, errno); failures++; } } while (0)

static volatile sig_atomic_t got_chld = 0;
static void on_chld(int s) { (void)s; got_chld = 1; }

/* socketpair + SCM_RIGHTS: nginx's master->worker channel. The received fd must
   be independently usable, which is the whole reason to pass it. */
static void test_channel(const char *dir) {
	int sv[2];
	CHECK(socketpair(AF_UNIX, SOCK_STREAM, 0, sv) == 0, "socketpair");

	char path[256];
	snprintf(path, sizeof path, "%s/payload", dir);
	int f = open(path, O_RDWR | O_CREAT | O_TRUNC, 0644);
	CHECK(f >= 0, "open payload");
	CHECK(write(f, "PAYLOAD", 7) == 7, "write payload");
	CHECK(lseek(f, 0, SEEK_SET) == 0, "lseek payload");

	struct iovec iov = { .iov_base = (void *)"CMD", .iov_len = 3 };
	union { char buf[CMSG_SPACE(sizeof(int))]; struct cmsghdr align; } u;
	memset(&u, 0, sizeof u);
	struct msghdr mh;
	memset(&mh, 0, sizeof mh);
	mh.msg_iov = &iov; mh.msg_iovlen = 1;
	mh.msg_control = u.buf; mh.msg_controllen = sizeof u.buf;
	struct cmsghdr *c = CMSG_FIRSTHDR(&mh);
	c->cmsg_level = SOL_SOCKET; c->cmsg_type = SCM_RIGHTS;
	c->cmsg_len = CMSG_LEN(sizeof(int));
	memcpy(CMSG_DATA(c), &f, sizeof(int));
	CHECK(sendmsg(sv[0], &mh, 0) == 3, "sendmsg");

	char rbuf[16];
	struct iovec riov = { .iov_base = rbuf, .iov_len = sizeof rbuf };
	union { char buf[CMSG_SPACE(sizeof(int))]; struct cmsghdr align; } ru;
	memset(&ru, 0, sizeof ru);
	struct msghdr rmh;
	memset(&rmh, 0, sizeof rmh);
	rmh.msg_iov = &riov; rmh.msg_iovlen = 1;
	rmh.msg_control = ru.buf; rmh.msg_controllen = sizeof ru.buf;
	ssize_t n = recvmsg(sv[1], &rmh, 0);
	CHECK(n == 3 && memcmp(rbuf, "CMD", 3) == 0, "recvmsg payload");

	int passed = -1;
	struct cmsghdr *rc = CMSG_FIRSTHDR(&rmh);
	CHECK(rc != NULL && rc->cmsg_level == SOL_SOCKET && rc->cmsg_type == SCM_RIGHTS,
	      "recvmsg SCM_RIGHTS header");
	if (rc) memcpy(&passed, CMSG_DATA(rc), sizeof(int));
	CHECK(passed >= 0, "received fd");

	char got[8] = {0};
	CHECK(passed >= 0 && read(passed, got, 7) == 7 && memcmp(got, "PAYLOAD", 7) == 0,
	      "read through passed fd");

	off_t off = 0;
	CHECK(sendfile(sv[0], f, &off, 7) == 7, "sendfile");
	CHECK(off == 7, "sendfile advanced offset");
	char sf[8] = {0};
	CHECK(read(sv[1], sf, 7) == 7 && memcmp(sf, "PAYLOAD", 7) == 0, "sendfile bytes arrived");

	close(passed); close(f); close(sv[0]); close(sv[1]);
	printf("channel done\n");
}

/* The same channel, but across a fork -- which is how nginx actually uses it,
   and the case the fd-refcount bookkeeping exists for. Each socketpair end holds
   a reference on BOTH pipe directions, so a fork that counts them wrong either
   leaks a reference (the peer never sees EOF) or drops one it never took (the
   peer sees EOF immediately). Neither shows up without a fork in the picture. */
static void test_channel_fork(const char *dir) {
	int sv[2];
	CHECK(socketpair(AF_UNIX, SOCK_STREAM, 0, sv) == 0, "socketpair fork");

	char path[256];
	snprintf(path, sizeof path, "%s/forked", dir);
	int f = open(path, O_RDWR | O_CREAT | O_TRUNC, 0644);
	CHECK(f >= 0, "open forked payload");
	CHECK(write(f, "ACROSS", 6) == 6, "write forked payload");
	CHECK(lseek(f, 0, SEEK_SET) == 0, "lseek forked payload");

	pid_t pid = fork();
	CHECK(pid >= 0, "fork for channel");
	if (pid == 0) {
		close(sv[0]);
		char rbuf[16];
		struct iovec riov = { .iov_base = rbuf, .iov_len = sizeof rbuf };
		union { char buf[CMSG_SPACE(sizeof(int))]; struct cmsghdr align; } ru;
		memset(&ru, 0, sizeof ru);
		struct msghdr rmh;
		memset(&rmh, 0, sizeof rmh);
		rmh.msg_iov = &riov; rmh.msg_iovlen = 1;
		rmh.msg_control = ru.buf; rmh.msg_controllen = sizeof ru.buf;
		if (recvmsg(sv[1], &rmh, 0) != 4) _exit(31);
		if (memcmp(rbuf, "OPEN", 4) != 0) _exit(32);
		struct cmsghdr *rc = CMSG_FIRSTHDR(&rmh);
		if (!rc || rc->cmsg_type != SCM_RIGHTS) _exit(33);
		int got = -1;
		memcpy(&got, CMSG_DATA(rc), sizeof(int));
		if (got < 0) _exit(34);
		char b[8] = {0};
		if (read(got, b, 6) != 6 || memcmp(b, "ACROSS", 6) != 0) _exit(35);
		/* The peer closing its end must be visible as EOF, not as a hang. */
		char eof[4];
		if (read(sv[1], eof, sizeof eof) != 0) _exit(36);
		_exit(0);
	}
	close(sv[1]);
	struct iovec iov = { .iov_base = (void *)"OPEN", .iov_len = 4 };
	union { char buf[CMSG_SPACE(sizeof(int))]; struct cmsghdr align; } u;
	memset(&u, 0, sizeof u);
	struct msghdr mh;
	memset(&mh, 0, sizeof mh);
	mh.msg_iov = &iov; mh.msg_iovlen = 1;
	mh.msg_control = u.buf; mh.msg_controllen = sizeof u.buf;
	struct cmsghdr *c = CMSG_FIRSTHDR(&mh);
	c->cmsg_level = SOL_SOCKET; c->cmsg_type = SCM_RIGHTS;
	c->cmsg_len = CMSG_LEN(sizeof(int));
	memcpy(CMSG_DATA(c), &f, sizeof(int));
	CHECK(sendmsg(sv[0], &mh, 0) == 4, "sendmsg across fork");
	close(sv[0]);
	close(f);

	int st = 0;
	CHECK(waitpid(pid, &st, 0) == pid, "waitpid channel");
	CHECK(WIFEXITED(st) && WEXITSTATUS(st) == 0, "child received fd across fork");
	if (WIFEXITED(st) && WEXITSTATUS(st) != 0)
		printf("  (child stage %d)\n", WEXITSTATUS(st));
	printf("channel-fork done\n");
}

static void test_eventfd(void) {
	int efd = eventfd(0, 0);
	CHECK(efd >= 0, "eventfd");
	int ep = epoll_create1(0);
	CHECK(ep >= 0, "epoll_create1");
	struct epoll_event ev = { .events = EPOLLIN, .data.fd = efd };
	CHECK(epoll_ctl(ep, EPOLL_CTL_ADD, efd, &ev) == 0, "epoll_ctl");

	struct epoll_event out[2];
	CHECK(epoll_wait(ep, out, 2, 0) == 0, "eventfd not ready before post");
	uint64_t one = 1;
	CHECK(write(efd, &one, 8) == 8, "eventfd write");
	CHECK(epoll_wait(ep, out, 2, 0) == 1, "eventfd ready after post");
	uint64_t v = 0;
	CHECK(read(efd, &v, 8) == 8 && v == 1, "eventfd read drains");
	CHECK(epoll_wait(ep, out, 2, 0) == 0, "eventfd not ready after drain");
	close(ep); close(efd);
	printf("eventfd done\n");
}

static void test_fs(const char *dir) {
	char sub[256];
	snprintf(sub, sizeof sub, "%s/temp", dir);
	CHECK(mkdir(sub, 0700) == 0, "mkdir");
	CHECK(mkdir(sub, 0700) == -1 && errno == EEXIST, "mkdir twice is EEXIST");

	char inner[300];
	snprintf(inner, sizeof inner, "%s/file", sub);
	int f = open(inner, O_RDWR | O_CREAT, 0644);
	CHECK(f >= 0, "create inside new dir");
	CHECK(fchown(f, 0, 0) == 0, "fchown");
	CHECK(fchmod(f, 0600) == 0, "fchmod");
	CHECK(fchownat(AT_FDCWD, inner, 0, 0, 0) == 0, "fchownat");
	close(f);
	printf("fs done\n");
}

/* The nginx master loop exactly: block SIGCHLD, fork, then sigsuspend until the
   handler has run. Without rt_sigsuspend as a delivery boundary this never
   returns -- the signal is posted and nothing ever hands it to the handler. */
static void test_sigsuspend(void) {
	struct sigaction sa;
	memset(&sa, 0, sizeof sa);
	sa.sa_handler = on_chld;
	CHECK(sigaction(SIGCHLD, &sa, NULL) == 0, "sigaction");

	sigset_t chld, old;
	sigemptyset(&chld); sigaddset(&chld, SIGCHLD);
	CHECK(sigprocmask(SIG_BLOCK, &chld, &old) == 0, "sigprocmask");

	pid_t pid = fork();
	CHECK(pid >= 0, "fork");
	if (pid == 0) _exit(7);

	sigset_t empty;
	sigemptyset(&empty);
	int rounds = 0;
	while (!got_chld && rounds < 100) { sigsuspend(&empty); rounds++; }
	CHECK(got_chld, "SIGCHLD handler ran");

	int st = 0;
	CHECK(waitpid(pid, &st, 0) == pid, "waitpid");
	CHECK(WIFEXITED(st) && WEXITSTATUS(st) == 7, "child status");
	CHECK(sigprocmask(SIG_SETMASK, &old, NULL) == 0, "sigprocmask restore");
	printf("sigsuspend done\n");
}

/* In a child, as nginx does it: the master stays root, workers drop. */
static void test_privdrop(void) {
	pid_t pid = fork();
	CHECK(pid >= 0, "fork for privdrop");
	if (pid == 0) {
		gid_t none = 0;
		if (setgroups(0, &none) != 0) _exit(21);
		if (setgid(65534) != 0) _exit(22);
		if (setuid(65534) != 0) _exit(23);
		if (getuid() != 65534 || getgid() != 65534) _exit(24);
		_exit(0);
	}
	int st = 0;
	CHECK(waitpid(pid, &st, 0) == pid, "waitpid privdrop");
	CHECK(WIFEXITED(st) && WEXITSTATUS(st) == 0, "privilege drop");
	printf("privdrop done\n");
}

int main(int argc, char **argv) {
	const char *dir = argc > 1 ? argv[1] : "/tmp";
	test_channel(dir);
	test_channel_fork(dir);
	test_eventfd();
	test_fs(dir);
	test_sigsuspend();
	test_privdrop();
	if (failures == 0) printf("SYSCALLS-OK\n"); else printf("SYSCALLS-FAILED %d\n", failures);
	return failures == 0 ? 0 : 1;
}
`

// TestNginxSyscallsNativeBaseline pins what the guest does with a real kernel,
// so the ecvisor result below has something to be compared against rather than
// merely inspected. It runs inside the builder image because several checks are
// root-only (fchown to uid 0, setuid) and the image runs as root -- exactly the
// identity a guest has under ecvisor.
func TestNginxSyscallsNativeBaseline(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()
	compileGuest(t, ctx, dir, "syscalls", syscallGuestSrc)

	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dockerRun(ctx, []string{"-v", abs + ":/w"},
		"mkdir -p /tmp/sd && /w/syscalls /tmp/sd")
	if err != nil {
		t.Fatalf("native run failed: %v\n%s", err, out)
	}
	assertSyscallGuestPassed(t, out)
}

// TestNginxSyscallsUnderEcvisor is the one that matters: the same program, lifted
// to wasm and run on ecvisor's syscall layer.
//
// Every check here failed with ENOSYS before the implementation, and ENOSYS is
// quiet -- it is logged only under RAPTORMARK_ECV_DEBUG, because glibc probes
// several harmlessly at startup. That is precisely why this test asserts on the
// guest's own verdict rather than on the absence of runtime complaints: a missing
// syscall does not announce itself, it just makes the guest take a worse path.
func TestNginxSyscallsUnderEcvisor(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "syscalls", syscallGuestSrc)
	wasm := liftOne(t, ctx, img, dir, elf, "syscalls")

	// The guest writes into its argv[1] directory, so the run needs a writable
	// path. The VFS overlays a tmpfs on the read-only sidecar, so /tmp is
	// writable even with no rootfs image mounted.
	out := runWasm(t, ctx, wasm)
	assertSyscallGuestPassed(t, out)
}

// assertSyscallGuestPassed reports the guest's own per-check failures, which name
// what broke, instead of only that the run did not end in SYSCALLS-OK.
func assertSyscallGuestPassed(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAIL ") {
			t.Errorf("guest check failed: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(out, "SYSCALLS-OK") {
		t.Errorf("guest did not reach SYSCALLS-OK; full output:\n%s", out)
	}
}

// liftOne runs a single static guest through translate + link and returns the
// module path. It is the shape TestEcvisorRuntime open-codes, factored out so a
// test that only wants "this binary, as wasm" does not repeat the ceremony.
func liftOne(t *testing.T, ctx context.Context, img, dir, elf, name string, extra ...string) string {
	t.Helper()
	b, err := translate.BuilderFromImage(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	opts := translate.Options{Runtime: "ecvisor"}
	sha, err := translate.FileSHA256(elf)
	if err != nil {
		t.Fatal(err)
	}
	moduleID := translate.ModuleID(elf, sha)
	prog := link.Program{Name: moduleID, Index: 0}

	frag := filepath.Join(dir, "frag_0.c")
	if err := os.WriteFile(frag, []byte(link.FragmentC(prog)), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	translateOne(t, ctx, b, translate.Request{
		ELF: elf, OutDir: outDir, ModuleID: moduleID,
		Fragment: frag, Keep: prog.Symbol(), Options: opts,
	})

	registry, err := link.RegistryC([]link.Program{prog})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "registry.c"), []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}
	return linkAll(t, ctx, dir, outDir, fmt.Sprintf("%s.wasm", name),
		[]string{"/out/" + moduleID + ".o"}, extra...)
}
