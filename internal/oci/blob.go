package oci

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// The OCI JSON documents, hand-rolled rather than pulled from
// github.com/opencontainers/image-spec: what raptormark emits is a narrow slice
// of the spec, and the whole surface here is under a hundred lines.

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Platform    *platform         `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

type manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Config        descriptor        `json:"config"`
	Layers        []descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

type index struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []descriptor `json:"manifests"`
}

type imageConfig struct {
	Architecture string      `json:"architecture"`
	OS           string      `json:"os"`
	Config       configBlock `json:"config"`
	RootFS       rootFS      `json:"rootfs"`
}

// configBlock uses the capitalised field names the image spec inherited from
// Docker; containerd reads these exact keys when it builds the runtime spec.
type configBlock struct {
	Entrypoint []string          `json:"Entrypoint,omitempty"`
	Cmd        []string          `json:"Cmd,omitempty"`
	Env        []string          `json:"Env,omitempty"`
	WorkingDir string            `json:"WorkingDir,omitempty"`
	User       string            `json:"User,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
}

type rootFS struct {
	Type    string   `json:"type"`
	DiffIDs []string `json:"diff_ids"`
}

type dockerManifest struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// blob is one content-addressed object bound for blobs/sha256/. Large blobs live
// on disk and are streamed; small ones (config, manifest) are held in memory.
type blob struct {
	mediaType string
	digest    string // "sha256:..."
	size      int64

	data []byte // in-memory blobs
	path string // on-disk blobs
	temp bool   // path is ours to delete
}

func (b blob) descriptor() descriptor {
	return descriptor{MediaType: b.mediaType, Digest: b.digest, Size: b.size}
}

// blobPath is the location inside the tar, and what manifest.json references.
func (b blob) blobPath() string {
	return "blobs/sha256/" + b.digest[len("sha256:"):]
}

func (b blob) writeTo(tw *tar.Writer) error {
	if b.data != nil {
		return writeFile(tw, b.blobPath(), b.data)
	}
	f, err := os.Open(b.path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := writeHeader(tw, b.blobPath(), b.size); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

func jsonBlob(mediaType string, v any) (blob, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return blob{}, err
	}
	sum := sha256.Sum256(data)
	return blob{
		mediaType: mediaType,
		digest:    "sha256:" + hex.EncodeToString(sum[:]),
		size:      int64(len(data)),
		data:      data,
	}, nil
}

// fileBlob hashes a file in place and streams it later, so a module of any size
// costs one read and no memory.
func fileBlob(mediaType, path string) (blob, error) {
	f, err := os.Open(path)
	if err != nil {
		return blob{}, fmt.Errorf("oci: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return blob{}, err
	}
	return blob{
		mediaType: mediaType,
		digest:    "sha256:" + hex.EncodeToString(h.Sum(nil)),
		size:      n,
		path:      path,
	}, nil
}

type tarEntry struct {
	name string // path inside the layer, with no leading slash
	src  string // host path
}

// tarLayerBlob builds an uncompressed layer tar in scratch and hashes it as it
// writes.
//
// Uncompressed on purpose: the layer's digest is then also its diff id, and the
// two things it carries are a wasm module and an already-DEFLATE'd rfs image, so
// gzip would spend real time to save very little.
func tarLayerBlob(scratch string, entries []tarEntry) (b blob, err error) {
	f, err := os.CreateTemp(scratch, "layer-*.tar")
	if err != nil {
		return blob{}, err
	}
	defer func() {
		f.Close()
		if err != nil {
			os.Remove(f.Name())
		}
	}()

	h := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(f, h))
	for _, e := range entries {
		if err = copyInto(tw, e); err != nil {
			return blob{}, err
		}
	}
	if err = tw.Close(); err != nil {
		return blob{}, err
	}
	info, err := f.Stat()
	if err != nil {
		return blob{}, err
	}

	return blob{
		mediaType: MediaTypeLayer,
		digest:    "sha256:" + hex.EncodeToString(h.Sum(nil)),
		size:      info.Size(),
		path:      f.Name(),
		temp:      true,
	}, nil
}

func copyInto(tw *tar.Writer, e tarEntry) error {
	src, err := os.Open(e.src)
	if err != nil {
		return fmt.Errorf("oci: %w", err)
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	if err := writeHeader(tw, e.name, info.Size()); err != nil {
		return err
	}
	_, err = io.Copy(tw, src)
	return err
}

// writeHeader emits a fixed, timestamp-free tar header. Determinism is the point:
// with no mtime and no ownership the same inputs hash to the same image.
func writeHeader(tw *tar.Writer, name string, size int64) error {
	return tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     0o644,
		Size:     size,
		Format:   tar.FormatPAX,
	})
}

func writeFile(tw *tar.Writer, name string, data []byte) error {
	if err := writeHeader(tw, name, int64(len(data))); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}
