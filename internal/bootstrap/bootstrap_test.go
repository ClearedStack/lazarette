package bootstrap_test

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ClearedStack/lazarette/internal/bootstrap"
	"github.com/ClearedStack/lazarette/internal/registry"
	"github.com/ClearedStack/lazarette/internal/store"
)

func TestBuildIsReproducibleAndAcceptedByLazarette(t *testing.T) {
	spec := bootstrap.Spec{
		Binary:     []byte("\x7fELF-fake-static-arm64-lazarette-binary"),
		ImageRef:   "lazarette.clearedstack.svc.cluster.local:5000/lazarette",
		Entrypoint: "/lazarette",
		Arch:       "arm64",
	}

	first, err := bootstrap.Build(spec)
	if err != nil {
		t.Fatalf("first Build: %v", err)
	}
	second, err := bootstrap.Build(spec)
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if first.ManifestDigest != second.ManifestDigest || !bytes.Equal(first.Tarball, second.Tarball) {
		t.Fatal("Build is not byte-for-byte deterministic")
	}

	// Verify OCI Image Layout entries inside the tarball.
	entries := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(first.Tarball))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read tar entry %s: %v", hdr.Name, err)
		}
		entries[hdr.Name] = data
	}
	if _, ok := entries["oci-layout"]; !ok {
		t.Fatal("missing oci-layout in tarball")
	}
	var idx struct {
		Manifests []struct {
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(entries["index.json"], &idx); err != nil || len(idx.Manifests) != 1 {
		t.Fatalf("invalid index.json: %v", err)
	}
	if idx.Manifests[0].Digest != first.ManifestDigest {
		t.Fatalf("index digest = %s, want %s", idx.Manifests[0].Digest, first.ManifestDigest)
	}
	wantRef := spec.ImageRef + "@" + first.ManifestDigest
	if idx.Manifests[0].Annotations["io.containerd.image.name"] != wantRef {
		t.Fatalf("containerd annotation = %q, want %q", idx.Manifests[0].Annotations["io.containerd.image.name"], wantRef)
	}

	// Push the unpacked blobs and manifest straight into a Lazarette server to
	// prove every digest, mediaType and size conforms to Lazarette's OCI rules.
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&registry.Handler{Store: st})
	defer srv.Close()

	for _, blob := range []struct {
		digest string
		data   []byte
	}{
		{first.ConfigDigest, first.ConfigBytes},
		{first.LayerDigest, first.LayerBytes},
	} {
		res, err := srv.Client().Post(
			srv.URL+"/v2/lazarette/blobs/uploads/?digest="+blob.digest,
			"application/octet-stream",
			bytes.NewReader(blob.data),
		)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("push blob %s: status %d, want 201", blob.digest, res.StatusCode)
		}
	}

	req, err := http.NewRequest(
		http.MethodPut,
		srv.URL+"/v2/lazarette/manifests/"+first.ManifestDigest,
		bytes.NewReader(first.ManifestBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", registry.MediaTypeImageManifest)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("push bootstrap manifest: status %d, want 201", res.StatusCode)
	}
}
