package serve

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestWasmIsServedAsApplicationWasm is the one that matters.
//
// ⚠️ `WebAssembly.compileStreaming` REJECTS any other Content-Type, and the
// browser's error names the MIME type rather than the server -- so a wrong
// header reads like a corrupt module. Go's `http.FileServer` guesses from the
// extension against the platform's MIME database, which commonly yields
// `application/octet-stream` for `.wasm`, so the correct type has to be set
// rather than relied upon.
func TestWasmIsServedAsApplicationWasm(t *testing.T) {
	dir := t.TempDir()
	// A real wasm preamble, so nothing can pass by serving an empty file.
	if err := os.WriteFile(filepath.Join(dir, "m.wasm"),
		[]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0, 0, 0}, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	Handler(dir).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/m.wasm", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/wasm" {
		t.Errorf("Content-Type is %q, not application/wasm. compileStreaming will "+
			"refuse this module and blame its MIME type, not this server.", got)
	}
	if rec.Body.Len() != 8 {
		t.Errorf("body is %d bytes, want 8", rec.Body.Len())
	}
}

// The header must not be applied indiscriminately: a page served as
// application/wasm does not render.
func TestOtherTypesAreLeftAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<p>hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	Handler(dir).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/index.html", nil))
	if ct := rec.Header().Get("Content-Type"); ct == "application/wasm" {
		t.Errorf("index.html served as %q; a page with that type does not render", ct)
	}
}

// ⚠️ COOP/COEP must be ABSENT. They are only needed for SharedArrayBuffer, and
// the browser profile uses none -- ecvisor is single-threaded and its module
// declares a non-shared memory. Setting them would be cargo-culting and would
// break embedding the page alongside cross-origin assets.
func TestNoCrossOriginIsolationHeaders(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	Handler(dir).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/a.txt", nil))
	for _, h := range []string{"Cross-Origin-Opener-Policy", "Cross-Origin-Embedder-Policy"} {
		if v := rec.Header().Get(h); v != "" {
			t.Errorf("%s is set to %q; the browser profile needs no cross-origin "+
				"isolation, and requiring it narrows where the page can be hosted", h, v)
		}
	}
}

// TestServeWidensServiceWorkerScope guards the header that lets the bundle live
// under `dist/` at all.
//
// ⚠️ A SERVICE WORKER'S MAXIMUM SCOPE IS THE DIRECTORY ITS SCRIPT WAS SERVED
// FROM. `web/dist/raptormark.js` is registered with scope `/` so it can
// intercept `/_guest/*`, and the ONLY thing that permits that is
// `Service-Worker-Allowed` on the script's own response. Without it the
// registration is rejected with a message naming the scope and not the header,
// and the page then loads, activates a worker, and silently intercepts nothing.
//
// WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? The browser suite would
// catch it, at ten minutes and with the failure attributed to the guest. This
// catches it in milliseconds, in the default Go gate, with no browser.
func TestServeWidensServiceWorkerScope(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dist", "raptormark.js"),
		[]byte("export const x = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	Handler(dir).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/dist/raptormark.js", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d", rec.Code)
	}
	if got := rec.Header().Get("Service-Worker-Allowed"); got != "/" {
		t.Errorf("Service-Worker-Allowed is %q, want \"/\". The bundle is served "+
			"from /dist/, so without this header its worker can only control "+
			"/dist/* and will never see the /_guest/* requests it exists for.", got)
	}
	// It is a JavaScript MIME type too, or registration fails before scope is
	// ever considered.
	if got := rec.Header().Get("Content-Type"); got != "text/javascript; charset=utf-8" {
		t.Errorf("Content-Type is %q; a service worker script must be served as "+
			"JavaScript", got)
	}
}

// The scope header must not be sprayed onto everything: it is meaningless on a
// non-script and an unexpected header is a thing to explain later.
func TestServiceWorkerScopeHeaderIsScriptOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<p>hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	Handler(dir).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/index.html", nil))
	if got := rec.Header().Get("Service-Worker-Allowed"); got != "" {
		t.Errorf("Service-Worker-Allowed is %q on an HTML page; it belongs on the "+
			"worker SCRIPT response and nowhere else", got)
	}
}
