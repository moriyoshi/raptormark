package oci

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"raptormark/internal/rootfs"
)

// fixture writes a module and a sidecar and returns their paths.
func fixture(t *testing.T) (dir, module, sidecar string) {
	t.Helper()
	dir = t.TempDir()
	module = filepath.Join(dir, "app.wasm")
	sidecar = filepath.Join(dir, "rootfs.img")
	// A real wasm header, so the bytes are at least the shape a shim sniffs.
	if err := os.WriteFile(module, append([]byte("\x00asm\x01\x00\x00\x00"), bytes.Repeat([]byte{0xaa}, 512)...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecar, []byte("RAPTORFS-fake-image"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, module, sidecar
}

// readTar indexes a tar into name -> contents.
func readTar(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = data
	}
	return out
}

func build(t *testing.T, s Spec) ([]byte, map[string][]byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := Build(&buf, t.TempDir(), s); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), readTar(t, buf.Bytes())
}

// blobFor resolves a descriptor to its bytes and checks the digest actually
// matches — the whole point of a content-addressed layout, and the one error a
// registry will reject the push for.
func blobFor(t *testing.T, files map[string][]byte, d descriptor) []byte {
	t.Helper()
	name := "blobs/sha256/" + d.Digest[len("sha256:"):]
	data, ok := files[name]
	if !ok {
		t.Fatalf("descriptor %s has no blob in the tar", d.Digest)
	}
	sum := sha256.Sum256(data)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != d.Digest {
		t.Errorf("blob %s hashes to %s", d.Digest, got)
	}
	if int64(len(data)) != d.Size {
		t.Errorf("descriptor %s says %d bytes, blob is %d", d.Digest, d.Size, len(data))
	}
	return data
}

// manifestOf walks index.json -> manifest, verifying digests on the way.
func manifestOf(t *testing.T, files map[string][]byte) (manifest, imageConfig) {
	t.Helper()
	var idx index
	if err := json.Unmarshal(files["index.json"], &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Manifests) != 1 {
		t.Fatalf("want one manifest, got %d", len(idx.Manifests))
	}
	var man manifest
	if err := json.Unmarshal(blobFor(t, files, idx.Manifests[0]), &man); err != nil {
		t.Fatal(err)
	}
	var cfg imageConfig
	if err := json.Unmarshal(blobFor(t, files, man.Config), &cfg); err != nil {
		t.Fatal(err)
	}
	return man, cfg
}

func TestRootfsFormatUsesStockImageHandling(t *testing.T) {
	_, module, sidecar := fixture(t)
	_, files := build(t, Spec{
		Ref: "ghcr.io/me/app:latest", Module: module, Sidecar: sidecar,
		Personality: rootfs.Boot{
			Argv: []string{"/usr/sbin/nginx", "-g", "daemon off;"},
			Env:  []string{"PATH=/usr/bin", "NGINX_VERSION=1.27"},
			Cwd:  "/var/www", UID: 101, GID: 101,
		},
	})
	man, cfg := manifestOf(t, files)

	if cfg.Architecture != "wasm" || cfg.OS != DefaultOS {
		t.Errorf("platform = %s/%s, want %s/wasm", cfg.OS, cfg.Architecture, DefaultOS)
	}

	// The entrypoint becomes argv[0] of the runtime spec, which is the path the
	// shim opens. If it does not name the module, nothing runs. (Confirmed
	// against a real shim: it logs "no WASM layers found in OCI image" and takes
	// this path -- see e2e/containerd_test.go.)
	if !slices.Equal(cfg.Config.Entrypoint, []string{"/app.wasm"}) {
		t.Errorf("Entrypoint = %q, want [/app.wasm]", cfg.Config.Entrypoint)
	}
	if !slices.Equal(cfg.Config.Cmd, []string{"-g", "daemon off;"}) {
		t.Errorf("Cmd = %q, want the argv tail", cfg.Config.Cmd)
	}

	// Naming the sidecar explicitly is what keeps a renamed module or an
	// unexpected cwd from silently yielding a tmpfs-only guest.
	if !slices.Contains(cfg.Config.Env, RootfsEnv+"="+SidecarPath) {
		t.Errorf("Env is missing %s=%s: %q", RootfsEnv, SidecarPath, cfg.Config.Env)
	}
	if !slices.Contains(cfg.Config.Env, "NGINX_VERSION=1.27") {
		t.Errorf("Env lost the guest environment: %q", cfg.Config.Env)
	}
	if cfg.Config.WorkingDir != "/var/www" || cfg.Config.User != "101:101" {
		t.Errorf("personality lost: cwd=%q user=%q", cfg.Config.WorkingDir, cfg.Config.User)
	}

	// One ordinary layer, and its digest is its diff id because it is
	// uncompressed.
	if len(man.Layers) != 1 || man.Layers[0].MediaType != MediaTypeLayer {
		t.Fatalf("want one %s layer, got %+v", MediaTypeLayer, man.Layers)
	}
	if !slices.Equal(cfg.RootFS.DiffIDs, []string{man.Layers[0].Digest}) {
		t.Errorf("diff_ids = %q, want the layer digest %q", cfg.RootFS.DiffIDs, man.Layers[0].Digest)
	}

	// Both files must be in the layer, and the sidecar exactly where
	// RAPTORMARK_ROOTFS says.
	layer := readTar(t, blobFor(t, files, man.Layers[0]))
	if _, ok := layer["app.wasm"]; !ok {
		t.Errorf("layer is missing app.wasm: %v", keys(layer))
	}
	if _, ok := layer["rootfs.img"]; !ok {
		t.Errorf("layer is missing rootfs.img: %v", keys(layer))
	}
}

func TestWasmLayersFormat(t *testing.T) {
	_, module, sidecar := fixture(t)
	_, files := build(t, Spec{
		Ref: "ghcr.io/me/app:v1", Format: FormatWasmLayers,
		Module: module, Sidecar: sidecar,
	})
	man, cfg := manifestOf(t, files)

	if len(man.Layers) != 2 {
		t.Fatalf("want module + sidecar layers, got %d", len(man.Layers))
	}
	if man.Layers[0].MediaType != MediaTypeWasmLayer {
		t.Errorf("module layer type = %s, want %s", man.Layers[0].MediaType, MediaTypeWasmLayer)
	}
	if man.Layers[1].MediaType != MediaTypeRootfsLayer {
		t.Errorf("sidecar layer type = %s, want %s", man.Layers[1].MediaType, MediaTypeRootfsLayer)
	}
	// No filesystem: the wasm arrives as a layer, not as a file.
	if len(cfg.RootFS.DiffIDs) != 0 {
		t.Errorf("diff_ids must be empty, got %q", cfg.RootFS.DiffIDs)
	}
	// With an empty rootfs, two different images would otherwise share a config
	// digest.
	if cfg.Config.Labels[layersUniquenessLabel] == "" {
		t.Errorf("missing the %s uniqueness label", layersUniquenessLabel)
	}
}

// The sidecar is loaded with fs::read, so a layer it cannot open is not a
// runnable image. Callers are warned; this pins the predicate.
func TestSidecarNeedsShim(t *testing.T) {
	cases := []struct {
		format  Format
		sidecar string
		want    bool
	}{
		{FormatRootfs, "rootfs.img", false},
		{FormatRootfs, "", false},
		{FormatWasmLayers, "", false},
		{FormatWasmLayers, "rootfs.img", true},
	}
	for _, c := range cases {
		got := Spec{Format: c.format, Sidecar: c.sidecar}.SidecarNeedsShim()
		if got != c.want {
			t.Errorf("format=%s sidecar=%q: got %v, want %v", c.format, c.sidecar, got, c.want)
		}
	}
}

// Without a sidecar there is nothing for a shim to materialise, so wasm-layers
// needs no more of it than rootfs does.
func TestWasmLayersWithoutSidecarNeedsNoExtraShimWork(t *testing.T) {
	_, module, _ := fixture(t)
	spec := Spec{Ref: "r/app:t", Format: FormatWasmLayers, Module: module}
	if spec.SidecarNeedsShim() {
		t.Error("no sidecar means nothing to materialise")
	}
	_, files := build(t, spec)
	man, _ := manifestOf(t, files)
	if len(man.Layers) != 1 || man.Layers[0].MediaType != MediaTypeWasmLayer {
		t.Errorf("want a single wasm layer, got %+v", man.Layers)
	}
}

// Determinism keeps the image digest a function of its inputs, which is what
// makes a rebuild comparable and a cache meaningful.
func TestBuildIsDeterministic(t *testing.T) {
	_, module, sidecar := fixture(t)
	s := Spec{Ref: "r/app:t", Module: module, Sidecar: sidecar,
		Personality: rootfs.Boot{Argv: []string{"/app"}, Cwd: "/"}}
	a, _ := build(t, s)
	b, _ := build(t, s)
	if !bytes.Equal(a, b) {
		t.Error("two builds of the same spec differ")
	}
}

func TestLayoutFilesArePresent(t *testing.T) {
	_, module, sidecar := fixture(t)
	_, files := build(t, Spec{Ref: "r/app:t", Module: module, Sidecar: sidecar})
	for _, want := range []string{"index.json", "oci-layout", "manifest.json"} {
		if _, ok := files[want]; !ok {
			t.Errorf("missing %s: %v", want, keys(files))
		}
	}
	// Docker cannot read the OCI layout, so manifest.json has to point at real
	// blobs too.
	var dm []dockerManifest
	if err := json.Unmarshal(files["manifest.json"], &dm); err != nil {
		t.Fatal(err)
	}
	if len(dm) != 1 || dm[0].RepoTags[0] != "r/app:t" {
		t.Fatalf("unexpected docker manifest: %+v", dm)
	}
	for _, p := range append([]string{dm[0].Config}, dm[0].Layers...) {
		if _, ok := files[p]; !ok {
			t.Errorf("manifest.json references %s, which is not in the tar", p)
		}
	}
}

// The os field is a convention, not a check: containerd-shim-wasm matches on
// Arch::Wasm alone. runwasi's builder writes wasip1; Docker's Wasm workloads are
// documented as wasi/wasm. Both must reach the config and the index descriptor,
// and they must agree with each other.
func TestOSIsSelectableAndConsistent(t *testing.T) {
	_, module, sidecar := fixture(t)
	for _, want := range []string{DefaultOS, "wasi", "wasip2"} {
		_, files := build(t, Spec{Ref: "r/a:t", Module: module, Sidecar: sidecar, OS: want})

		var idx index
		if err := json.Unmarshal(files["index.json"], &idx); err != nil {
			t.Fatal(err)
		}
		if got := idx.Manifests[0].Platform; got == nil || got.OS != want || got.Architecture != "wasm" {
			t.Errorf("index platform = %+v, want %s/wasm", got, want)
		}
		_, cfg := manifestOf(t, files)
		if cfg.OS != want {
			t.Errorf("config os = %q, want %q", cfg.OS, want)
		}
		if cfg.Architecture != "wasm" {
			t.Errorf("architecture = %q, must always be wasm", cfg.Architecture)
		}
	}
}

func TestDefaultOSMatchesRunwasisBuilder(t *testing.T) {
	// crates/oci-tar-builder/src/bin.rs and crates/wasi-demo-app/build.rs both
	// call .os("wasip1"), and those are the images known to work with containerd.
	if DefaultOS != "wasip1" {
		t.Errorf("DefaultOS = %q; runwasi's own builder writes wasip1", DefaultOS)
	}
}

func TestRefTagHandlesRegistryPorts(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/me/app:latest":       "latest",
		"localhost:5000/app:v1.2":     "v1.2",
		"localhost:5000/team/app:dev": "dev",
		"app":                         "",
		"localhost:5000/app":          "",
	}
	for ref, want := range cases {
		if got := refTag(ref); got != want {
			t.Errorf("refTag(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestValidation(t *testing.T) {
	_, module, _ := fixture(t)
	for name, s := range map[string]Spec{
		"no ref":     {Module: module},
		"no module":  {Ref: "r/a:t"},
		"bad format": {Ref: "r/a:t", Module: module, Format: "artifact"},
	} {
		if err := Build(io.Discard, t.TempDir(), s); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
