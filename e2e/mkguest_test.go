package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"raptormark/internal/rootfs"
)

// Builds a browser-profile guest into web/public/ for the Playwright suite.
func TestBuildBrowserFixture(t *testing.T) {
	if os.Getenv("RAPTORMARK_BUILD_BROWSER_FIXTURE") != "1" {
		t.Skip("set RAPTORMARK_BUILD_BROWSER_FIXTURE=1")
	}
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	elf := compileGuest(t, ctx, dir, "webguest", fmt.Sprintf(guestSrc, "BROWSER-OK"))
	wasm := liftOne(t, ctx, img, dir, elf, "webguest", "--profile", "browser")

	// A second fixture that SLEEPS. The banner guest never blocks, so it proves
	// the module boots but says nothing about whether the tab stays responsive
	// -- which is the entire point of the re-entrant driver.
	sleepElf := compileGuest(t, ctx, dir, "websleep", sleepGuestSrc)
	sleepWasm := liftOne(t, ctx, img, dir, sleepElf, "websleep", "--profile", "browser")

	root := t.TempDir()
	image, _, err := rootfs.Build(root, rootfs.Options{Boot: &rootfs.Boot{
		Argv: []string{"webguest"}, Cwd: "/",
	}})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join("..", "web", "public")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(wasm)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "guest.wasm"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "rootfs.img"), image, 0o644); err != nil {
		t.Fatal(err)
	}
	// A third fixture: resolve a name and exchange bytes with it. In a browser
	// that path only completes through the relay, so this is what makes the
	// relay's existence checkable rather than asserted.
	netElf := compileGuest(t, ctx, dir, "webnet", dnsGuestSrc)
	netWasm := liftOne(t, ctx, img, dir, netElf, "webnet", "--profile", "browser")
	nb, err := os.ReadFile(netWasm)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "net.wasm"), nb, 0o644); err != nil {
		t.Fatal(err)
	}

	// A fourth fixture: an HTTP SERVER. Inbound is the one direction no other
	// fixture exercises -- `bind`/`listen`/`accept` and a process that parks in
	// `accept` rather than running to completion.
	httpdElf := compileGuest(t, ctx, dir, "webhttpd", httpdGuestSrc)
	httpdWasm := liftOne(t, ctx, img, dir, httpdElf, "webhttpd", "--profile", "browser")
	hb, err := os.ReadFile(httpdWasm)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "httpd.wasm"), hb, 0o644); err != nil {
		t.Fatal(err)
	}

	// A fifth fixture: a server that is ALSO a client. It is the only one that
	// can tell a composite backend from one that routes every socket to the same
	// place, because serving and dialling are different transports in a browser.
	bothElf := compileGuest(t, ctx, dir, "webboth", bothGuestSrc)
	bothWasm := liftOne(t, ctx, img, dir, bothElf, "webboth", "--profile", "browser")
	bb, err := os.ReadFile(bothWasm)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "both.wasm"), bb, 0o644); err != nil {
		t.Fatal(err)
	}

	// A sixth fixture: a server that answers and NEVER closes. It is the only
	// one that can tell "the response was framed" from "the guest hung up",
	// because a guest that closes satisfies both.
	kaElf := compileGuest(t, ctx, dir, "webka", kaGuestSrc)
	kaWasm := liftOne(t, ctx, img, dir, kaElf, "webka", "--profile", "browser")
	kb, err := os.ReadFile(kaWasm)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "ka.wasm"), kb, 0o644); err != nil {
		t.Fatal(err)
	}

	sb, err := os.ReadFile(sleepWasm)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "sleep.wasm"), sb, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s: guest.wasm (%d), sleep.wasm (%d), net.wasm (%d), httpd.wasm (%d), both.wasm (%d), ka.wasm (%d), rootfs.img (%d)",
		out, len(b), len(sb), len(nb), len(hb), len(bb), len(kb), len(image))
}
