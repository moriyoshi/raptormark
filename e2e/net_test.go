package e2e

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os/exec"
	"testing"
	"time"
)

// The three socket guests, reconstructed from the surviving `raptormark-test-
// net*` fixture images (2026-07-18) by disassembling their `main`. Those images
// are unreproducible artifacts of the lost tree -- the package doc explains why
// nothing here may depend on them -- so the recovery is the C source, verified
// against the binaries it came from rather than trusted.
//
// Equivalence was established before these were written down: each source was
// compiled and run against the same peer as its original, and all three matched
// on stdout, exit status, AND syscall sequence. For `netforkserver` that
// sequence is, in both:
//
//	parent: socket bind listen getsockname clone accept read write wait4 close close write
//	child:  socket connect write read close
//
// Together they cover the three socket shapes a server image needs, which is why
// all three are worth keeping rather than just the richest one:
//
//   - netserver      inbound:  the guest listens, something outside connects
//   - netclient      outbound: the guest connects to something outside
//   - netforkserver  both ends inside one guest, across a fork
//
// netforkserver is the one that matters most for nginx: fork plus a loopback
// connection between parent and child is the master/worker shape, and it needs
// nothing outside the guest to run.
//
// The ports are the originals' (47826 inbound, 47825 outbound) and are fixed
// rather than ephemeral, because the guest's whole job is to be found at a known
// address by a peer it does not coordinate with. netforkserver binds port 0 and
// recovers the assignment with getsockname, so it never collides.
const (
	netServerSrc = `#include <arpa/inet.h>
#include <netinet/in.h>
#include <stdio.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

int main(void) {
	int fd = socket(AF_INET, SOCK_STREAM, 0);
	if (fd < 0) { perror("socket"); return 1; }
	int one = 1;
	setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof one);

	struct sockaddr_in a;
	memset(&a, 0, sizeof a);
	a.sin_family = AF_INET;
	a.sin_port = htons(47826);
	a.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
	if (bind(fd, (struct sockaddr *)&a, sizeof a) != 0) { perror("bind"); return 1; }
	if (listen(fd, 1) != 0) { perror("listen"); return 1; }

	int c = accept(fd, NULL, NULL);
	if (c < 0) { perror("accept"); return 1; }
	char buf[64];
	ssize_t n = read(c, buf, sizeof buf);
	if (n < 0) { perror("read"); return 1; }
	if (write(c, "pong", 4) != 4) { perror("write"); return 1; }
	for (ssize_t off = 0; off < n;) {
		ssize_t w = write(1, buf + off, n - off);
		if (w < 0) { perror("write stdout"); return 1; }
		off += w;
	}
	write(1, "\n", 1);
	close(c);
	close(fd);
	return 0;
}
`

	netClientSrc = `#include <arpa/inet.h>
#include <netinet/in.h>
#include <stdio.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

int main(void) {
	int fd = socket(AF_INET, SOCK_STREAM, 0);
	if (fd < 0) { perror("socket"); return 1; }

	struct sockaddr_in a;
	memset(&a, 0, sizeof a);
	a.sin_family = AF_INET;
	a.sin_port = htons(47825);
	a.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
	if (connect(fd, (struct sockaddr *)&a, sizeof a) != 0) { perror("connect"); return 1; }

	const char *msg = "ping";
	if (write(fd, msg, 4) != 4) { perror("write"); return 1; }
	char buf[64];
	ssize_t n = read(fd, buf, sizeof buf);
	if (n < 0) { perror("read"); return 1; }
	for (ssize_t off = 0; off < n;) {
		ssize_t w = write(1, buf + off, n - off);
		if (w < 0) { perror("write stdout"); return 1; }
		off += w;
	}
	close(fd);
	return 0;
}
`

	netForkServerSrc = `#include <arpa/inet.h>
#include <netinet/in.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/wait.h>
#include <unistd.h>

int main(void) {
	int fd = socket(AF_INET, SOCK_STREAM, 0);
	if (fd < 0) return 1;

	struct sockaddr_in a;
	memset(&a, 0, sizeof a);
	a.sin_family = AF_INET;
	a.sin_port = 0;
	a.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
	if (bind(fd, (struct sockaddr *)&a, sizeof a) != 0) return 1;
	if (listen(fd, 1) != 0) return 1;

	struct sockaddr_in bound;
	socklen_t blen = sizeof bound;
	if (getsockname(fd, (struct sockaddr *)&bound, &blen) != 0) return 1;

	pid_t pid = fork();
	if (pid < 0) return 1;

	if (pid == 0) {
		int c = socket(AF_INET, SOCK_STREAM, 0);
		if (c < 0) _exit(11);
		struct sockaddr_in p;
		memset(&p, 0, sizeof p);
		p.sin_family = AF_INET;
		p.sin_port = bound.sin_port;
		p.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
		if (connect(c, (struct sockaddr *)&p, sizeof p) != 0) _exit(12);
		if (write(c, "ping", 4) != 4) _exit(13);
		char rb[16];
		ssize_t rn = read(c, rb, sizeof rb);
		close(c);
		_exit(rn == 4 && memcmp(rb, "pong", 4) == 0 ? 0 : 14);
	}

	int c = accept(fd, NULL, NULL);
	if (c < 0) return 1;
	char buf[16];
	ssize_t n = read(c, buf, sizeof buf);
	if (write(c, "pong", 4) != 4) return 1;

	int st = 0;
	waitpid(pid, &st, 0);
	close(c);
	close(fd);

	if (n == 4 && memcmp(buf, "ping", 4) == 0 && WIFEXITED(st) && WEXITSTATUS(st) == 0) {
		write(1, "ok\n", 3);
		return 0;
	}
	return 1;
}
`
)

// TestNetGuestsNativeContract pins what the three guests do when nothing is
// emulated: compiled by the builder image's gcc and run natively on this host.
//
// This is deliberately not an ecvisor test. It is the baseline an ecvisor run
// has to reproduce, and it is the half of the pair that can be trusted -- if it
// ever disagrees with the assertions below, the reconstruction drifted, not the
// runtime. Running the lifted modules needs syscalls the runtime does not have
// yet (socketpair/sendmsg/recvmsg are absent; see .agents/docs/JOURNAL.md),
// so the ecvisor side is added when they land.
//
// The guests are static aarch64 ELFs and this host is aarch64, so they run
// directly. The peers are in-process Go, which keeps the ports on the loopback
// interface of the test machine and out of any container's network namespace.
func TestNetGuestsNativeContract(t *testing.T) {
	requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	t.Run("forkserver", func(t *testing.T) {
		// Self-contained: both ends of the connection live in one guest, either
		// side of a fork. No peer, so nothing here can be flaky for a reason
		// outside the guest.
		bin := compileGuest(t, ctx, dir, "netforkserver", netForkServerSrc)
		out, code := runGuest(t, ctx, bin, nil)
		if code != 0 || string(out) != "ok\n" {
			t.Errorf("netforkserver: exit=%d stdout=%q, want exit=0 stdout=%q", code, out, "ok\n")
		}
	})

	t.Run("server_inbound", func(t *testing.T) {
		// The guest listens on 47826; we connect, send, and expect "pong" back
		// plus the request echoed to its stdout.
		bin := compileGuest(t, ctx, dir, "netserver", netServerSrc)
		var reply []byte
		out, code := runGuest(t, ctx, bin, func() {
			c := dialWithRetry(t, "127.0.0.1:47826")
			defer c.Close()
			if _, err := c.Write([]byte("hello")); err != nil {
				t.Errorf("writing to netserver: %v", err)
				return
			}
			reply = readN(t, c, 4)
		})
		if code != 0 {
			t.Errorf("netserver exited %d, want 0 (stdout %q)", code, out)
		}
		if string(reply) != "pong" {
			t.Errorf("netserver replied %q, want %q", reply, "pong")
		}
		if string(out) != "hello\n" {
			t.Errorf("netserver stdout = %q, want %q", out, "hello\n")
		}
	})

	t.Run("client_outbound", func(t *testing.T) {
		// The guest connects out to 47825, sends "ping", and prints our reply.
		bin := compileGuest(t, ctx, dir, "netclient", netClientSrc)
		ln, err := net.Listen("tcp", "127.0.0.1:47825")
		if err != nil {
			t.Fatalf("listening for netclient: %v", err)
		}
		defer ln.Close()

		var got []byte
		out, code := runGuest(t, ctx, bin, func() {
			c, err := ln.Accept()
			if err != nil {
				t.Errorf("accepting from netclient: %v", err)
				return
			}
			defer c.Close()
			got = readN(t, c, 4)
			if _, err := c.Write([]byte("pong")); err != nil {
				t.Errorf("replying to netclient: %v", err)
			}
		})
		if code != 0 {
			t.Errorf("netclient exited %d, want 0 (stdout %q)", code, out)
		}
		if string(got) != "ping" {
			t.Errorf("netclient sent %q, want %q", got, "ping")
		}
		if string(out) != "pong" {
			t.Errorf("netclient stdout = %q, want %q", out, "pong")
		}
	})
}

// runGuest starts bin, runs peer (if any) while it is alive, and returns its
// stdout and exit status. A guest that never exits is killed by the context
// rather than hanging the suite.
func runGuest(t *testing.T, ctx context.Context, bin string, peer func()) ([]byte, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", bin, err)
	}
	if peer != nil {
		peer()
	}
	err := cmd.Wait()
	if stderr.Len() > 0 {
		t.Logf("%s stderr: %s", bin, stderr.String())
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return stdout.Bytes(), ee.ExitCode()
	}
	if err != nil {
		t.Fatalf("waiting for %s: %v", bin, err)
	}
	return stdout.Bytes(), 0
}

// dialWithRetry connects once the guest has reached its listen(2). There is no
// readiness signal to wait on, so retry until the timeout rather than sleeping a
// guessed amount.
func dialWithRetry(t *testing.T, addr string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("connecting to %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readN(t *testing.T, c net.Conn, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if err := c.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Errorf("reading %d bytes: %v", n, err)
		return nil
	}
	return buf
}
