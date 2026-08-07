package e2e

import (
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"raptormark/internal/serve"
)

// The browser suite: a translated aarch64 guest running in a real browser.
//
// ⚠️ SEPARATELY GATED, and not by accident. `npx playwright install` downloads
// ~500 MB of browser builds and needs network -- the same reason the Docker E2E
// suite is opt-in -- and some engines additionally need system libraries that
// only root can install. So this needs `RAPTORMARK_E2E_BROWSER=1` ON TOP of
// `RAPTORMARK_E2E=1`, and skips with a diagnostic naming what is missing rather
// than failing on a machine that was never set up for it.
//
// It also needs artifacts built with `--profile browser`. `TestBuildBrowserFixture`
// writes them into `web/public/`; without them there is nothing to load, so the
// test skips rather than reporting a browser failure for a missing file.
func requireBrowserSuite(t *testing.T) string {
	t.Helper()
	if os.Getenv("RAPTORMARK_E2E_BROWSER") != "1" {
		t.Skip("set RAPTORMARK_E2E_BROWSER=1 to run the browser suite " +
			"(needs `npm install` and `npx playwright install` in web/)")
	}
	node := requireNode(t)
	web, err := filepath.Abs("../web")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(web, "node_modules", "@playwright", "test")); err != nil {
		t.Skipf("@playwright/test is not installed in %s; run `npm install` there", web)
	}
	if _, err := os.Stat(filepath.Join(web, "public", "guest.wasm")); err != nil {
		t.Skip("web/public/guest.wasm is missing; build it with " +
			"RAPTORMARK_BUILD_BROWSER_FIXTURE=1 go test ./e2e/ -run TestBuildBrowserFixture")
	}
	// ⚠️ ONE ARTIFACT, TWO ROLES. `web/dist/raptormark.js` is both the module the
	// pages import and the script registered as the service worker. A worker's
	// maximum scope defaults to the directory it was served from, so this one
	// reaches `/_guest/*` only because `internal/serve` sends
	// `Service-Worker-Allowed: /` -- see `TestServeWidensServiceWorkerScope`.
	bundle := filepath.Join(web, "dist", "raptormark.js")
	st, err := os.Stat(bundle)
	if err != nil {
		t.Skipf("web/dist/raptormark.js is missing; run `npm run build` in %s. A browser "+
			"cannot execute the TypeScript sources the Node host runs directly.", web)
	}
	// ⚠️ EXISTING IS NOT THE SAME AS CURRENT, and a stale artifact fails in the
	// most expensive possible way: every browser test times out waiting for a
	// guest that never boots, ~10 minutes of suite, and the reported error names
	// the guest rather than the build. That happened twice on 2026-08-21 -- once
	// when the bundle predated a renamed export, once mid-refactor -- and both
	// times only the existence check ran, and passed.
	if newest := newestSourceUnder(t, web); newest.After(st.ModTime()) {
		t.Fatalf("web/dist/raptormark.js is STALE (built %s, sources changed %s). Run "+
			"`npm run build` in %s -- the browser runs the bundle, not the "+
			"TypeScript the Node host reads.",
			st.ModTime().Format(time.RFC3339), newest.Format(time.RFC3339), web)
	}
	_ = node
	return web
}

// newestSourceUnder is the most recent mtime among the TypeScript the browser
// bundle is built from. It is one file serving both the page and the service
// worker, so one timestamp bounds it.
//
// ⚠️ `*.test.ts` IS EXCLUDED, and that is not a shortcut. esbuild emits only what
// is reachable from `src/browser/run.ts`, and a vitest file is reachable from
// nothing -- so a new unit test cannot change the bundle, and counting it would
// mark a perfectly current artifact stale and demand a rebuild that produces
// byte-identical output. Adding the vitest suite made that immediate: twenty new
// tests, none of them in the bundle, all of them newer than it.
func newestSourceUnder(t *testing.T, web string) time.Time {
	t.Helper()
	var newest time.Time
	for _, dir := range []string{"src", "bin"} {
		err := filepath.WalkDir(filepath.Join(web, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(p) != ".ts" {
				return err
			}
			if strings.HasSuffix(p, ".test.ts") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if info.ModTime().After(newest) {
				newest = info.ModTime()
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s/%s: %v", web, dir, err)
		}
	}
	return newest
}

// TestGuestRunsInABrowser is the end of the whole browser port: an aarch64 Linux
// program, translated ahead of time, running in a tab.
//
// The Go side owns the server -- `internal/serve`, the same one `raptormark
// serve` uses -- so the Content-Type that `compileStreaming` demands is the one
// under test rather than one Playwright happened to configure.
func TestGuestRunsInABrowser(t *testing.T) {
	web := requireBrowserSuite(t)

	srv, ln, err := serve.Listen("127.0.0.1:0", web)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	base := "http://127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	t.Logf("serving %s at %s", web, base)

	cmd := exec.CommandContext(ctxFor(t), "npx", "playwright", "test")
	cmd.Dir = web
	cmd.Env = append(os.Environ(),
		"RAPTORMARK_BASE_URL="+base,
		// Chromium only by default: the others need system libraries that only
		// root can install, and a skipped engine must not look like a failure.
		"RAPTORMARK_BROWSERS="+os.Getenv("RAPTORMARK_BROWSERS"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("playwright: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "passed") {
		t.Errorf("playwright reported no passing tests:\n%s", out)
	}
	t.Logf("%s", out)
}
