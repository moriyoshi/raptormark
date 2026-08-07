package e2e

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"raptormark/internal/link"
	"raptormark/internal/oci"
	"raptormark/internal/rootfs"
	"raptormark/internal/translate"
)

// The OCI packer's unit tests prove the layout is internally consistent. What
// they cannot prove is that a real container runtime accepts it and that the
// paths it bakes in are the ones ecvisor actually looks for at runtime. That is
// what these do.

// buildModuleAndSidecar produces a real ecvisor module plus an rfs sidecar whose
// boot record is identifiable, so a later assertion can tell "the sidecar was
// read" from "the guest fell back to host argv".
func buildModuleAndSidecar(t *testing.T, ctx context.Context, img string, argv []string) (wasm, sidecar string) {
	t.Helper()
	work := t.TempDir()

	guestDir := filepath.Join(work, "ctx")
	if err := os.MkdirAll(guestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	elf := compileGuest(t, ctx, guestDir, "guest", fmt.Sprintf(guestSrc, "OCI-PACK-OK"))

	// A minimal guest filesystem: the sidecar's job here is to carry the boot
	// record, not a real rootfs.
	fsRoot := filepath.Join(work, "root")
	if err := os.MkdirAll(filepath.Join(fsRoot, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fsRoot, "etc", "marker"), []byte("marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	image, _, err := rootfs.Build(fsRoot, rootfs.Options{Boot: &rootfs.Boot{
		Argv: argv,
		Env:  []string{"OCI_PACK=1"},
		Cwd:  "/",
	}})
	if err != nil {
		t.Fatalf("building the rfs sidecar: %v", err)
	}
	sidecar = filepath.Join(work, "rootfs.img")
	if err := os.WriteFile(sidecar, image, 0o644); err != nil {
		t.Fatal(err)
	}

	const name = "ociguest"
	prog := link.Program{Name: name, Index: 0}
	fragment := filepath.Join(work, "frag.c")
	if err := os.WriteFile(fragment, []byte(link.FragmentC(prog)), 0o644); err != nil {
		t.Fatal(err)
	}
	registry, err := link.RegistryC([]link.Program{prog})
	if err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(work, "registry.c")
	if err := os.WriteFile(registryPath, []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}

	b, err := translate.BuilderFromImage(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(work, "out")
	translateOne(t, ctx, b, translate.Request{
		ELF: elf, OutDir: outDir, ModuleID: name,
		Fragment: fragment, Keep: prog.Symbol(),
		Options: translate.Options{Runtime: "ecvisor"},
	})
	wasm = filepath.Join(work, name+".wasm")
	if err := b.Link(ctx, translate.LinkRequest{
		Registry: registryPath,
		Objects:  []string{filepath.Join(outDir, name+".o")},
		Out:      wasm,
	}); err != nil {
		t.Fatalf("linking: %v", err)
	}
	return wasm, sidecar
}

// TestOCIRootfsImageIsAcceptedAndRunnable packs a real module, imports the tar
// with containerd and Docker, and then runs the module out of the layer with
// exactly the environment the image config specifies.
func TestOCIRootfsImageIsAcceptedAndRunnable(t *testing.T) {
	img := requireE2E(t)
	ctx := ctxFor(t)

	bootArgv := []string{"/sentinel/from-boot-record", "--flag"}
	wasm, sidecar := buildModuleAndSidecar(t, ctx, img, bootArgv)

	dir := t.TempDir()
	tarPath := filepath.Join(dir, "img.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	spec := oci.Spec{
		Ref:     "raptormark.test/ociguest:e2e",
		Module:  wasm,
		Sidecar: sidecar,
		Personality: rootfs.Boot{
			Argv: bootArgv,
			Env:  []string{"OCI_PACK=1"},
			Cwd:  "/",
		},
	}
	if err := oci.Build(f, dir, spec); err != nil {
		t.Fatalf("packing: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(tarPath); err == nil {
		t.Logf("packed %s (%.1f MiB)", tarPath, float64(info.Size())/(1<<20))
	}
	// The temp dirs go away with the test, so a run that needs to be poked at
	// afterwards — fed to a real containerd, inspected with regctl — has nothing
	// left to poke. RAPTORMARK_OCI_KEEP names somewhere durable to copy to.
	if keep := os.Getenv("RAPTORMARK_OCI_KEEP"); keep != "" {
		copyFile(t, tarPath, filepath.Join(keep, "img.tar"))
		copyFile(t, wasm, filepath.Join(keep, filepath.Base(wasm)))
		copyFile(t, sidecar, filepath.Join(keep, "rootfs.img"))
		t.Logf("kept the artifacts in %s", keep)
	}

	importWithContainerd(t, ctx, tarPath, spec.Ref)
	dockerMustParseAndRefuse(t, ctx, tarPath)

	cfg := configFromTar(t, ctx, tarPath)

	// What containerd will put in the runtime spec, and therefore what the shim
	// sees. argv[0] must name the module or nothing runs.
	if len(cfg.Entrypoint) == 0 || !strings.HasSuffix(cfg.Entrypoint[0], ".wasm") {
		t.Errorf("Entrypoint = %q, want the module path", cfg.Entrypoint)
	}
	if !contains(cfg.Env, oci.RootfsEnv+"="+oci.SidecarPath) {
		t.Errorf("Env = %q, missing %s", cfg.Env, oci.RootfsEnv)
	}

	// Now the part the layout exists for: unpack the layer and run the module
	// under exactly the entrypoint and environment the config declares. The
	// sidecar has to be findable at the path the config names.
	runDir := t.TempDir()
	extractLayer(t, ctx, tarPath, runDir)

	moduleName := filepath.Base(cfg.Entrypoint[0])
	if _, err := os.Stat(filepath.Join(runDir, moduleName)); err != nil {
		t.Fatalf("the entrypoint %s is not in the layer: %v", cfg.Entrypoint[0], err)
	}

	got := runWasmIn(t, ctx, filepath.Join(runDir, moduleName), nil,
		[]string{oci.RootfsEnv + "=" + oci.SidecarPath, "RAPTORMARK_ECV_DEBUG=1"}, "/:/out")

	if !strings.Contains(got, "OCI-PACK-OK") {
		t.Errorf("module did not run out of the packed layer:\n%s", got)
	}
	// ecvisor echoes the boot record it parsed. Seeing the sentinel argv proves
	// the sidecar was found at SidecarPath and decoded — not that the guest fell
	// back to host argv with a tmpfs-only filesystem.
	if !strings.Contains(got, "/sentinel/from-boot-record") {
		t.Errorf("the sidecar's boot record was not read; the guest fell back to host argv:\n%s", got)
	}
	if strings.Contains(got, "set but unreadable") {
		t.Errorf("%s points somewhere the guest cannot open:\n%s", oci.RootfsEnv, got)
	}
}

// importWithContainerd checks the tar against the runtime that will actually run
// it. It needs the containerd socket, so it is skipped rather than failed when
// the socket is not reachable as this user.
func importWithContainerd(t *testing.T, ctx context.Context, tarPath, ref string) {
	t.Helper()
	if _, err := exec.LookPath("ctr"); err != nil {
		t.Log("ctr not on PATH; skipping the containerd import")
		return
	}
	out, err := exec.CommandContext(ctx, "ctr", "images", "import",
		"--platform", oci.DefaultOS+"/wasm", tarPath).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "permission denied") || strings.Contains(string(out), "connection refused") {
			t.Logf("containerd not reachable as this user; skipping: %s", strings.TrimSpace(string(out)))
			return
		}
		t.Fatalf("ctr images import: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		exec.Command("ctr", "images", "rm", ref).Run()
	})
	t.Logf("containerd accepted the image: %s", strings.TrimSpace(string(out)))

	// containerd must record it as a wasm image, or the shim's platform check
	// (Arch::Wasm in containerd-shim-wasm's client.rs) never fires.
	list, err := exec.CommandContext(ctx, "ctr", "images", "ls", "name=="+ref).CombinedOutput()
	if err == nil && !strings.Contains(string(list), "wasm") {
		t.Errorf("containerd did not record a wasm platform:\n%s", list)
	}
}

// dockerMustParseAndRefuse is a decoder check, not an install check.
//
// Docker will not load a non-linux image at all ("cannot load wasip1 image on
// linux"). That is expected: wasm images are distributed through containerd and
// registries, not `docker load`, and runwasi's own demo images are the same
// shape. But reaching that refusal means Docker walked manifest.json, found the
// config blob and parsed its platform — which is exactly the second-implementation
// layout check worth having. Any *other* failure is a real defect.
func dockerMustParseAndRefuse(t *testing.T, ctx context.Context, tarPath string) {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", "load", "-i", tarPath).CombinedOutput()
	msg := strings.TrimSpace(string(out))
	switch {
	case err == nil:
		t.Logf("docker loaded the image: %s", msg)
	case strings.Contains(msg, oci.DefaultOS):
		t.Logf("docker parsed the layout and refused on platform, as expected: %s", msg)
	default:
		t.Errorf("docker could not read the image tar: %v\n%s", err, msg)
	}
}

// configFromTar reads the image config back out of the packed tar the way a
// registry client would: index.json -> manifest -> config blob. It deliberately
// does not use internal/oci's own types, so a mistake in those structs cannot
// make this pass.
func configFromTar(t *testing.T, ctx context.Context, tarPath string) imageConfigView {
	t.Helper()
	files := untar(t, tarPath)

	var idx struct {
		Manifests []struct {
			Digest   string
			Platform struct{ Architecture, OS string }
		}
	}
	if err := json.Unmarshal(files["index.json"], &idx); err != nil {
		t.Fatalf("index.json: %v", err)
	}
	if len(idx.Manifests) != 1 {
		t.Fatalf("want one manifest, got %d", len(idx.Manifests))
	}
	if p := idx.Manifests[0].Platform; p.OS != oci.DefaultOS || p.Architecture != "wasm" {
		t.Errorf("index platform = %s/%s, want %s/wasm", p.OS, p.Architecture, oci.DefaultOS)
	}

	var man struct {
		Config struct{ Digest string }
		Layers []struct {
			Digest, MediaType string
		}
	}
	if err := json.Unmarshal(files[blobName(idx.Manifests[0].Digest)], &man); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var cfg struct {
		Architecture string
		OS           string
		Config       imageConfigView
	}
	if err := json.Unmarshal(files[blobName(man.Config.Digest)], &cfg); err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.OS != oci.DefaultOS || cfg.Architecture != "wasm" {
		t.Errorf("config platform = %s/%s, want %s/wasm", cfg.OS, cfg.Architecture, oci.DefaultOS)
	}
	return cfg.Config
}

type imageConfigView struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string
	User       string
}

func blobName(digest string) string {
	return "blobs/sha256/" + strings.TrimPrefix(digest, "sha256:")
}

func untar(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string][]byte{}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = b
	}
	return out
}

// extractLayer unpacks the image's single layer into dest, using the builder
// image's own tar so this does not depend on host tooling.
func extractLayer(t *testing.T, ctx context.Context, tarPath, dest string) {
	t.Helper()
	absTar, err := filepath.Abs(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	absDest, err := filepath.Abs(dest)
	if err != nil {
		t.Fatal(err)
	}
	// The layer is the one blob that is itself a tar; find it by trying each.
	out, err := dockerRun(ctx,
		[]string{"-v", filepath.Dir(absTar) + ":/tar:ro", "-v", absDest + ":/dest"},
		"set -e; mkdir -p /tmp/img && tar -xf /tar/"+filepath.Base(absTar)+" -C /tmp/img; "+
			"for b in /tmp/img/blobs/sha256/*; do "+
			"  if tar -tf \"$b\" >/dev/null 2>&1; then tar -xf \"$b\" -C /dest; fi; "+
			"done; ls -la /dest")
	if err != nil {
		t.Fatalf("extracting the layer: %v\n%s", err, out)
	}
	t.Logf("layer contents:\n%s", out)
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func contains(hay []string, want string) bool {
	for _, h := range hay {
		if h == want {
			return true
		}
	}
	return false
}
