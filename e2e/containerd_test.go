package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"raptormark/internal/oci"
	"raptormark/internal/rootfs"
)

// This is the integration nothing else covers: a packed image put in front of a
// real containerd running a real runwasi shim.
//
// containerd runs *inside a container* rather than on the host, because the host
// socket needs root and because the point is to test the image, not the machine.
// Three things about that nesting had to be worked out and are load-bearing:
//
//   - --snapshotter native. The default overlayfs snapshotter cannot mount
//     overlay-on-overlay inside Docker; it fails with "mount process exit
//     unexpectedly, exit code: EINVAL".
//   - --cgroupns=host. In a private cgroup namespace the container's cgroup root
//     has an empty cgroup.subtree_control and youki's setup fails writing +io
//     with EOPNOTSUPP.
//   - --privileged, so containerd can mount and manage cgroups at all.
//
// It is opt-in on top of RAPTORMARK_E2E because the harness image pulls ubuntu
// and downloads shim releases from GitHub, which a normal e2e run should not
// need to do.
const containerdEnv = "RAPTORMARK_E2E_CONTAINERD"

// Shim releases, pinned. aarch64 only: raptormark's guests are aarch64 and the
// builder image runs natively, so that is the only host this suite runs on.
const harnessDockerfile = `FROM ubuntu:24.04
RUN apt-get update -qq && \
    apt-get install -y -qq containerd runc curl ca-certificates >/dev/null && \
    rm -rf /var/lib/apt/lists/*
ARG BASE=https://github.com/containerd/runwasi/releases/download
RUN for s in wasmedge/v0.6.1 wasmtime/v0.6.1; do \
      r=$(echo $s | cut -d/ -f1); v=$(echo $s | cut -d/ -f2); \
      curl -sSL "$BASE/containerd-shim-$r/$v/containerd-shim-$r-aarch64-linux-musl.tar.gz" \
        | tar -xz -C /usr/local/bin; \
      chmod +x /usr/local/bin/containerd-shim-$r-v1; \
    done
`

const harnessImage = "raptormark-e2e-containerd:v1"

func requireContainerdHarness(t *testing.T, ctx context.Context) {
	t.Helper()
	if os.Getenv(containerdEnv) != "1" {
		t.Skipf("set %s=1 to run the containerd-in-a-container integration "+
			"(pulls ubuntu and downloads runwasi shim releases)", containerdEnv)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(harnessDockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(ctx, "docker", "build", "-q", "-t", harnessImage, dir).CombinedOutput()
	if err != nil {
		t.Fatalf("building the containerd harness: %v\n%s", err, out)
	}
}

// runInContainerd starts containerd in a container, imports the tar and runs it
// under the named shim, returning the combined output of the whole script.
func runInContainerd(t *testing.T, ctx context.Context, tarPath, ref, runtime string) string {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf(`
containerd --log-level warn >/tmp/containerd.log 2>&1 &
for i in $(seq 60); do ctr version >/dev/null 2>&1 && break; sleep 0.25; done
echo "=== import ==="
ctr images import --platform %s/wasm /art/%s || exit 1
ctr images ls
echo "=== run (%s) ==="
ctr run --rm --snapshotter native --runtime=%s %s e2e-$$
echo "=== exit=$? ==="
echo "=== shim log ==="
grep -iE "WASM OCI|WASM layers|error running|boot argv" /tmp/containerd.log || true
`, oci.DefaultOS, filepath.Base(tarPath), runtime, runtime, ref)
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	absArt, err := filepath.Abs(filepath.Dir(tarPath))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm",
		"--privileged", "--cgroupns=host",
		"-v", absArt+":/art:ro", "-v", dir+":/script:ro",
		harnessImage, "bash", "/script/run.sh").CombinedOutput()
	// The script's own exit status is not the signal; the assertions read the
	// output. A docker-level failure still matters.
	if err != nil && len(out) == 0 {
		t.Fatalf("docker run: %v", err)
	}
	t.Logf("%s:\n%s", runtime, out)
	return string(out)
}

// TestContainerdAcceptsAndRunsTheImage is the whole-chain check: pack an image,
// import it into a real containerd, and run it under an unmodified released
// runwasi shim — no fork, no patched engine, no proposal flags.
func TestContainerdAcceptsAndRunsTheImage(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)
	requireContainerdHarness(t, ctx)

	bootArgv := []string{"/sentinel/from-boot-record"}
	wasm, sidecar := buildModuleAndSidecar(t, ctx, img, bootArgv)

	dir := t.TempDir()
	tarPath := filepath.Join(dir, "img.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	const ref = "raptormark.test/ociguest:ctr"
	if err := oci.Build(f, dir, oci.Spec{
		Ref: ref, Module: wasm, Sidecar: sidecar,
		// RAPTORMARK_ECV_DEBUG makes ecvisor echo the boot record it parsed, which
		// is the only way to tell "the sidecar was read" from "the guest ran with
		// a tmpfs-only fallback". Personality.Env becomes the image config Env, so
		// it reaches the guest as container environment through the stock shim —
		// there is nowhere else to inject it in a `ctr run`.
		Personality: rootfs.Boot{
			Argv: bootArgv,
			Cwd:  "/",
			Env:  []string{"RAPTORMARK_ECV_DEBUG=1"},
		},
	}); err != nil {
		t.Fatalf("packing: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// Both shims accept and load the module. Only wasmedge can RUN it, and the
	// reason is not the shim: ecvisor imports WasmEdge's non-standard preview1
	// socket extensions (sock_open / sock_accept, runtime/src/sys.rs), which
	// wasmtime's WASI p1 does not provide. That dependency predates this test
	// and was simply invisible while wasmtime rejected the module at parse time.
	for _, rt := range []struct {
		name string
		// runs is false while a known, named blocker stops the guest short of
		// completion. blocker must then appear in the output.
		runs    bool
		blocker string
	}{
		{name: "io.containerd.wasmedge.v1", runs: true},
		{name: "io.containerd.wasmtime.v1", blocker: "wasi_snapshot_preview1::sock_open"},
	} {
		runtime := rt.name
		t.Run(runtime, func(t *testing.T) {
			out := runInContainerd(t, ctx, tarPath, ref, runtime)

			// 1. containerd accepts the tar and records the platform. This is the
			//    part `docker load` can never check, because the classic image
			//    store refuses any non-linux image.
			if !strings.Contains(out, ref) {
				t.Fatalf("containerd did not import the image:\n%s", out)
			}
			if !strings.Contains(out, oci.DefaultOS+"/wasm") {
				t.Errorf("containerd did not record %s/wasm:\n%s", oci.DefaultOS, out)
			}

			// 2. The shim takes the file-in-rootfs path, which is the whole
			//    premise of FormatRootfs: arch is wasm, but no layer matches
			//    supported_layers_types(), so the module is read from the rootfs.
			if !strings.Contains(out, "found manifest with WASM OCI image format") {
				t.Errorf("the shim did not recognise the image as wasm:\n%s", out)
			}
			if !strings.Contains(out, "no WASM layers found in OCI image") {
				t.Errorf("the shim did not fall back to the rootfs module:\n%s", out)
			}

			// 3. The module RUNS, on a stock shim, unmodified. This is the point
			//    of lowering suspension to a plain return (elfconv patch 0026):
			//    the module needs no wasm proposal, so no shim fork is needed.
			//
			//    Until 2026-08-09 this asserted the opposite — both released
			//    shims rejected every raptormark module, because link-all
			//    finalised with `wasm-opt --translate-to-exnref` and neither
			//    shim enables exception-handling. Nothing about the shims
			//    changed; the module stopped asking for the proposal.
			// This one holds for BOTH engines and is the property worth guarding:
			// no wasm proposal, so nothing is rejected at parse time.
			if strings.Contains(out, "Exception Handling proposal") ||
				strings.Contains(out, "exceptions proposal not enabled") {
				t.Fatalf("the module carries exception-handling EH again — something "+
					"put a proposal back into the emitted wasm (check link-all's "+
					"wasmOptArgs and the ecv_sjlj shim's absence):\n%s", out)
			}

			ran := strings.Contains(out, "OCI-PACK-OK")
			if !rt.runs {
				if ran {
					t.Errorf("the guest RAN to completion on %s. The %q blocker is "+
						"gone — set runs:true for this runtime and drop the blocker.",
						runtime, rt.blocker)
				} else if !strings.Contains(out, rt.blocker) {
					t.Errorf("%s failed for an unexpected reason (not the known %q "+
						"blocker):\n%s", runtime, rt.blocker, out)
				}
				return
			}
			if !ran {
				t.Errorf("the guest did not run to completion on a stock %s shim:\n%s",
					runtime, out)
			}
			// ecvisor echoes the boot record it parsed (RAPTORMARK_ECV_DEBUG, set
			// in the image config Env above), so the sentinel argv proves the rootfs
			// sidecar was found and read through the shim's stock image handling —
			// not just that the module validated and started.
			if !strings.Contains(out, "/sentinel/from-boot-record") {
				t.Errorf("the guest ran but did not report the boot-record argv, so the "+
					"sidecar was not picked up:\n%s", out)
			}
		})
	}
}
