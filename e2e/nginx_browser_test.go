package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	mrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"raptormark/internal/image"
	"raptormark/internal/rootfs"
	"raptormark/internal/serve"
)

// nginxConf is as small as the question allows.
//
// ⚠️ `master_process off` -- ONE process, deliberately. nginx's normal shape is
// a master that forks workers, and ecvisor reconstructs a fork by full replay.
// That works under wasmedge, but nothing has ever driven it through the
// RE-ENTRANT scheduler, and combining an untried fork path with an untried
// server in one step would make any failure ambiguous. Workers are the next
// question, not this one.
//
// `return 200` needs no document root, no mime types and no index, so a failure
// is about the runtime rather than about the VFS serving a file. The BODY is
// what the test matches: a 200 with the wrong bytes is what silent corruption
// looks like.
const nginxConf = `daemon off;
master_process off;
error_log /dev/stderr notice;
pid /tmp/nginx.pid;
events {
    worker_connections 64;
}
http {
    access_log off;
    server {
        listen 8080;
        # The locations carry the /_guest/ prefix, because the service worker
        # forwards the path VERBATIM and does not rewrite it. That is the honest
        # arrangement -- nginx sees exactly the bytes the browser sent -- and it
        # is what lets the test assert the echoed path equals the requested one.
        # Matching on /echo instead silently fell through to location /, since
        # nginx takes the LONGEST matching prefix and /_guest/echo does not
        # begin with /echo.
        location /_guest/echo { return 200 "path=$request_uri host=$http_host\n"; }
        location /_guest/deny { return 403; }
        # $pid is emitted here too, and it is not decoration: it is what lets the
        # WORKER test's "not the master" assertion be neutralized against this
        # config. Without it that neutralization fails on a missing pid instead,
        # and the assertion that actually carries the claim stays unexercised.
        location / { return 200 "RAPTORMARK-NGINX-OK pid=$pid\n"; }
    }
}
`

// nginxWorkerConf is nginx in its NORMAL shape: a master that forks workers.
//
// ⚠️ THIS IS THE UNTRIED PATH. ecvisor reconstructs a fork by FULL REPLAY, which
// works under wasmedge but has never been driven through the re-entrant
// scheduler -- and a browser host returns to its event loop mid-replay, which a
// blocking host never does.
//
// $pid is what makes the result checkable. With `master_process off` every
// response comes from pid 1; here a response from pid 1 would mean the master
// served it itself and no worker was ever involved. The body carries the pid so
// the test can require otherwise.
const nginxWorkerConf = `daemon off;
worker_processes 2;
error_log /dev/stderr notice;
pid /tmp/nginx.pid;
events {
    worker_connections 64;
}
http {
    access_log off;
    server {
        listen 8080;
        location /_guest/echo { return 200 "path=$request_uri host=$http_host pid=$pid\n"; }
        location /_guest/deny { return 403; }
        location / { return 200 "RAPTORMARK-NGINX-OK pid=$pid\n"; }
    }
}
`

// nginxFilesConf serves real FILES from the sidecar, not a string from a config.
//
// ⚠️ THIS IS THE FIRST TIME THE VFS CARRIES THE REQUEST. Every nginx variant so
// far used `return 200`, which needs no document root -- so the sidecar was only
// ever read once, at startup, for nginx.conf. Here nginx must open, stat and
// read files out of RAPTORFS per request, resolve an index, and pick a content
// type from mime.types.
//
// `sendfile off` is EXPLICIT. sendfile(2) from a VFS file to a host socket is a
// different mechanism entirely, and mixing it into the first test of file
// serving would make any failure ambiguous. It gets its own fixture below.
const nginxFilesConf = `daemon off;
worker_processes 2;
error_log /dev/stderr notice;
pid /tmp/nginx.pid;
events {
    worker_connections 64;
}
http {
    include /etc/nginx/mime.types;
    default_type application/octet-stream;
    access_log off;
    sendfile off;
    server {
        listen 8080;
        location /_guest/ {
            alias /srv/www/;
            index index.html;
        }
    }
}
`

// nginxSendfileConf is the same, with sendfile(2) turned on -- which is what a
// real deployment does, and what the Alpine image ships.
const nginxSendfileConf = `daemon off;
worker_processes 2;
error_log /dev/stderr notice;
pid /tmp/nginx.pid;
events {
    worker_connections 64;
}
http {
    include /etc/nginx/mime.types;
    default_type application/octet-stream;
    access_log off;
    sendfile on;
    server {
        listen 8080;
        location /_guest/ {
            alias /srv/www/;
            index index.html;
        }
    }
}
`

// TestBuildNginxBrowserFixture lifts the fused nginx closure for the browser.
//
// ⚠️ IT HAS ITS OWN OPT-IN, above `RAPTORMARK_E2E_SLOW`. The fused nginx object
// is in the object cache and this is then a ~5-minute RELINK -- but if the cache
// key does not match, the same command is a SIX AND A HALF HOUR translation.
// (Cost follows the largest single function, not total volume: the far bigger
// OpenSSL closure takes 28 minutes.) A gate that can silently cost a working day
// does not belong behind a flag anyone sets casually.
//
// Watch the log: `served from the object cache` means the relink; anything else
// means it is translating, and should be stopped unless that is intended.
func TestBuildNginxBrowserFixture(t *testing.T) {
	if os.Getenv("RAPTORMARK_BUILD_NGINX_FIXTURE") != "1" {
		t.Skip("set RAPTORMARK_BUILD_NGINX_FIXTURE=1 (a cache miss here is ~6.5 hours)")
	}
	img := requireE2E(t)
	ctx := ctxFor(t)
	dir := t.TempDir()

	fused, err := filepath.Abs(filepath.Join("..", ".agents-workspace", "fixtures", "nginx-alpine.fused"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fused); err != nil {
		t.Skipf("no fused nginx at %s; it is an expensive fixture and is not rebuilt here", fused)
	}

	wasm := liftOne(t, ctx, img, dir, fused, "nginx", "--profile", "browser")

	// The sidecar. nginx needs a real filesystem -- its config above all -- and
	// the boot record is what supplies argv, because a re-entrant host has no
	// crt1 to take it from.
	root := t.TempDir()
	if err := image.ExportRootfs(ctx, "nginx:alpine", root); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"etc/nginx", "var/cache/nginx", "var/log/nginx", "run", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	conf := filepath.Join(root, "etc", "nginx", "nginx.conf")
	if err := os.Remove(conf); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(conf, []byte(nginxConf), 0o644); err != nil {
		t.Fatal(err)
	}

	// A document root. ⚠️ `data.bin` comes from a real PRNG with a fixed seed --
	// an arithmetic byte pattern looks random and is not, and it has silently
	// inverted the meaning of a byte-exactness check in this tree before.
	www := filepath.Join(root, "srv", "www")
	if err := os.MkdirAll(www, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(www, "index.html"),
		[]byte("<!doctype html>\n<title>from the VFS</title>\n<p>RAPTORMARK-VFS-INDEX</p>\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(www, "style.css"),
		[]byte("body { color: rebeccapurple } /* RAPTORMARK-VFS-CSS */\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, 64*1024)
	if _, err := mrand.New(mrand.NewSource(20260821)).Read(blob); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(www, "data.bin"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(blob)
	blobSHA := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join("..", "web", "public", "data.bin.sha256"),
		[]byte(blobSHA), 0o644); err != nil {
		t.Fatal(err)
	}

	build := func() []byte {
		t.Helper()
		img, _, err := rootfs.Build(root, rootfs.Options{Boot: &rootfs.Boot{
			Argv: []string{"nginx", "-c", "/etc/nginx/nginx.conf"},
			Cwd:  "/",
			Env:  []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return img
	}
	image := build()

	// ⚠️ ONE MODULE, TWO SIDECARS. The worker variant differs only in
	// nginx.conf, so it needs no second lift -- which is what makes trying
	// nginx's real process model cost a sidecar rebuild rather than a relink.
	if err := os.WriteFile(conf, []byte(nginxWorkerConf), 0o644); err != nil {
		t.Fatal(err)
	}
	workers := build()

	out := filepath.Join("..", "web", "public")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	wb, err := os.ReadFile(wasm)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "nginx.wasm"), wb, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "nginx.img"), image, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "nginx-workers.img"), workers, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct {
		conf, name string
	}{
		{nginxFilesConf, "nginx-files.img"},
		{nginxSendfileConf, "nginx-sendfile.img"},
	} {
		if err := os.WriteFile(conf, []byte(v.conf), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, v.name), build(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("wrote %s: nginx.wasm (%d), nginx.img (%d), nginx-workers.img (%d)",
		out, len(wb), len(image), len(workers))
}

// TestNginxServesFromABrowser is the claim this whole browser effort has been
// making and not backing: a REAL server, not a fixture written to be servable.
//
// ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? The body alone is not
// enough -- it is a string from a config file this repo wrote. So the test also
// requires the `Server: nginx/...` response header, which nginx generates and
// nothing here supplies, and it requires nginx's own `$request_uri` and
// `$http_host` variables to come back with the values the browser sent. Those
// can only be produced by nginx having parsed the request bytes.
//
// The `Host` header is the sharpest of them: `fetch` never exposes it on a
// `Request`, so it is SYNTHESIZED by `serializeRequest`. If that synthesis were
// dropped, nginx would answer 400 and no assertion about the body would fire.
func TestNginxServesFromABrowser(t *testing.T) {
	requireBrowserFixtures(t, "nginx.wasm", "nginx.img")
	node := requireNode(t)

	web, err := filepath.Abs(filepath.Join("..", "web"))
	if err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	base := "http://127.0.0.1:" + port
	srv, _, err := serve.Listen("127.0.0.1:"+port, web)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	t.Logf("serving %s", base)

	cmd := exec.CommandContext(ctxFor(t), "npx", "playwright", "test", "tests/nginx.spec.ts")
	cmd.Dir = web
	cmd.Env = append(os.Environ(),
		"RAPTORMARK_BASE_URL="+base,
		"PATH="+filepath.Dir(node)+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("playwright: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		t.Log(line)
	}
}

// TestNginxWorkerProcessesServeFromABrowser drives nginx's REAL process model:
// a master that forks workers, reconstructed by full replay, through the
// re-entrant scheduler.
//
// ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? If the fork never
// happened -- or happened and the child never ran -- the master would either
// serve the request itself or serve nothing. So the assertion is on the PID in
// the response body: nginx's own $pid, which must not be the master's. A
// response is not evidence of a worker; a response from a different process is.
func TestNginxWorkerProcessesServeFromABrowser(t *testing.T) {
	requireBrowserFixtures(t, "nginx.wasm", "nginx-workers.img")
	node := requireNode(t)

	web, err := filepath.Abs(filepath.Join("..", "web"))
	if err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	base := "http://127.0.0.1:" + port
	srv, _, err := serve.Listen("127.0.0.1:"+port, web)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	t.Logf("serving %s", base)

	cmd := exec.CommandContext(ctxFor(t), "npx", "playwright", "test", "tests/nginxworkers.spec.ts")
	cmd.Dir = web
	cmd.Env = append(os.Environ(),
		"RAPTORMARK_BASE_URL="+base,
		"PATH="+filepath.Dir(node)+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("playwright: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		t.Log(line)
	}
}

// TestNginxServesConcurrentRequests puts 25 requests in flight at once.
//
// ⚠️ THE POINT IS NOT THROUGHPUT. It is that 25 connections are open on the host
// simultaneously, each with its own accepted socket, response framer and pending
// promise, and that no reply lands on the wrong one. Crossed responses are the
// shape a concurrency bug takes here, and they are invisible to any check that
// only counts successes.
//
// It is also the regime the single-worker PINNING bug was found in -- traced
// accept4 counts of 20/1/0/0 across four workers -- which four sequential
// requests cannot reach.
func TestNginxServesConcurrentRequests(t *testing.T) {
	requireBrowserFixtures(t, "nginx.wasm", "nginx-workers.img")
	node := requireNode(t)

	web, err := filepath.Abs(filepath.Join("..", "web"))
	if err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	base := "http://127.0.0.1:" + port
	srv, _, err := serve.Listen("127.0.0.1:"+port, web)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	cmd := exec.CommandContext(ctxFor(t), "npx", "playwright", "test", "tests/nginxconc.spec.ts")
	cmd.Dir = web
	cmd.Env = append(os.Environ(),
		"RAPTORMARK_BASE_URL="+base,
		"PATH="+filepath.Dir(node)+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("playwright: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		t.Log(line)
	}
}

// TestNginxServesFilesFromTheVFS puts the RAPTORFS sidecar on the request path.
//
// Every other nginx variant here uses `return 200`, so the sidecar is read once
// at startup and never again. This one makes nginx open, stat and read a file
// per request, resolve an index, and apply mime.types -- and checks a 64 KiB
// binary by SHA-256, which is the only assertion a partially-working read cannot
// satisfy.
//
// Both `sendfile off` and `sendfile on` are exercised. They are different
// mechanisms -- read/write versus sendfile(2) from a VFS file to a host socket
// -- and the Alpine image ships the latter, so testing only the former would
// leave what a real deployment does unmeasured.
func TestNginxServesFilesFromTheVFS(t *testing.T) {
	requireBrowserFixtures(t, "nginx.wasm", "nginx-files.img", "nginx-sendfile.img",
		"data.bin.sha256")
	node := requireNode(t)

	web, err := filepath.Abs(filepath.Join("..", "web"))
	if err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	base := "http://127.0.0.1:" + port
	srv, _, err := serve.Listen("127.0.0.1:"+port, web)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	cmd := exec.CommandContext(ctxFor(t), "npx", "playwright", "test", "tests/nginxfiles.spec.ts")
	cmd.Dir = web
	cmd.Env = append(os.Environ(),
		"RAPTORMARK_BASE_URL="+base,
		"PATH="+filepath.Dir(node)+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("playwright: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		t.Log(line)
	}
}

// TestNginxWorkerRestartsAfterAKill drives nginx's SUPERVISION path: a worker
// dies and the master forks a replacement.
//
// ⚠️ It needed a new export. A guest in a tab has no outside process that could
// `kill` it, so nginx's master -- whose whole job is reacting to signals -- had
// only ever been observed doing its startup work. `ecv_signal` is that sender.
func TestNginxWorkerRestartsAfterAKill(t *testing.T) {
	requireBrowserFixtures(t, "nginx.wasm", "nginx-workers.img")
	node := requireNode(t)

	web, err := filepath.Abs(filepath.Join("..", "web"))
	if err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	base := "http://127.0.0.1:" + port
	srv, _, err := serve.Listen("127.0.0.1:"+port, web)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	cmd := exec.CommandContext(ctxFor(t), "npx", "playwright", "test", "tests/nginxrestart.spec.ts")
	cmd.Dir = web
	cmd.Env = append(os.Environ(),
		"RAPTORMARK_BASE_URL="+base,
		"PATH="+filepath.Dir(node)+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("playwright: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		t.Log(line)
	}
}

// TestNginxReloadsOnSIGHUP drives a graceful reload: nginx re-reads its config
// and cycles its workers without dropping the listener.
//
// Reachable only because `ecv_signal` exists -- see TestNginxWorkerRestartsAfterAKill.
func TestNginxReloadsOnSIGHUP(t *testing.T) {
	requireBrowserFixtures(t, "nginx.wasm", "nginx-workers.img")
	node := requireNode(t)

	web, err := filepath.Abs(filepath.Join("..", "web"))
	if err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	base := "http://127.0.0.1:" + port
	srv, _, err := serve.Listen("127.0.0.1:"+port, web)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	cmd := exec.CommandContext(ctxFor(t), "npx", "playwright", "test", "tests/nginxreload.spec.ts")
	cmd.Dir = web
	cmd.Env = append(os.Environ(),
		"RAPTORMARK_BASE_URL="+base,
		"PATH="+filepath.Dir(node)+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("playwright: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		t.Log(line)
	}
}
