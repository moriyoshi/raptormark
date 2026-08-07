package e2e

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNginxServesRealSocketsUnderTheNodeHost is the only test in the tree where a
// translated guest is reached over a REAL TCP connection.
//
// Every other inbound test goes through the service worker: the browser hands
// request bytes to `InboundSockets` and nothing binds a port. So `bind`,
// `listen` and `accept` against an actual OS socket -- the `NodeSockets` backend
// -- had no coverage at all, and did not work.
//
// ⚠️ WHAT IT CAUGHT, AND WHY THE SYMPTOM POINTED ELSEWHERE. nginx calls
// `listen(2)` TWICE on the same socket: once in `ngx_open_listening_sockets` and
// again in `ngx_configure_listening_sockets`, to apply the configured backlog.
// Linux allows that -- a second listen only adjusts the backlog. The Node
// backend created a second `net.createServer()`, which could not bind because
// the first held the port, overwrote the working server with the failed one, and
// set a STICKY socket error. A socket carrying an error reports as permanently
// readable, so epoll woke nginx forever and every `accept4` returned the sticky
// error: 572 318 iterations in 90 s, and not one byte served.
//
// The visible symptom was three layers from the cause, and the errno made it
// worse -- ADDRINUSE reached the guest as `(5: I/O error)`, because the runtime
// collapsed the bind family into EIO.
func TestNginxServesRealSocketsUnderTheNodeHost(t *testing.T) {
	requireBrowserFixtures(t, "nginx.wasm", "nginx.img")
	node := requireNode(t)

	web, err := filepath.Abs(filepath.Join("..", "web"))
	if err != nil {
		t.Fatal(err)
	}

	// ⚠️ The guest's port comes from the SIDECAR boot record, which pins 8080 --
	// host argv cannot move it. So the test cannot pick a free port, and a
	// machine already using 8080 has to be told rather than left to fail as a
	// mysterious timeout.
	const port = 8080
	if ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port)); err != nil {
		t.Skipf("port %d is already in use, and the nginx sidecar pins it: %v", port, err)
	} else {
		_ = ln.Close()
	}

	ctx, cancel := context.WithTimeout(ctxFor(t), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, node, filepath.Join(web, "bin", "run.ts"),
		"--module", filepath.Join(web, "public", "nginx.wasm"),
		"--rootfs", filepath.Join(web, "public", "nginx.img"),
		"--reentrant", "--net-v1")
	cmd.Dir = web
	// ⚠️ KEEP THE OUTPUT. The guest is nginx, and when this fails the reason is
	// almost always in nginx's own error log -- `listen() ... failed` was exactly
	// that. A killed process with a discarded pipe leaves nothing to read.
	var mu sync.Mutex
	var buf strings.Builder
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		t.Fatalf("launching the Node host: %v", err)
	}
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := pr.Read(b)
			if n > 0 {
				mu.Lock()
				buf.Write(b[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	logs := func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = pw.Close()
	}()

	// Poll for a served response rather than for a log line: "nginx started" and
	// "nginx serves" are different claims, and only the second is the point.
	client := &http.Client{Timeout: 5 * time.Second}
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/nodehost"
	var body string
	var server string
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				break
			}
			time.Sleep(250 * time.Millisecond)
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			body = string(b)
			server = resp.Header.Get("Server")
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

	if body == "" {
		t.Fatalf("nginx never served a request over a real socket in 90s.\n"+
			"If the log below says `listen() ... failed`, the backend's second "+
			"listen is creating a second server instead of succeeding.\n%s", logs())
	}

	// ⚠️ ASSERT ON WHAT ONLY NGINX PRODUCES. The body is a string this repo put
	// in a config file, so a stub could return it. The `Server:` header is
	// nginx's own, and it can only be there if nginx parsed the request bytes
	// that crossed a real TCP connection.
	if !strings.HasPrefix(server, "nginx/") {
		t.Errorf("Server header is %q, not nginx's own; something other than the "+
			"guest answered.\n%s", server, logs())
	}
	if !strings.Contains(body, "RAPTORMARK-NGINX-OK") {
		t.Errorf("unexpected body %q\n%s", body, logs())
	}

	// The spin this test exists to prevent is silent when it is only slow, so
	// name it explicitly. Serving does not by itself prove it is gone: the old
	// failure logged one alert per event-loop turn while never serving, but a
	// PARTIAL regression could do both.
	if n := strings.Count(logs(), "accept4() failed"); n > 0 {
		t.Errorf("nginx logged %d accept4 failures; the listener is reporting "+
			"readiness it cannot satisfy.\n%s", n, logs())
	}
	if strings.Contains(logs(), "listen() to") {
		t.Errorf("nginx reported a failed listen, which it then ignores -- so the "+
			"socket it is serving from is not the one it configured.\n%s", logs())
	}
	t.Logf("served %q from %s over a real TCP socket", strings.TrimSpace(body), server)
}
