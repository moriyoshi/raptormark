// Package oci packs a translated module into an OCI image tar that containerd,
// docker and a registry will all accept.
//
// # Stock shims run these, and that is a deliberate constraint
//
// Measured against containerd 2.2.1 with runwasi's own released shims (see
// e2e/containerd_test.go, which runs containerd in a container to check it).
//
// It did not start that way. Until 2026-08-09 both shims rejected every
// raptormark module — "This instruction or syntax requires enabling Exception
// Handling proposal" — because link-all finalised with `wasm-opt
// --translate-to-exnref`. The fix was not to enable the proposal but to stop
// needing it: suspension is a plain return now (elfconv patch 0026), so the
// emitted module uses nothing beyond Wasm 2.0.
//
// Enabling it in a shim was never really an option. runwasi pins
// `wasmedge-sdk 0.14.0`, whose CommonConfigOptions has no exception-handling
// field at all, and the wasmtime shim pins wasmtime 36 with the proposal off —
// so "flip a flag" was actually "fork the shim and migrate an engine SDK",
// for a shim only we could run.
//
// Treat "no wasm proposals" as a standing constraint on the emitted module, not
// an incidental property. It is what makes an unmodified released shim work,
// and internal/builder's TestWasmOptEnablesNoProposal guards it.
//
// ⚠️ It is NOT sufficient, and this paragraph used to imply it was. No-proposals
// says an engine can DECODE the module; the embedder must also SATISFY its
// imports, and every raptormark module -- even one whose guest is
// `int main(void){return 0;}` -- imports 11 functions from WasmEdge's socket
// extension to preview1 (sock_open, sock_bind, ...). A stock wasmtime shim
// decodes the module and then cannot instantiate it. See ModuleImports below and
// e2e/imports_test.go, which pin that surface; making a socket-free guest
// wasmtime-runnable is an open lever, not a property we have.
//
// # Why there are two formats
//
// raptormark emits two artifacts, not one: the module and the rfs sidecar. The
// sidecar is not decoration — it carries the guest filesystem *and* the boot
// record (argv/env/cwd/uid/gid), and ecvisor loads it with `std::fs::read`
// through WASI (runtime/src/entry.rs, load_sidecar). It must therefore be a real
// file the module can open at runtime, which is what rules the formats apart:
//
//   - FormatRootfs puts both files in an ordinary image layer, and the shim's
//     image handling is then entirely stock: it falls back to reading the module
//     out of the container rootfs, where the sidecar is sitting next to it.
//     Nothing else is required of the shim. This is the default, and the only
//     format with a working path to containerd today.
//
//   - FormatWasmLayers is the wasm-OCI shape: the module as a wasm layer, empty
//     rootfs, no filesystem. WITH A SIDECAR IT CANNOT RUN ANYWHERE. It needs a
//     shim that accepts the rootfs media type and writes that layer out as a
//     file before starting the module — there is otherwise nowhere for the
//     sidecar to be — and no such shim exists or is planned; upstream would be
//     unlikely to take one. Useful only for a sidecar-less module, or for
//     producing an image some other tool consumes. See SidecarNeedsShim.
//
// Output is an OCI image layout tar (plus a Docker-compatible manifest.json, as
// Docker still does not read the OCI layout), importable with any of:
//
//	regctl image import localhost:5000/name:tag img.tar
//	ctr images import img.tar
//	docker load -i img.tar
//
// Builds are deterministic: no timestamps are recorded and tar metadata is
// fixed, so the same inputs produce the same image digest.
package oci

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"raptormark/internal/rootfs"
)

// Format selects the image shape. See the package comment.
type Format string

const (
	FormatRootfs     Format = "rootfs"
	FormatWasmLayers Format = "wasm-layers"
)

// Media types. The wasm layer type is the one runwasi's default
// `supported_layers_types()` accepts; the rootfs type is ours, and no stock shim
// knows it.
const (
	MediaTypeLayer        = "application/vnd.oci.image.layer.v1.tar"
	MediaTypeImageConfig  = "application/vnd.oci.image.config.v1+json"
	MediaTypeManifest     = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeIndex        = "application/vnd.oci.image.index.v1+json"
	MediaTypeWasmLayer    = "application/vnd.bytecodealliance.wasm.component.layer.v0+wasm"
	MediaTypeRootfsLayer  = "application/vnd.raptormark.rootfs.v1+rfs"
	imageLayoutVersion    = "1.0.0"
	refNameAnnotation     = "org.opencontainers.image.ref.name"
	imageNameAnnotation   = "io.containerd.image.name"
	layersUniquenessLabel = "raptormark.layers"
)

// Guest-visible paths inside FormatRootfs images.
const (
	// SidecarPath is where the sidecar lands, and what RAPTORMARK_ROOTFS names.
	// ecvisor would also find it by its conventional names, but naming it
	// explicitly means a renamed module or an unexpected cwd cannot silently
	// produce a tmpfs-only guest with no filesystem and no boot record.
	SidecarPath = "/rootfs.img"
	// RootfsEnv must match ROOTFS_ENV in runtime/src/entry.rs.
	RootfsEnv = "RAPTORMARK_ROOTFS"
)

// SidecarNeedsShim reports whether a spec produces an image no stock runwasi
// shim can run, so callers can warn rather than ship something that fails at
// `ctr run` time with a tmpfs-only guest.
func (s Spec) SidecarNeedsShim() bool {
	return s.Format == FormatWasmLayers && s.Sidecar != ""
}

// Spec is one image to build.
type Spec struct {
	// Ref is the image reference, e.g. ghcr.io/me/app:latest.
	Ref string
	// Format selects the image shape; empty means FormatRootfs.
	Format Format
	// Module is the host path of the .wasm.
	Module string
	// Sidecar is the host path of the rfs image, or empty for a module that
	// needs no filesystem.
	Sidecar string
	// OS is the image config's `os`, empty meaning DefaultOS.
	//
	// Two conventions are in the wild and neither is wrong. runwasi's own
	// oci-tar-builder writes "wasip1", and containerd-shim-wasm never looks at
	// the field — its platform check is `let Arch::Wasm = ...` on the
	// architecture alone (containerd/client.rs). Docker's Wasm workloads are
	// documented with `--platform=wasi/wasm`, so an image aimed there wants
	// "wasi". The architecture is always "wasm".
	OS string
	// Personality is what the guest will actually see, and it is baked into the
	// sidecar's boot record already. It is mirrored into the image config so
	// `docker inspect` describes the container truthfully, and so a future
	// raptormark shim can override the boot record from the runtime spec.
	Personality rootfs.Boot
}

func (s Spec) format() Format {
	if s.Format == "" {
		return FormatRootfs
	}
	return s.Format
}

func (s Spec) validate() error {
	if s.Ref == "" {
		return fmt.Errorf("oci: Ref is required")
	}
	if s.Module == "" {
		return fmt.Errorf("oci: Module is required")
	}
	switch s.format() {
	case FormatRootfs, FormatWasmLayers:
	default:
		return fmt.Errorf("oci: unknown format %q", s.Format)
	}
	return nil
}

// Build writes the image tar to w. scratch is a directory for intermediate
// layer files; it is used so a multi-hundred-megabyte module never has to be
// held in memory.
func Build(w io.Writer, scratch string, s Spec) error {
	if err := s.validate(); err != nil {
		return err
	}

	layers, err := s.layers(scratch)
	if err != nil {
		return err
	}
	defer func() {
		for _, l := range layers {
			if l.temp {
				os.Remove(l.path)
			}
		}
	}()

	cfg, err := s.imageConfig(layers)
	if err != nil {
		return err
	}
	cfgBlob, err := jsonBlob(MediaTypeImageConfig, cfg)
	if err != nil {
		return err
	}

	annotations := map[string]string{imageNameAnnotation: s.Ref}
	if tag := refTag(s.Ref); tag != "" {
		annotations[refNameAnnotation] = tag
	}

	man := manifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeManifest,
		Config:        cfgBlob.descriptor(),
		Annotations:   annotations,
	}
	for _, l := range layers {
		man.Layers = append(man.Layers, l.descriptor())
	}
	manBlob, err := jsonBlob(MediaTypeManifest, man)
	if err != nil {
		return err
	}

	manDesc := manBlob.descriptor()
	manDesc.Platform = &platform{Architecture: archWasm, OS: s.os()}
	manDesc.Annotations = annotations
	idx := index{
		SchemaVersion: 2,
		MediaType:     MediaTypeIndex,
		Manifests:     []descriptor{manDesc},
	}
	idxJSON, err := json.Marshal(idx)
	if err != nil {
		return err
	}

	tw := tar.NewWriter(w)
	for _, b := range append(layers, cfgBlob, manBlob) {
		if err := b.writeTo(tw); err != nil {
			return err
		}
	}
	if err := writeFile(tw, "index.json", idxJSON); err != nil {
		return err
	}
	layout, err := json.Marshal(map[string]string{"imageLayoutVersion": imageLayoutVersion})
	if err != nil {
		return err
	}
	if err := writeFile(tw, "oci-layout", layout); err != nil {
		return err
	}

	// Docker still cannot read an OCI layout, so carry the legacy manifest too.
	dm := []dockerManifest{{
		Config:   cfgBlob.blobPath(),
		RepoTags: []string{s.Ref},
	}}
	for _, l := range layers {
		dm[0].Layers = append(dm[0].Layers, l.blobPath())
	}
	dmJSON, err := json.Marshal(dm)
	if err != nil {
		return err
	}
	if err := writeFile(tw, "manifest.json", dmJSON); err != nil {
		return err
	}

	return tw.Close()
}

const (
	archWasm = "wasm"
	// DefaultOS matches what runwasi's own image builder writes, which is the
	// value its demo images ship with and containerd is known to accept.
	DefaultOS = "wasip1"
	moduleDir = "/"
)

func (s Spec) os() string {
	if s.OS == "" {
		return DefaultOS
	}
	return s.OS
}

// layers builds the layer blobs for the chosen format.
func (s Spec) layers(scratch string) ([]blob, error) {
	if s.format() == FormatWasmLayers {
		module, err := fileBlob(MediaTypeWasmLayer, s.Module)
		if err != nil {
			return nil, err
		}
		out := []blob{module}
		if s.Sidecar != "" {
			side, err := fileBlob(MediaTypeRootfsLayer, s.Sidecar)
			if err != nil {
				return nil, err
			}
			out = append(out, side)
		}
		return out, nil
	}

	// FormatRootfs: one ordinary layer holding the module and, beside it, the
	// sidecar the module will open at runtime.
	entries := []tarEntry{{name: strings.TrimPrefix(s.modulePath(), "/"), src: s.Module}}
	if s.Sidecar != "" {
		entries = append(entries, tarEntry{name: strings.TrimPrefix(SidecarPath, "/"), src: s.Sidecar})
	}
	l, err := tarLayerBlob(scratch, entries)
	if err != nil {
		return nil, err
	}
	return []blob{l}, nil
}

// modulePath is where the module lands inside a FormatRootfs image, and what the
// entrypoint names. runwasi resolves the module from argv[0] of the runtime
// spec, so these two must agree.
func (s Spec) modulePath() string {
	return path.Join(moduleDir, path.Base(s.Module))
}

func (s Spec) imageConfig(layers []blob) (imageConfig, error) {
	cfg := imageConfig{
		Architecture: archWasm,
		OS:           s.os(),
		RootFS:       rootFS{Type: "layers", DiffIDs: []string{}},
	}

	env := append([]string{}, s.Personality.Env...)
	block := configBlock{
		WorkingDir: s.Personality.Cwd,
		User:       fmt.Sprintf("%d:%d", s.Personality.UID, s.Personality.GID),
	}
	if block.WorkingDir == "" {
		block.WorkingDir = "/"
	}

	switch s.format() {
	case FormatRootfs:
		// The layer is the rootfs, so its diff id belongs in the config.
		for _, l := range layers {
			cfg.RootFS.DiffIDs = append(cfg.RootFS.DiffIDs, l.digest)
		}
		// Entrypoint must be the module's path: it becomes argv[0] of the
		// runtime spec, which is what the shim opens.
		block.Entrypoint = []string{s.modulePath()}
		if len(s.Personality.Argv) > 1 {
			block.Cmd = append([]string{}, s.Personality.Argv[1:]...)
		}
		if s.Sidecar != "" {
			env = append(env, RootfsEnv+"="+SidecarPath)
		}
	case FormatWasmLayers:
		// No rootfs at all; the module arrives as a layer and the entrypoint is
		// a name, not a path that exists. Two images differing only in their
		// layers would otherwise share a config digest, so make it unique the
		// way runwasi's own builder does.
		block.Entrypoint = []string{path.Base(s.Module)}
		if len(s.Personality.Argv) > 1 {
			block.Cmd = append([]string{}, s.Personality.Argv[1:]...)
		}
		h := sha256.New()
		for _, l := range layers {
			h.Write([]byte(l.digest))
		}
		block.Labels = map[string]string{
			layersUniquenessLabel: hex.EncodeToString(h.Sum(nil)),
		}
	}

	block.Env = env
	cfg.Config = block
	return cfg, nil
}

// refTag extracts the tag from a reference, tolerating a registry port
// (localhost:5000/x:tag) and a digest reference.
func refTag(ref string) string {
	name := ref
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		name = ref[i+1:]
	}
	if i := strings.LastIndex(name, ":"); i >= 0 {
		return name[i+1:]
	}
	return ""
}
