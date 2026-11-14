package registry

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/ClearedStack/lazarette/internal/store"
)

// These tests exercise the full OCI distribution v2 push, sign, pull,
// retention and garbage-collection lifecycle over real HTTP on loopback using
// only the Go standard library.

type ociTestClient struct {
	t       *testing.T
	baseURL string
	http    *http.Client
}

func newTestServer(t *testing.T, dir string) (*store.Store, *ociTestClient) {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&Handler{Store: st})
	t.Cleanup(srv.Close)
	return st, &ociTestClient{t: t, baseURL: srv.URL, http: srv.Client()}
}

func shaDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (c *ociTestClient) pushBlobMonolithic(repo string, data []byte) string {
	c.t.Helper()
	d := shaDigest(data)
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/v2/"+repo+"/blobs/uploads/?digest="+d, bytes.NewReader(data))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(res.Body)
		c.t.Fatalf("monolithic push %s: status %d: %s", d, res.StatusCode, body)
	}
	if got := res.Header.Get("Docker-Content-Digest"); got != d {
		c.t.Fatalf("Docker-Content-Digest = %q, want %q", got, d)
	}
	return d
}

func (c *ociTestClient) pushBlobChunked(repo string, data []byte, chunkSize int) string {
	c.t.Helper()
	d := shaDigest(data)
	res, err := c.http.Post(c.baseURL+"/v2/"+repo+"/blobs/uploads/", "application/octet-stream", nil)
	if err != nil {
		c.t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		c.t.Fatalf("start chunked upload: status %d", res.StatusCode)
	}
	loc := res.Header.Get("Location")

	for off := 0; off < len(data); off += chunkSize {
		end := min(off+chunkSize, len(data))
		chunk := data[off:end]
		req, err := http.NewRequest(http.MethodPatch, c.baseURL+loc, bytes.NewReader(chunk))
		if err != nil {
			c.t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Content-Range", fmt.Sprintf("%d-%d", off, end-1))
		req.ContentLength = int64(len(chunk))
		pres, err := c.http.Do(req)
		if err != nil {
			c.t.Fatal(err)
		}
		pres.Body.Close()
		if pres.StatusCode != http.StatusAccepted {
			c.t.Fatalf("PATCH %d-%d: status %d", off, end-1, pres.StatusCode)
		}
		loc = pres.Header.Get("Location")
	}

	req, err := http.NewRequest(http.MethodPut, c.baseURL+loc+"?digest="+d, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	fres, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	fres.Body.Close()
	if fres.StatusCode != http.StatusCreated {
		c.t.Fatalf("finish upload: status %d", fres.StatusCode)
	}
	return d
}

func (c *ociTestClient) pushManifest(repo, ref, mediaType string, raw []byte) (string, http.Header) {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodPut, c.baseURL+"/v2/"+repo+"/manifests/"+ref, bytes.NewReader(raw))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", mediaType)
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		c.t.Fatalf("PUT manifest %s: status %d: %s", ref, res.StatusCode, body)
	}
	return res.Header.Get("Docker-Content-Digest"), res.Header
}

func (c *ociTestClient) getBytes(path string) (int, http.Header, []byte) {
	c.t.Helper()
	res, err := c.http.Get(c.baseURL + path)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return res.StatusCode, res.Header, body
}

func buildMadeUpImage(c *ociTestClient, repo, tagSeed string) (manifestDigest string, manifestBytes []byte, layerDigests []string) {
	c.t.Helper()
	cfg := []byte(fmt.Sprintf(`{"architecture":"arm64","os":"linux","config":{"Entrypoint":["/bin/%s"]}}`, tagSeed))
	cfgDigest := c.pushBlobMonolithic(repo, cfg)

	l1 := bytes.Repeat([]byte("layer-one-"+tagSeed+"-"), 600)
	l2 := bytes.Repeat([]byte("layer-two-"+tagSeed+"-"), 900)
	d1 := c.pushBlobChunked(repo, l1, 1024)
	d2 := c.pushBlobMonolithic(repo, l2)

	manifest := fmt.Sprintf(`{
  "schemaVersion": 2,
  "mediaType": %q,
  "config": {"mediaType": "application/vnd.oci.image.config.v1+json", "digest": %q, "size": %d},
  "layers": [
    {"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": %q, "size": %d},
    {"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": %q, "size": %d}
  ]
}`, MediaTypeImageManifest, cfgDigest, len(cfg), d1, len(l1), d2, len(l2))
	raw := []byte(manifest)
	wantDigest := shaDigest(raw)
	gotDigest, _ := c.pushManifest(repo, wantDigest, MediaTypeImageManifest, raw)
	if gotDigest != wantDigest {
		c.t.Fatalf("manifest digest = %q, want %q", gotDigest, wantDigest)
	}
	return wantDigest, raw, []string{cfgDigest, d1, d2}
}

func signWithCosignTag(c *ociTestClient, repo, targetDigest string, targetSize int, seed string) string {
	c.t.Helper()
	emptyCfg := []byte(`{}`)
	cfgDigest := c.pushBlobMonolithic(repo, emptyCfg)
	sigPayload := []byte(fmt.Sprintf(`{"critical":{"identity":{"docker-reference":%q},"image":{"docker-manifest-digest":%q},"type":"cosign container image signature"},"optional":{"seed":%q}}`, repo, targetDigest, seed))
	sigLayer := c.pushBlobMonolithic(repo, sigPayload)

	sigManifest := []byte(fmt.Sprintf(`{
  "schemaVersion": 2,
  "mediaType": %q,
  "config": {"mediaType": "application/vnd.oci.image.config.v1+json", "digest": %q, "size": %d},
  "layers": [
    {"mediaType": "application/vnd.dev.cosign.simplesigning.v1+json", "digest": %q, "size": %d}
  ]
}`, MediaTypeImageManifest, cfgDigest, len(emptyCfg), sigLayer, len(sigPayload)))
	tag := "sha256-" + targetDigest[len("sha256:"):] + ".sig"
	sigDigest, _ := c.pushManifest(repo, tag, MediaTypeImageManifest, sigManifest)
	_ = targetSize
	return sigDigest
}

func TestLoopbackMultiArchPushSignPullAndRestart(t *testing.T) {
	dir := t.TempDir()
	_, client := newTestServer(t, dir)

	armDigest, armBytes, _ := buildMadeUpImage(client, "lazarette", "arm64-v1")
	amdDigest, amdBytes, _ := buildMadeUpImage(client, "lazarette", "amd64-v1")

	indexRaw := []byte(fmt.Sprintf(`{
  "schemaVersion": 2,
  "mediaType": %q,
  "manifests": [
    {"mediaType": %q, "digest": %q, "size": %d},
    {"mediaType": %q, "digest": %q, "size": %d}
  ]
}`, MediaTypeImageIndex, MediaTypeImageManifest, armDigest, len(armBytes), MediaTypeImageManifest, amdDigest, len(amdBytes)))
	indexDigest := shaDigest(indexRaw)
	gotIndexDigest, _ := client.pushManifest("lazarette", indexDigest, MediaTypeImageIndex, indexRaw)
	if gotIndexDigest != indexDigest {
		t.Fatalf("index digest = %q, want %q", gotIndexDigest, indexDigest)
	}

	// Sign the image index via OCI v1.1 Referrers (cosign bundle mediaType).
	emptyCfg := []byte(`{}`)
	cfgD := client.pushBlobMonolithic("lazarette", emptyCfg)
	bundle := []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","content":"made-up-sig"}`)
	bundleD := client.pushBlobMonolithic("lazarette", bundle)
	refManifest := []byte(fmt.Sprintf(`{
  "schemaVersion": 2,
  "mediaType": %q,
  "artifactType": "application/vnd.dev.sigstore.bundle.v0.3+json",
  "config": {"mediaType": "application/vnd.oci.empty.v1+json", "digest": %q, "size": %d},
  "layers": [
    {"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json", "digest": %q, "size": %d}
  ],
  "subject": {"mediaType": %q, "digest": %q, "size": %d},
  "annotations": {"org.opencontainers.image.created": "2026-01-05T09:00:00Z"}
}`, MediaTypeImageManifest, cfgD, len(emptyCfg), bundleD, len(bundle), MediaTypeImageIndex, indexDigest, len(indexRaw)))
	refDigest := shaDigest(refManifest)
	_, hdr := client.pushManifest("lazarette", refDigest, MediaTypeImageManifest, refManifest)
	if gotSubj := hdr.Get("OCI-Subject"); gotSubj != indexDigest {
		t.Errorf("OCI-Subject = %q, want %q", gotSubj, indexDigest)
	}

	// Start a second server on the same directory to verify persistence across restarts.
	_, restarted := newTestServer(t, dir)

	status, mhdr, gotIndex := restarted.getBytes("/v2/lazarette/manifests/" + indexDigest)
	if status != http.StatusOK || mhdr.Get("Content-Type") != MediaTypeImageIndex || !bytes.Equal(gotIndex, indexRaw) {
		t.Fatalf("pulled index mismatch: status=%d content-type=%q", status, mhdr.Get("Content-Type"))
	}

	status, rHdr, refBody := restarted.getBytes("/v2/lazarette/referrers/" + indexDigest + "?artifactType=application/vnd.dev.sigstore.bundle.v0.3%2Bjson")
	if status != http.StatusOK || rHdr.Get("OCI-Filters-Applied") != "artifactType" {
		t.Fatalf("referrers status=%d filters=%q body=%s", status, rHdr.Get("OCI-Filters-Applied"), refBody)
	}
	var parsedRefs struct {
		Manifests []store.ReferrerEntry `json:"manifests"`
	}
	if err := json.Unmarshal(refBody, &parsedRefs); err != nil || len(parsedRefs.Manifests) != 1 || parsedRefs.Manifests[0].Digest != refDigest {
		t.Fatalf("unexpected referrers: %s (%v)", refBody, err)
	}

	status, _, digBody := restarted.getBytes("/v2/lazarette/digests/list")
	if status != http.StatusOK {
		t.Fatalf("digests/list status=%d", status)
	}
	var digList struct {
		Name    string   `json:"name"`
		Digests []string `json:"digests"`
	}
	if err := json.Unmarshal(digBody, &digList); err != nil {
		t.Fatal(err)
	}
	if digList.Name != "lazarette" || !reflect.DeepEqual(digList.Digests, []string{indexDigest}) {
		t.Errorf("digests/list = %+v, want [indexDigest]", digList)
	}

	for _, d := range []string{"uploads", "tmp"} {
		left, err := os.ReadDir(filepath.Join(dir, d))
		if err != nil || len(left) != 0 {
			t.Errorf("%s leftover entries: %d (%v)", d, len(left), err)
		}
	}
}

func TestLoopbackFiveDigestRetentionRollbackAndGarbageCollection(t *testing.T) {
	dir := t.TempDir()
	st, client := newTestServer(t, dir)

	var allDigests []string
	var allRaw [][]byte
	for i := 0; i < 7; i++ {
		d, raw, _ := buildMadeUpImage(client, "apps/assay", "release-"+strconv.Itoa(i))
		signWithCosignTag(client, "apps/assay", d, len(raw), strconv.Itoa(i))
		allDigests = append(allDigests, d)
		allRaw = append(allRaw, raw)
	}

	// Push an unsigned 8th image; it must not enter the retained signed index.
	unsignedDigest, _, _ := buildMadeUpImage(client, "apps/assay", "unsigned-candidate")

	status, _, body := client.getBytes("/v2/apps/assay/digests/list")
	if status != http.StatusOK {
		t.Fatalf("digests/list: %d", status)
	}
	var listed struct {
		Name    string   `json:"name"`
		Digests []string `json:"digests"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	wantRetained := allDigests[2:7]
	if !reflect.DeepEqual(listed.Digests, wantRetained) {
		t.Fatalf("retained digests = %v, want %v", listed.Digests, wantRetained)
	}

	// Run daily GC (grace=0 so deterministic immediate cleanup runs).
	stats, err := st.CollectGarbage(time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC), 0)
	if err != nil {
		t.Fatalf("CollectGarbage: %v", err)
	}
	if stats.ManifestsRemoved == 0 || stats.BlobsRemoved == 0 {
		t.Fatalf("expected GC to prune aged and unsigned images, got %+v", stats)
	}

	// Releases 0 and 1, plus the unsigned 8th image, are pruned.
	for _, pruned := range []string{allDigests[0], allDigests[1], unsignedDigest} {
		stCode, _, _ := client.getBytes("/v2/apps/assay/manifests/" + pruned)
		if stCode != http.StatusNotFound {
			t.Errorf("expected pruned manifest %s to be 404, got %d", pruned, stCode)
		}
	}

	// Releases 2..6 remain pullable byte-for-byte with their cosign .sig tags for instant rollback.
	for i := 2; i < 7; i++ {
		stCode, _, got := client.getBytes("/v2/apps/assay/manifests/" + allDigests[i])
		if stCode != http.StatusOK || !bytes.Equal(got, allRaw[i]) {
			t.Errorf("retained release %d not pullable after GC: status=%d", i, stCode)
		}
		sigTag := "sha256-" + allDigests[i][len("sha256:"):] + ".sig"
		sigCode, _, _ := client.getBytes("/v2/apps/assay/manifests/" + sigTag)
		if sigCode != http.StatusOK {
			t.Errorf("retained release %d .sig tag missing after GC: status=%d", i, sigCode)
		}
	}
}

func TestLoopbackMutableTagPushAndPullRefused(t *testing.T) {
	_, client := newTestServer(t, t.TempDir())
	req, _ := http.NewRequest(http.MethodPut, client.baseURL+"/v2/apps/assay/manifests/latest", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", MediaTypeImageManifest)
	res, err := client.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("PUT :latest status = %d, want 400", res.StatusCode)
	}
}

// TestKeelworksAndShipkeeperWireCompatibility drives Lazarette with the exact
// HTTP calls and ECDSA P-256 SimpleSigning verification rules used by
// ClearedStack/keelworks (internal/lazarette) and ClearedStack/shipkeeper
// (internal/lazarette), covering both classic cosign sha256-<hex>.sig pushes
// and OCI 1.1 subject referrer pushes.
func TestKeelworksAndShipkeeperWireCompatibility(t *testing.T) {
	_, client := newTestServer(t, t.TempDir())

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	block, _ := pem.Decode(pubPEM)
	parsedPub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pub := parsedPub.(*ecdsa.PublicKey)

	const (
		simpleSigningMediaType = "application/vnd.dev.cosign.simplesigning.v1+json"
		sigAnnotationKey       = "dev.cosignproject.cosign/signature"
	)

	// Exact verification flow from ClearedStack/keelworks/internal/lazarette.
	keelworksVerify := func(repo, imgDigest string) bool {
		headReq, _ := http.NewRequest(http.MethodHead, client.baseURL+"/v2/"+repo+"/manifests/"+imgDigest, nil)
		headReq.Header.Set("Accept", MediaTypeImageManifest+", "+MediaTypeImageIndex)
		headRes, err := client.http.Do(headReq)
		if err != nil {
			t.Fatal(err)
		}
		headRes.Body.Close()
		if headRes.StatusCode != http.StatusOK || headRes.Header.Get("Docker-Content-Digest") != imgDigest {
			return false
		}

		refPath := "/v2/" + repo + "/referrers/" + imgDigest + "?artifactType=" + url.QueryEscape(simpleSigningMediaType)
		st, _, refBody := client.getBytes(refPath)
		if st != http.StatusOK {
			return false
		}
		var idx struct {
			Manifests []struct {
				Digest       string `json:"digest"`
				ArtifactType string `json:"artifactType"`
			} `json:"manifests"`
		}
		if err := json.Unmarshal(refBody, &idx); err != nil {
			t.Fatal(err)
		}
		for _, desc := range idx.Manifests {
			if desc.ArtifactType != "" && desc.ArtifactType != simpleSigningMediaType {
				continue
			}
			mst, _, mbody := client.getBytes("/v2/" + repo + "/manifests/" + desc.Digest)
			if mst != http.StatusOK || shaDigest(mbody) != desc.Digest {
				continue
			}
			var m struct {
				SchemaVersion int `json:"schemaVersion"`
				Layers        []struct {
					MediaType   string            `json:"mediaType"`
					Digest      string            `json:"digest"`
					Annotations map[string]string `json:"annotations"`
				} `json:"layers"`
			}
			if json.Unmarshal(mbody, &m) != nil || m.SchemaVersion != 2 {
				continue
			}
			for _, layer := range m.Layers {
				if layer.MediaType != simpleSigningMediaType {
					continue
				}
				sigBytes, err := base64.StdEncoding.DecodeString(layer.Annotations[sigAnnotationKey])
				if err != nil || len(sigBytes) == 0 {
					continue
				}
				bst, _, payload := client.getBytes("/v2/" + repo + "/blobs/" + layer.Digest)
				if bst != http.StatusOK || shaDigest(payload) != layer.Digest {
					continue
				}
				sum := sha256.Sum256(payload)
				if !ecdsa.VerifyASN1(pub, sum[:], sigBytes) {
					continue
				}
				var ss struct {
					Critical struct {
						Image struct {
							DockerManifestDigest string `json:"docker-manifest-digest"`
						} `json:"image"`
						Type string `json:"type"`
					} `json:"critical"`
				}
				if json.Unmarshal(payload, &ss) == nil &&
					ss.Critical.Type == "cosign container image signature" &&
					ss.Critical.Image.DockerManifestDigest == imgDigest {
					return true
				}
			}
		}
		return false
	}

	// Exact HEAD check from ClearedStack/shipkeeper/internal/lazarette.
	shipkeeperCheck := func(repo, imgDigest string) bool {
		req, _ := http.NewRequest(http.MethodHead, client.baseURL+"/v2/"+repo+"/manifests/"+imgDigest, nil)
		req.Header.Set("Accept", MediaTypeImageManifest+", "+MediaTypeImageIndex)
		res, err := client.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode == http.StatusOK && res.Header.Get("Docker-Content-Digest") == imgDigest
	}

	makeSignedPayload := func(key *ecdsa.PrivateKey, repo, signedDigest string) (string, int, string) {
		payload := []byte(fmt.Sprintf(
			`{"critical":{"identity":{"docker-reference":"lazarette.clearedstack.svc.cluster.local:5000/%s"},"image":{"docker-manifest-digest":%q},"type":"cosign container image signature"},"optional":null}`,
			repo, signedDigest,
		))
		sum := sha256.Sum256(payload)
		sigASN1, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		blobDigest := client.pushBlobMonolithic(repo, payload)
		return blobDigest, len(payload), base64.StdEncoding.EncodeToString(sigASN1)
	}

	// 1. Classic cosign sha256-<hex>.sig tag push (no subject field in JSON).
	img1Digest, _, _ := buildMadeUpImage(client, "muster", "v1-binary")
	if keelworksVerify("muster", img1Digest) {
		t.Fatal("unsigned image verified before cosign push")
	}
	emptyCfg := []byte(`{}`)
	emptyCfgDigest := client.pushBlobMonolithic("muster", emptyCfg)
	sig1Blob, sig1Len, sig1B64 := makeSignedPayload(priv, "muster", img1Digest)
	sig1Manifest := []byte(fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":%q,"digest":%q,"size":%d,"annotations":{%q:%q}}]}`,
		MediaTypeImageManifest, emptyCfgDigest, len(emptyCfg),
		simpleSigningMediaType, sig1Blob, sig1Len, sigAnnotationKey, sig1B64,
	))
	sig1Tag := "sha256-" + img1Digest[len("sha256:"):] + ".sig"
	client.pushManifest("muster", sig1Tag, MediaTypeImageManifest, sig1Manifest)

	if !keelworksVerify("muster", img1Digest) {
		t.Fatal("Keelworks OCIVerifier failed to verify classic cosign .sig tag push")
	}
	if !shipkeeperCheck("muster", img1Digest) {
		t.Fatal("Shipkeeper HasSignedDigest failed on classic signed image")
	}

	// 2. OCI 1.1 subject referrer push with artifactType.
	img2Digest, img2Raw, _ := buildMadeUpImage(client, "muster", "v2-binary")
	sig2Blob, sig2Len, sig2B64 := makeSignedPayload(priv, "muster", img2Digest)
	sig2Manifest := []byte(fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":%q,"artifactType":%q,"config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":%q,"digest":%q,"size":%d,"annotations":{%q:%q}}],"subject":{"mediaType":%q,"digest":%q,"size":%d}}`,
		MediaTypeImageManifest, simpleSigningMediaType, emptyCfgDigest, len(emptyCfg),
		simpleSigningMediaType, sig2Blob, sig2Len, sigAnnotationKey, sig2B64,
		MediaTypeImageManifest, img2Digest, len(img2Raw),
	))
	client.pushManifest("muster", shaDigest(sig2Manifest), MediaTypeImageManifest, sig2Manifest)

	if !keelworksVerify("muster", img2Digest) {
		t.Fatal("Keelworks OCIVerifier failed to verify OCI 1.1 subject referrer push")
	}
	if !shipkeeperCheck("muster", img2Digest) {
		t.Fatal("Shipkeeper HasSignedDigest failed on OCI 1.1 signed image")
	}

	// 3. Signature signed by an unknown key is stored by OCI rules, yet
	// Keelworks cryptographic verification rejects it cleanly.
	wrongPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	img3Digest, _, _ := buildMadeUpImage(client, "muster", "v3-rogue")
	badBlob, badLen, badB64 := makeSignedPayload(wrongPriv, "muster", img3Digest)
	badSigManifest := []byte(fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":%q,"digest":%q,"size":%d,"annotations":{%q:%q}}]}`,
		MediaTypeImageManifest, emptyCfgDigest, len(emptyCfg),
		simpleSigningMediaType, badBlob, badLen, sigAnnotationKey, badB64,
	))
	badTag := "sha256-" + img3Digest[len("sha256:"):] + ".sig"
	client.pushManifest("muster", badTag, MediaTypeImageManifest, badSigManifest)
	if keelworksVerify("muster", img3Digest) {
		t.Fatal("Keelworks OCIVerifier accepted signature from wrong ECDSA key")
	}
}
