package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClearedStack/lazarette/internal/registry"
	"github.com/ClearedStack/lazarette/internal/store"
)

func TestBootstrapCLI(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), nil, &stdout, &stderr); code != 2 {
		t.Fatalf("missing flags: exit %d, want 2", code)
	}

	dir := t.TempDir()
	binFile := filepath.Join(dir, "lazarette")
	outFile := filepath.Join(dir, "lazarette.tar")
	keyFile := filepath.Join(dir, "cosign.key")
	if err := os.WriteFile(binFile, []byte("\x7fELF-static-arm64"), 0o755); err != nil {
		t.Fatal(err)
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(dir, "registry"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&registry.Handler{Store: st})
	defer srv.Close()

	stdout.Reset()
	stderr.Reset()
	code := run(context.Background(), []string{
		"-binary", binFile,
		"-out", outFile,
		"-push", srv.URL,
		"-sign-key", keyFile,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", code, stderr.String())
	}
	digest := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		t.Fatalf("unexpected digest output: %q", digest)
	}
	info, err := os.Stat(outFile)
	if err != nil || info.Size() == 0 {
		t.Fatalf("expected non-empty tarball at %s: err=%v", outFile, err)
	}
	retained, err := st.ListRetainedDigests("lazarette")
	if err != nil || len(retained) != 1 || retained[0] != digest {
		t.Fatalf("expected signed bootstrap digest in store retention list, got %v (err=%v)", retained, err)
	}
}
