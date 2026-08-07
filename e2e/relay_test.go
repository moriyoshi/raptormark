package e2e

import (
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"raptormark/internal/relay"
	"raptormark/internal/serve"
)

// TestRelayCarriesTCPFromABrowser is the last link in the chain: a guest in a
// tab reaching a TCP server it could not otherwise touch.
//
// ⚠️ A BROWSER CANNOT OPEN A TCP SOCKET, and `fetch` cannot stand in -- a
// cross-origin request to a server that has not opted into CORS rejects with an
// error that cannot even be reported accurately, and `Set-Cookie` is never
// visible to JavaScript at any setting. So this path is the only one a guest has,
// and until now the client half (`web/src/browser/relay.ts`) had nothing to talk
// to.
//
// The guest resolves a NAME, which only the in-runtime DNS tap can answer, and
// connects to the synthetic address it gets back -- which only the address pool
// can reverse, and only the relay can then dial. Every layer has to work for the
// exchange to complete, which is why the assertion is on the bytes rather than
// on any one of them.
func TestRelayCarriesTCPFromABrowser(t *testing.T) {
	web := requireBrowserSuite(t)
	if _, err := os.Stat(web + "/public/net.wasm"); err != nil {
		t.Skip("web/public/net.wasm is missing; rebuild the browser fixtures")
	}

	// The destination. It is on loopback, which a browser could not reach on the
	// guest's behalf by any other means.
	target := pingPongServer(t)

	// Serve the page AND the relay from ONE origin, which is also the origin the
	// relay then permits. The port is chosen first because the config has to
	// name the origin before the server exists -- and a relay that accepts any
	// origin is not a relay this test should be exercising.
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	srv, _, err := serve.ListenWithRelay("127.0.0.1:"+port, web, &relay.Config{
		// Only what this test needs. An empty list refuses everything, and a
		// wildcard would make the test prove less than it claims.
		Allow:   []string{"localhost:" + target},
		Origins: []string{base},
		Log:     func(f string, a ...any) { t.Logf(f, a...) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	t.Logf("serving %s with a relay allowing localhost:%s", base, target)

	cmd := exec.CommandContext(ctxFor(t), "npx", "playwright", "test", "tests/relay.spec.ts")
	cmd.Dir = web
	cmd.Env = append(os.Environ(),
		"RAPTORMARK_BASE_URL="+base,
		"RAPTORMARK_RELAY_URL=ws://127.0.0.1:"+port+"/relay",
		"RAPTORMARK_TARGET_PORT="+target,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("playwright: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "passed") {
		t.Errorf("no passing tests:\n%s", out)
	}
	t.Logf("%s", out)
}

// freePort returns a port nothing is listening on.
//
// Racy in principle and fine here: the window is microseconds and the
// alternative -- binding first -- cannot work, because the relay's origin
// allowlist has to name the address before the server is created.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	l.Close()
	return p
}
