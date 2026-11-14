package bootstrap_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http/httptest"
	"net/url"
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
}

func TestPushAndSignProducesVerifiedRetainedDigest(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&registry.Handler{Store: st})
	defer srv.Close()

	rawKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8DER, err := x509.MarshalPKCS8PrivateKey(rawKey)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8PEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8DER})
	sec1DER, err := x509.MarshalECPrivateKey(rawKey)
	if err != nil {
		t.Fatal(err)
	}
	sec1PEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1DER})

	if _, err := bootstrap.ParsePrivateKey(pkcs8PEM); err != nil {
		t.Fatalf("ParsePrivateKey PKCS#8: %v", err)
	}
	priv, err := bootstrap.ParsePrivateKey(sec1PEM)
	if err != nil {
		t.Fatalf("ParsePrivateKey SEC 1: %v", err)
	}

	res, err := bootstrap.Build(bootstrap.Spec{
		Binary:     []byte("\x7fELF-static-shipkeeper-arm64"),
		ImageRef:   "lazarette.clearedstack.svc.cluster.local:5000/shipkeeper",
		Entrypoint: "/shipkeeper",
		Arch:       "arm64",
	})
	if err != nil {
		t.Fatal(err)
	}

	refDigest, err := bootstrap.PushAndSign(context.Background(), bootstrap.PushOptions{
		BaseURL:    srv.URL,
		HTTP:       srv.Client(),
		DockerRef:  "lazarette.clearedstack.svc.cluster.local:5000/shipkeeper",
		Result:     res,
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("PushAndSign: %v", err)
	}
	if refDigest == "" {
		t.Fatal("expected non-empty referrer digest")
	}

	// Verify the image entered the 5-signed-digest retention list.
	digRes, err := srv.Client().Get(srv.URL + "/v2/shipkeeper/digests/list")
	if err != nil {
		t.Fatal(err)
	}
	defer digRes.Body.Close()
	var retained struct {
		Name    string   `json:"name"`
		Digests []string `json:"digests"`
	}
	if err := json.NewDecoder(digRes.Body).Decode(&retained); err != nil {
		t.Fatal(err)
	}
	if len(retained.Digests) != 1 || retained.Digests[0] != res.ManifestDigest {
		t.Fatalf("retained digests = %v, want [%s]", retained.Digests, res.ManifestDigest)
	}

	// Verify ECDSA P-256 SimpleSigning payload via OCI 1.1 /referrers/<digest>
	// using the exact checks performed by Keelworks and Shipkeeper.
	refQuery := srv.URL + "/v2/shipkeeper/referrers/" + res.ManifestDigest + "?artifactType=" + url.QueryEscape("application/vnd.dev.cosign.simplesigning.v1+json")
	rRes, err := srv.Client().Get(refQuery)
	if err != nil {
		t.Fatal(err)
	}
	defer rRes.Body.Close()
	var refIdx struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.NewDecoder(rRes.Body).Decode(&refIdx); err != nil || len(refIdx.Manifests) != 1 {
		t.Fatalf("unexpected referrers index: err=%v len=%d", err, len(refIdx.Manifests))
	}

	mRes, err := srv.Client().Get(srv.URL + "/v2/shipkeeper/manifests/" + refIdx.Manifests[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer mRes.Body.Close()
	var sigManifest struct {
		Layers []struct {
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"layers"`
	}
	if err := json.NewDecoder(mRes.Body).Decode(&sigManifest); err != nil || len(sigManifest.Layers) != 1 {
		t.Fatalf("decode referrer manifest: %v", err)
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sigManifest.Layers[0].Annotations["dev.cosignproject.cosign/signature"])
	if err != nil {
		t.Fatal(err)
	}
	bRes, err := srv.Client().Get(srv.URL + "/v2/shipkeeper/blobs/" + sigManifest.Layers[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(bRes.Body)
	bRes.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	if !ecdsa.VerifyASN1(&priv.PublicKey, sum[:], sigBytes) {
		t.Fatal("ECDSA P-256 signature verification failed")
	}
}
