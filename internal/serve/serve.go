// Package serve hosts the browser embedder and the artifacts it loads.
//
// It exists because a raptormark module has requirements a generic static
// server does not meet, and each failure is opaque from the browser side.
package serve

import (
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"

	"raptormark/internal/relay"
)

// Handler serves `root` with the headers a raptormark page needs.
//
// ⚠️ `Content-Type: application/wasm` IS LOAD-BEARING.
// `WebAssembly.compileStreaming` rejects any other type outright, and the error
// it raises ("Incorrect response MIME type") names the MIME type but not the
// server, so it reads like a corrupt artifact. Go's own `http.FileServer`
// guesses from the extension and, depending on the platform's MIME database,
// frequently guesses `application/octet-stream`.
//
// ⚠️ NO COOP/COEP HEADERS, deliberately. Cross-origin isolation is only needed
// for `SharedArrayBuffer`, and the browser profile uses none: ecvisor is
// single-threaded by construction and its module declares a NON-shared memory.
// Setting them anyway would be cargo-culting, and would break embedding the page
// anywhere that serves cross-origin assets.
func Handler(root string) http.Handler {
	fs := http.FileServer(http.Dir(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.ToLower(filepath.Ext(r.URL.Path)) {
		case ".wasm":
			w.Header().Set("Content-Type", "application/wasm")
		case ".js":
			// ⚠️ A SERVICE WORKER SCRIPT IS REJECTED unless its type is a
			// JavaScript MIME type, and the registration error names the type
			// but not the file. Go's guess comes from the platform's MIME
			// database, which is not guaranteed to have `.js` at all.
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			// ⚠️ WITHOUT THIS HEADER THE SERVICE WORKER CANNOT SEE THE PAGE.
			//
			// A worker's maximum scope defaults to the DIRECTORY ITS SCRIPT WAS
			// SERVED FROM, so `dist/raptormark.js` could only ever control
			// `/dist/*` -- and the requests it exists to intercept are
			// `/_guest/*`. `Service-Worker-Allowed` is the only mechanism that
			// widens it; the alternative is to serve the bundle from the origin
			// root, which is what this tree did until the artifact moved back
			// under `dist/`.
			//
			// The browser reads it from the response to the SCRIPT fetch, not
			// from the page, so it has to be set here rather than by the
			// embedder. Registering with a scope wider than this allows fails
			// with a message that names the scope but not the missing header.
			//
			// Set on every `.js` rather than on one hardcoded path: this server
			// exists to host `web/`, every script it serves is part of that
			// embedder, and a path constant here would silently stop matching
			// the first time the bundle is renamed.
			w.Header().Set("Service-Worker-Allowed", "/")
		case ".img":
			// The RAPTORFS sidecar. Its contents are already per-file
			// DEFLATEd by internal/rootfs, so there is little left for
			// transfer compression to take.
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		// Artifacts are content-addressed by the caller's `?v=` cache key, and
		// a stale module is the worst failure here: silent, and it looks like a
		// rebuild did not take.
		w.Header().Set("Cache-Control", "no-cache")
		fs.ServeHTTP(w, r)
	})
}

// Listen starts a server on `addr` and returns it with the address it actually
// bound, so a caller that asked for port 0 can find out which port it got.
func Listen(addr, root string) (*http.Server, net.Listener, error) {
	return ListenWithRelay(addr, root, nil)
}

// ListenWithRelay is Listen plus the WebSocket-to-TCP relay at /relay.
//
// A nil config means NO relay is mounted at all -- not a relay that refuses.
// The distinction matters: a page that finds nothing at /relay fails to connect
// and says so, whereas one that connects and is then refused every destination
// looks like a network problem.
func ListenWithRelay(addr, root string, rc *relay.Config) (*http.Server, net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("serve: %w", err)
	}
	h := Handler(root)
	if rc != nil {
		mux := http.NewServeMux()
		mux.Handle("/relay", relay.Handler(*rc))
		mux.Handle("/", h)
		h = mux
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	return srv, ln, nil
}
