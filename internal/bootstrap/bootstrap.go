// Package bootstrap packages a static Linux binary into a deterministic,
// reproducible OCI Image Layout v1.0 tarball suitable for offline import into
// k3s containerd (`k3s ctr images import`) and direct push to Lazarette.
package bootstrap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"
	"time"
)

const (
	mediaTypeManifest  = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeIndex     = "application/vnd.oci.image.index.v1+json"
	mediaTypeConfig    = "application/vnd.oci.image.config.v1+json"
	mediaTypeLayerGzip = "application/vnd.oci.image.layer.v1.tar+gzip"

	nonRootUID = 65532
	nonRootGID = 65532
)

// Spec describes the static binary image to package.
type Spec struct {
	// Binary is the raw compiled ELF binary content. Required.
	Binary []byte
	// ImageRef is the repository reference recorded in index.json annotations,
	// for example "lazarette.clearedstack.svc.cluster.local:5000/lazarette".
	ImageRef string
	// Entrypoint is the absolute path of the binary inside the image rootfs,
	// for example "/lazarette".
	Entrypoint string
	// Arch is the OCI target architecture, for example "arm64".
	Arch string
}

// Result holds the deterministic OCI artifacts produced by Build.
type Result struct {
	ManifestDigest string
	ConfigDigest   string
	LayerDigest    string
	DiffID         string
	ManifestBytes  []byte
	ConfigBytes    []byte
	LayerBytes     []byte
	Tarball        []byte
}

type ociConfig struct {
	Created      string        `json:"created"`
	Architecture string        `json:"architecture"`
	OS           string        `json:"os"`
	Config       ociExecConfig `json:"config"`
	RootFS       ociRootFS     `json:"rootfs"`
}

type ociExecConfig struct {
	User       string   `json:"User"`
	Entrypoint []string `json:"Entrypoint"`
}

type ociRootFS struct {
	Type    string   `json:"type"`
	DiffIDs []string `json:"diff_ids"`
}

type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type ociManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Config        ociDescriptor   `json:"config"`
	Layers        []ociDescriptor `json:"layers"`
}

type ociIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []ociDescriptor `json:"manifests"`
}

// Build packages spec into a byte-for-byte reproducible OCI Image Layout
// tarball. Two calls with identical Spec values always produce identical bytes
// and identical sha256 digests.
func Build(spec Spec) (Result, error) {
	var res Result
	if len(spec.Binary) == 0 {
		return res, errors.New("binary is empty")
	}
	entry := path.Clean("/" + strings.TrimSpace(spec.Entrypoint))
	if entry == "/" || entry == "/." {
		return res, errors.New("entrypoint must name a file path")
	}
	arch := strings.TrimSpace(spec.Arch)
	if arch == "" {
		arch = "arm64"
	}
	ref := strings.TrimSpace(spec.ImageRef)
	if ref == "" {
		return res, errors.New("image reference is empty")
	}

	// 1. Build deterministic uncompressed USTAR layer containing the binary.
	relEntry := strings.TrimPrefix(entry, "/")
	rawLayer, err := buildBinaryTar(relEntry, spec.Binary)
	if err != nil {
		return res, err
	}
	res.DiffID = digestString(rawLayer)

	// 2. Compress the layer with deterministic gzip headers.
	gzLayer, err := gzipDeterministic(rawLayer)
	if err != nil {
		return res, err
	}
	res.LayerBytes = gzLayer
	res.LayerDigest = digestString(gzLayer)

	// 3. Marshal deterministic OCI image configuration.
	cfg := ociConfig{
		Created:      "1970-01-01T00:00:00Z",
		Architecture: arch,
		OS:           "linux",
		Config: ociExecConfig{
			User:       "65532:65532",
			Entrypoint: []string{entry},
		},
		RootFS: ociRootFS{
			Type:    "layers",
			DiffIDs: []string{res.DiffID},
		},
	}
	cfgBytes, err := json.Marshal(cfg)
	if err != nil {
		return res, err
	}
	res.ConfigBytes = cfgBytes
	res.ConfigDigest = digestString(cfgBytes)

	// 4. Marshal deterministic OCI image manifest.
	manifest := ociManifest{
		SchemaVersion: 2,
		MediaType:     mediaTypeManifest,
		Config: ociDescriptor{
			MediaType: mediaTypeConfig,
			Digest:    res.ConfigDigest,
			Size:      int64(len(cfgBytes)),
		},
		Layers: []ociDescriptor{
			{
				MediaType: mediaTypeLayerGzip,
				Digest:    res.LayerDigest,
				Size:      int64(len(gzLayer)),
			},
		},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return res, err
	}
	res.ManifestBytes = manifestBytes
	res.ManifestDigest = digestString(manifestBytes)

	// 5. Assemble OCI Image Layout v1.0 tar archive for containerd import.
	pinnedRef := ref + "@" + res.ManifestDigest
	idx := ociIndex{
		SchemaVersion: 2,
		MediaType:     mediaTypeIndex,
		Manifests: []ociDescriptor{
			{
				MediaType: mediaTypeManifest,
				Digest:    res.ManifestDigest,
				Size:      int64(len(manifestBytes)),
				Annotations: map[string]string{
					"io.containerd.image.name":          pinnedRef,
					"org.opencontainers.image.ref.name": pinnedRef,
				},
			},
		},
	}
	idxBytes, err := json.Marshal(idx)
	if err != nil {
		return res, err
	}
	layoutBytes := []byte("{\"imageLayoutVersion\":\"1.0.0\"}\n")

	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	files := []struct {
		name string
		data []byte
	}{
		{"oci-layout", layoutBytes},
		{"index.json", append(idxBytes, '\n')},
		{"blobs/sha256/" + trimAlgo(res.ConfigDigest), cfgBytes},
		{"blobs/sha256/" + trimAlgo(res.LayerDigest), gzLayer},
		{"blobs/sha256/" + trimAlgo(res.ManifestDigest), manifestBytes},
	}
	for _, f := range files {
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     f.name,
			Size:     int64(len(f.data)),
			Mode:     0o444,
			ModTime:  time.Unix(0, 0).UTC(),
			Format:   tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return res, err
		}
		if _, err := io.Copy(tw, bytes.NewReader(f.data)); err != nil {
			return res, err
		}
	}
	if err := tw.Close(); err != nil {
		return res, err
	}
	res.Tarball = out.Bytes()
	return res, nil
}

func buildBinaryTar(relPath string, binary []byte) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	parts := strings.Split(relPath, "/")
	for i := 1; i < len(parts); i++ {
		dir := strings.Join(parts[:i], "/") + "/"
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir,
			Name:     dir,
			Mode:     0o555,
			Uid:      nonRootUID,
			Gid:      nonRootGID,
			ModTime:  time.Unix(0, 0).UTC(),
			Format:   tar.FormatUSTAR,
		}); err != nil {
			return nil, err
		}
	}
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     relPath,
		Size:     int64(len(binary)),
		Mode:     0o555,
		Uid:      nonRootUID,
		Gid:      nonRootGID,
		ModTime:  time.Unix(0, 0).UTC(),
		Format:   tar.FormatUSTAR,
	}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(binary); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gzipDeterministic(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	zw.ModTime = time.Unix(0, 0).UTC()
	zw.OS = 255
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func digestString(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func trimAlgo(d string) string {
	return strings.TrimPrefix(d, "sha256:")
}
