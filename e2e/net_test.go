package e2e

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
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
// on stdout, exit status, AND syscall sequence.
//
// ❗ **THAT EQUIVALENCE NO LONGER DESCRIBES TWO OF THE THREE. Corrected
// 2026-08-25.** `netserver` and `netclient` were deliberately changed to
// coordinate their ports (see below), so the current sources are NOT the
// verified reconstructions any more: `netserver` gained a `getsockname` and a
// `printf` before its first `accept`, and `netclient` reads a port from argv.
// Their syscall sequences differ from the originals' by construction.
//
// ⚠️ This matters because the originals CANNOT BE RE-COMPARED -- the
// `raptormark-test-net*` images are unreproducible artifacts of the lost tree,
// and this tree lost two other such artifacts in one week. The fidelity
// evidence was spent, not renewed. `netforkserver` is untouched and its
// equivalence still stands, which is part of why it was the one lifted first.
//
// ✅ **AND IT IS NOW GUARDED RATHER THAN MERELY WRITTEN DOWN.**
// `TestNetForkServerMatchesItsRecordedSyscallSequence` (`netsyscall_test.go`)
// straces the native guest and compares both sequences below against the
// transcription, so an edit to `netForkServerSrc` fails and says what it is
// spending. Until 2026-08-25 the only thing protecting the last unspent
// reconstruction in this tree was this paragraph -- which is precisely the shape
// that lost `_recovery/` and the OpenSSL fixture in the same week.
//
// For `netforkserver` that sequence is, in both:
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
// ⚠️ **THE FIXED PORTS ARE GONE, on the user's decision, 2026-08-25.** This
// paragraph used to read: "The ports are the originals' (47826 inbound, 47825
// outbound) and are fixed rather than ephemeral, because the guest's whole job
// is to be found at a known address by a peer it does not coordinate with."
//
// That reason was true and was NOT what kept the ports fixed. `runGuest`
// buffered stdout and returned it only after `Wait()`, so an ephemeral port
// could not reach the peer in time -- the peer would have waited for a guest
// that was itself blocked in `accept()` waiting for the peer. The fixed ports
// were that limitation's consequence. `streamPeer` reads stdout WHILE the guest
// runs, and the constraint dissolved.
//
// All three now coordinate: `netserver` binds 0 and announces `PORT <n>`,
// `netclient` takes the port on argv and refuses to default, and
// `netforkserver` was always ephemeral. Nothing here collides with a concurrent
// run, which is what makes the lifted halves in `nethost_test.go` able to use
// `--network host` at all.
const (
	netServerSrc = `#include <arpa/inet.h>
#include <netinet/in.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

int main(int argc, char **argv) {
	int fd = socket(AF_INET, SOCK_STREAM, 0);
	if (fd < 0) { perror("socket"); return 1; }
	int one = 1;
	setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof one);

	/* ❗ THE PORT IS OPTIONAL, AND BOTH MODES ARE LOAD-BEARING.
	   With no argument the guest binds 0 and announces what it was given, so
	   nothing collides with a concurrent run -- that is what the coordinating
	   harnesses use.
	   With an explicit port it binds THAT, which is the only way to exercise the
	   ENCODE side of an address codec: bind(0) hands the port choice to the host
	   and so has no port to encode. wasixnet_test.go depends on this mode --
	   a byte-swapped encode makes the bind succeed on the swapped port and the
	   harness's dial time out. */
	int want_port = 0;
	if (argc > 1) {
		want_port = atoi(argv[1]);
		if (want_port <= 0 || want_port > 65535) { fprintf(stderr, "bad port %s\n", argv[1]); return 2; }
	}

	struct sockaddr_in a;
	memset(&a, 0, sizeof a);
	a.sin_family = AF_INET;
	a.sin_port = htons((unsigned short)want_port);
	a.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
	if (bind(fd, (struct sockaddr *)&a, sizeof a) != 0) { perror("bind"); return 1; }
	if (listen(fd, 1) != 0) { perror("listen"); return 1; }

	/* ❗ REPORT THE PORT BEFORE THE FIRST ACCEPT, and FLUSH.
	   The harness blocks on this line to learn where to connect, so a port
	   printed after accept() would deadlock: nobody would ever connect. And
	   stdout to a pipe is block-buffered, so without the flush the line sits in
	   libc until exit -- which is also a deadlock, and one that disappears the
	   moment you run the guest on a terminal. */
	struct sockaddr_in bound;
	socklen_t blen = sizeof bound;
	if (getsockname(fd, (struct sockaddr *)&bound, &blen) != 0) { perror("getsockname"); return 1; }
	printf("PORT %d\n", (int)ntohs(bound.sin_port));
	fflush(stdout);

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
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

int main(int argc, char **argv) {
	/* ❗ THE PORT COMES FROM argv, not from a constant. The harness listens on
	   an ephemeral port and passes it in, so two concurrent runs cannot collide.
	   Refusing to guess a default is deliberate: a default would let a
	   misconfigured run dial something plausible and fail somewhere else. */
	if (argc < 2) { fprintf(stderr, "usage: netclient <port>\n"); return 2; }
	int port = atoi(argv[1]);
	if (port <= 0 || port > 65535) { fprintf(stderr, "bad port %s\n", argv[1]); return 2; }

	int fd = socket(AF_INET, SOCK_STREAM, 0);
	if (fd < 0) { perror("socket"); return 1; }

	struct sockaddr_in a;
	memset(&a, 0, sizeof a);
	a.sin_family = AF_INET;
	a.sin_port = htons((unsigned short)port);
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
// runtime.
//
// ❗ **THE BLOCKER THIS USED TO NAME IS GONE.** It said "running the lifted
// modules needs syscalls the runtime does not have yet (socketpair/sendmsg/
// recvmsg are absent) ... so the ecvisor side is added when they land". They
// landed. Verified 2026-08-25: all three are dispatched in
// `runtime/src/sys.rs` (`NR_SOCKETPAIR`/`NR_SENDMSG`/`NR_RECVMSG` at :697-699)
// and implemented, not stubbed (`sys_socketpair` :4653, `sys_sendmsg` :4714,
// `sys_recvmsg` :4812); `e2e/uds_test.go` exercises guest AF_UNIX end to end.
//
// So the ecvisor half of this pair is now UNBUILT, not BLOCKED -- a different
// thing, and the reason to say so here is that the old wording reads as a
// standing impossibility and would keep anyone from trying. Whether it is worth
// building is an open question (`.agents/docs/TODO.md`, 2026-08-25): these three
// guests fork, bind fixed ports and talk to in-process Go peers, so an ecvisor
// side needs the port and peer arrangement rethought, not just a lift.
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
		// The guest listens on an EPHEMERAL port and announces it; we connect,
		// send, and expect "pong" back
		// plus the request echoed to its stdout.
		bin := compileGuest(t, ctx, dir, "netserver", netServerSrc)
		var reply []byte
		out, code := runGuestArgs(t, ctx, bin, nil, func(nextLine func() string) {
			// The guest prints `PORT <n>` once it is listening, which is both the
			// address AND the readiness signal -- dialWithRetry existed only
			// because a fixed-port guest gave no way to know when it was up.
			c := dialWithRetry(t, "127.0.0.1:"+portFrom(t, nextLine()))
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
		// The port line precedes the echo now, so the echo is checked as a
		// SUFFIX. ⚠️ Still exact after the port line, not a Contains: the guest
		// echoes exactly what it read, and a loose check would accept a partial
		// read.
		if _, echo, ok := strings.Cut(string(out), "\n"); !ok || echo != "hello\n" {
			t.Errorf("netserver stdout after the PORT line = %q, want %q", echo, "hello\n")
		}
	})

	t.Run("client_outbound", func(t *testing.T) {
		// The guest connects out to the port WE were assigned and passed in,
		// sends "ping", and prints our reply.
		bin := compileGuest(t, ctx, dir, "netclient", netClientSrc)
		// Port 0: the kernel picks, and the guest is TOLD. Nothing here is fixed,
		// so two concurrent runs cannot collide.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listening for netclient: %v", err)
		}
		defer ln.Close()
		_, port, err := net.SplitHostPort(ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}

		var got []byte
		out, code := runGuestArgs(t, ctx, bin, []string{port}, func(func() string) {
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
	return runGuestArgs(t, ctx, bin, nil, func(func() string) {
		if peer != nil {
			peer()
		}
	})
}

// runGuestArgs is runGuest with guest argv and a peer that can read the guest's
// stdout WHILE IT RUNS.
//
// ❗ WHY THE LINE READER EXISTS. The two coordinating guests need information to
// flow in both directions before either can finish: the server prints the
// ephemeral port it was assigned and the peer must connect to it, and a peer
// that waited for the process to exit would wait forever, because the guest is
// blocked in accept() waiting for that peer.
//
// `runGuest` buffered stdout into a `bytes.Buffer` and read it only after
// `Wait()`, which is why the old guests had to use FIXED ports -- there was no
// way to learn an ephemeral one in time. That constraint is gone; the fixed
// ports were its consequence, not its cause.
//
// `nextLine` blocks for the guest's next stdout line and fails the test on EOF
// rather than returning "", because an empty string would flow onward and
// produce a confusing failure somewhere else -- a guest that died before
// printing its port should say so here.
func runGuestArgs(t *testing.T, ctx context.Context, bin string, args []string,
	peer func(nextLine func() string)) ([]byte, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := streamPeer(t, ctx, cmd, peer)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return []byte(out), ee.ExitCode()
	}
	if err != nil {
		t.Fatalf("running %s: %v", bin, err)
	}
	return []byte(out), 0
}

// streamPeer starts cmd, hands `peer` a function that blocks for cmd's next
// stdout line, and returns everything cmd printed plus its wait error.
//
// Shared by the native runner and the lifted one (`nethost_test.go`) because the
// tricky parts are identical and easy to get subtly wrong in a second copy --
// two of them are recorded below as things this code got wrong first.
func streamPeer(t *testing.T, ctx context.Context, cmd *exec.Cmd,
	peer func(nextLine func() string)) (string, error) {
	t.Helper()
	pr, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %v: %v", cmd.Args, err)
	}

	// ⚠️ `io.TeeReader`, so `stdout` receives the process's EXACT BYTES while the
	// scanner reads lines from the same stream. Rebuilding stdout from scanned
	// lines appends a newline to every one -- and `netclient` prints "pong" with
	// NO trailing newline, so the existing assertion would break for a reason
	// that has nothing to do with the guest.
	lines := make(chan string, 64)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		defer close(lines)
		sc := bufio.NewScanner(io.TeeReader(pr, &stdout))
		for sc.Scan() {
			// ❗ NON-BLOCKING SEND. A peer that never calls nextLine must not stall
			// the process. The first version used `io.Pipe`, which is synchronous:
			// the forkserver's peer reads nothing, so the guest blocked on its
			// first write and the suite hung rather than failed.
			select {
			case lines <- sc.Text():
			default:
			}
		}
	}()

	next := func() string {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatalf("no further stdout from %v; it exited or died before printing "+
					"what the harness was waiting for. stderr: %s", cmd.Args, stderr.String())
			}
			return l
		case <-ctx.Done():
			t.Fatalf("timed out waiting for stdout from %v. stderr: %s", cmd.Args, stderr.String())
			return ""
		}
	}
	if peer != nil {
		peer(next)
	}
	<-drained // the scanner must finish before Wait closes the pipe under it
	err = cmd.Wait()
	if stderr.Len() > 0 {
		t.Logf("%v stderr: %s", cmd.Args, stderr.String())
	}
	return stdout.String(), err
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

// portFrom parses the guest's `PORT <n>` announcement.
//
// ❌ It FATALS on anything else rather than returning a zero port. A zero would
// flow into a dial that fails for an unrelated-looking reason, which is the
// class of confusing failure the fixed-port arrangement used to produce.
func portFrom(t *testing.T, line string) string {
	t.Helper()
	f := strings.Fields(line)
	if len(f) != 2 || f[0] != "PORT" {
		t.Fatalf("expected the guest's first stdout line to be `PORT <n>`, got %q.\n"+
			"It announces its ephemeral port there; without it the harness has no "+
			"address to connect to.", line)
	}
	if n, err := strconv.Atoi(f[1]); err != nil || n <= 0 || n > 65535 {
		t.Fatalf("guest announced a nonsensical port %q", f[1])
	}
	return f[1]
}
