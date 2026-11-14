package bootstrap

import (
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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	mediaTypeEmptyConfig   = "application/vnd.oci.empty.v1+json"
	mediaTypeSimpleSigning = "application/vnd.dev.cosign.simplesigning.v1+json"
	signatureAnnotation    = "dev.cosignproject.cosign/signature"
	cosignPayloadType      = "cosign container image signature"
)

// ParsePrivateKey parses a PEM-encoded ECDSA P-256 private key in PKCS#8
// ("PRIVATE KEY") or SEC 1 ("EC PRIVATE KEY") format.
func ParsePrivateKey(pemData []byte) (*ecdsa.PrivateKey, error) {
	block, rest := pem.Decode(pemData)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("cosign private key must be a single PEM block")
	}
	var priv *ecdsa.PrivateKey
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("cosign private key is not valid PKCS#8")
		}
		k, ok := parsed.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("cosign private key must be ECDSA P-256")
		}
		priv = k
	case "EC PRIVATE KEY":
		k, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("cosign private key is not valid SEC 1 EC")
		}
		priv = k
	default:
		return nil, errors.New("cosign private key has an unfamiliar PEM block type")
	}
	if priv.Curve != elliptic.P256() {
		return nil, errors.New("cosign private key must use curve P-256")
	}
	return priv, nil
}

// RepoFromRef extracts the repository path from an image reference such as
// "lazarette.clearedstack.svc.cluster.local:5000/lazarette" or "apps/assay".
func RepoFromRef(imageRef string) (string, error) {
	ref := strings.TrimSpace(imageRef)
	if ref == "" {
		return "", errors.New("image reference is empty")
	}
	if at := strings.IndexByte(ref, '@'); at >= 0 {
		ref = ref[:at]
	}
	first, rest, hasSlash := strings.Cut(ref, "/")
	if !hasSlash {
		return first, nil
	}
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		if rest == "" {
			return "", errors.New("image reference has host without repository path")
		}
		return rest, nil
	}
	return ref, nil
}

// PushOptions configures PushAndSign against a running Lazarette endpoint.
type PushOptions struct {
	BaseURL    string
	HTTP       *http.Client
	Timeout    time.Duration
	Repo       string
	DockerRef  string
	Result     Result
	PrivateKey *ecdsa.PrivateKey
}

type simpleSigningDoc struct {
	Critical simpleSigningCritical `json:"critical"`
	Optional any                   `json:"optional"`
}

type simpleSigningCritical struct {
	Identity simpleSigningIdentity `json:"identity"`
	Image    simpleSigningImage    `json:"image"`
	Type     string                `json:"type"`
}

type simpleSigningIdentity struct {
	DockerReference string `json:"docker-reference"`
}

type simpleSigningImage struct {
	DockerManifestDigest string `json:"docker-manifest-digest"`
}

type referrerManifestDoc struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	ArtifactType  string          `json:"artifactType"`
	Config        ociDescriptor   `json:"config"`
	Layers        []ociDescriptor `json:"layers"`
	Subject       ociDescriptor   `json:"subject"`
}

// SignSimpleSigning builds and signs a canonical cosign SimpleSigning JSON
// payload for dockerRef at manifestDigest using priv.
func SignSimpleSigning(priv *ecdsa.PrivateKey, dockerRef, manifestDigest string) ([]byte, string, error) {
	if priv == nil || priv.Curve != elliptic.P256() {
		return nil, "", errors.New("cosign private key must be ECDSA P-256")
	}
	if strings.TrimSpace(dockerRef) == "" || !strings.HasPrefix(manifestDigest, "sha256:") {
		return nil, "", errors.New("docker reference and sha256 manifest digest are required")
	}
	doc := simpleSigningDoc{
		Critical: simpleSigningCritical{
			Identity: simpleSigningIdentity{DockerReference: dockerRef},
			Image:    simpleSigningImage{DockerManifestDigest: manifestDigest},
			Type:     cosignPayloadType,
		},
		Optional: nil,
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(payload)
	sigASN1, err := ecdsa.SignASN1(rand.Reader, priv, sum[:])
	if err != nil {
		return nil, "", err
	}
	return payload, base64.StdEncoding.EncodeToString(sigASN1), nil
}

// PushAndSign uploads opts.Result into Lazarette at opts.BaseURL and, when
// opts.PrivateKey is non-nil, signs the manifest digest and commits an OCI v1.1
// SimpleSigning referrer manifest. It returns the referrer manifest digest when
// signed, or an empty string when unsigned.
func PushAndSign(ctx context.Context, opts PushOptions) (string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("lazarette base URL must be http or https with a host")
	}
	repo := strings.TrimSpace(opts.Repo)
	if repo == "" {
		repo, err = RepoFromRef(opts.DockerRef)
		if err != nil {
			return "", err
		}
	}
	dockerRef := strings.TrimSpace(opts.DockerRef)
	if dockerRef == "" {
		dockerRef = u.Host + "/" + repo
	}
	if len(opts.Result.ManifestBytes) == 0 || len(opts.Result.ConfigBytes) == 0 || len(opts.Result.LayerBytes) == 0 {
		return "", errors.New("bootstrap Result is incomplete")
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	var hc http.Client
	if opts.HTTP != nil {
		hc = *opts.HTTP
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	base := u.String()
	if err := ensureBlob(callCtx, &hc, base, repo, opts.Result.ConfigDigest, opts.Result.ConfigBytes); err != nil {
		return "", err
	}
	if err := ensureBlob(callCtx, &hc, base, repo, opts.Result.LayerDigest, opts.Result.LayerBytes); err != nil {
		return "", err
	}
	if err := putManifest(callCtx, &hc, base, repo, opts.Result.ManifestDigest, opts.Result.ManifestBytes); err != nil {
		return "", err
	}

	if opts.PrivateKey == nil {
		return "", nil
	}

	payload, sigB64, err := SignSimpleSigning(opts.PrivateKey, dockerRef, opts.Result.ManifestDigest)
	if err != nil {
		return "", err
	}
	emptyCfg := []byte("{}")
	emptyCfgDigest := digestString(emptyCfg)
	if err := ensureBlob(callCtx, &hc, base, repo, emptyCfgDigest, emptyCfg); err != nil {
		return "", err
	}
	payloadDigest := digestString(payload)
	if err := ensureBlob(callCtx, &hc, base, repo, payloadDigest, payload); err != nil {
		return "", err
	}

	refDoc := referrerManifestDoc{
		SchemaVersion: 2,
		MediaType:     mediaTypeManifest,
		ArtifactType:  mediaTypeSimpleSigning,
		Config: ociDescriptor{
			MediaType: mediaTypeEmptyConfig,
			Digest:    emptyCfgDigest,
			Size:      int64(len(emptyCfg)),
		},
		Layers: []ociDescriptor{{
			MediaType: mediaTypeSimpleSigning,
			Digest:    payloadDigest,
			Size:      int64(len(payload)),
			Annotations: map[string]string{
				signatureAnnotation: sigB64,
			},
		}},
		Subject: ociDescriptor{
			MediaType: mediaTypeManifest,
			Digest:    opts.Result.ManifestDigest,
			Size:      int64(len(opts.Result.ManifestBytes)),
		},
	}
	refBytes, err := json.Marshal(refDoc)
	if err != nil {
		return "", err
	}
	refDigest := digestString(refBytes)
	if err := putManifest(callCtx, &hc, base, repo, refDigest, refBytes); err != nil {
		return "", err
	}
	return refDigest, nil
}

func ensureBlob(ctx context.Context, hc *http.Client, base, repo, digest string, data []byte) error {
	headReq, err := http.NewRequestWithContext(ctx, http.MethodHead, base+"/v2/"+repo+"/blobs/"+digest, nil)
	if err != nil {
		return err
	}
	headRes, err := hc.Do(headReq)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(headRes.Body, 4096))
	headRes.Body.Close()
	if headRes.StatusCode == http.StatusOK {
		return nil
	}

	postURL := base + "/v2/" + repo + "/blobs/uploads/?digest=" + url.QueryEscape(digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, postURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(data))
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		return fmt.Errorf("upload blob %s: unexpected status %d", digest, res.StatusCode)
	}
	return nil
}

func putManifest(ctx context.Context, hc *http.Client, base, repo, digest string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/v2/"+repo+"/manifests/"+digest, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mediaTypeManifest)
	req.ContentLength = int64(len(data))
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	res.Body.Close()
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
		return fmt.Errorf("put manifest %s: unexpected status %d", digest, res.StatusCode)
	}
	return nil
}
