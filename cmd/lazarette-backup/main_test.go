package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClearedStack/lazarette/internal/bootstrap"
	"github.com/ClearedStack/lazarette/internal/registry"
	"github.com/ClearedStack/lazarette/internal/store"
)

func TestBackupSnapshotUploadDrillAndRestoreCLI(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), nil, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("no subcommand: exit %d, want 2", code)
	}
	if code := run(context.Background(), []string{"unknown"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("unknown subcommand: exit %d, want 2", code)
	}

	root := t.TempDir()
	liveDir := filepath.Join(root, "live-registry")
	st, err := store.Open(liveDir)
	if err != nil {
		t.Fatal(err)
	}
	regSrv := httptest.NewServer(&registry.Handler{Store: st})
	defer regSrv.Close()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	built, err := bootstrap.Build(bootstrap.Spec{
		Binary:     []byte("\x7fELF-static-assay-arm64"),
		ImageRef:   "lazarette.clearedstack.svc.cluster.local:5000/apps/assay",
		Entrypoint: "/assay",
		Arch:       "arm64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.PushAndSign(context.Background(), bootstrap.PushOptions{
		BaseURL:    regSrv.URL,
		HTTP:       regSrv.Client(),
		DockerRef:  "lazarette.clearedstack.svc.cluster.local:5000/apps/assay",
		Result:     built,
		PrivateKey: priv,
	}); err != nil {
		t.Fatalf("PushAndSign: %v", err)
	}

	var uploadedBytes []byte
	var gotAuth, gotDigest string
	uploadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		gotDigest = r.Header.Get("X-Content-SHA256")
		var err error
		uploadedBytes, err = io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer uploadSrv.Close()

	tokenFile := filepath.Join(root, "upload-token")
	if err := os.WriteFile(tokenFile, []byte("ephemeral-test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archiveFile := filepath.Join(root, "snapshot.tar.gz")
	scratchDir := filepath.Join(root, "scratch")
	if err := os.MkdirAll(scratchDir, 0o750); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	code := run(context.Background(), []string{
		"snapshot",
		"-data", liveDir,
		"-out", archiveFile,
		"-scratch", scratchDir,
		"-upload-url", uploadSrv.URL + "/lazarette/latest.tar.gz",
		"-upload-token-file", tokenFile,
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("snapshot exit %d, stderr: %s", code, stderr.String())
	}
	var snapSum summaryOutput
	if err := json.Unmarshal(stdout.Bytes(), &snapSum); err != nil {
		t.Fatalf("decode snapshot summary: %v (%q)", err, stdout.String())
	}
	if snapSum.Op != "snapshot" || snapSum.Repos != 1 || snapSum.Manifests != 2 {
		t.Fatalf("unexpected snapshot summary: %+v", snapSum)
	}
	if gotAuth != "Bearer ephemeral-test-token" || gotDigest != snapSum.ArchiveSHA256 || len(uploadedBytes) == 0 {
		t.Fatalf("unexpected upload headers/body: auth=%q digest=%q bytes=%d", gotAuth, gotDigest, len(uploadedBytes))
	}

	// Drill from live directory and verify scratch directory is cleaned up afterward.
	stdout.Reset()
	stderr.Reset()
	code = run(context.Background(), []string{
		"drill",
		"-data", liveDir,
		"-scratch", scratchDir,
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("drill (-data) exit %d, stderr: %s", code, stderr.String())
	}
	scratchEntries, err := os.ReadDir(scratchDir)
	if err != nil || len(scratchEntries) != 0 {
		t.Fatalf("expected clean scratch directory after drill, got %d entries (err=%v)", len(scratchEntries), err)
	}

	// Drill from saved archive file.
	stdout.Reset()
	stderr.Reset()
	code = run(context.Background(), []string{
		"drill",
		"-in", archiveFile,
		"-scratch", scratchDir,
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("drill (-in) exit %d, stderr: %s", code, stderr.String())
	}

	// Drill rejects a corrupted archive.
	corruptFile := filepath.Join(root, "corrupt.tar.gz")
	if err := os.WriteFile(corruptFile, []byte("not-a-gzip-archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"drill", "-in", corruptFile, "-scratch", scratchDir}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Fatalf("drill corrupt archive: exit %d, want 1", code)
	}

	// Restore into an empty directory and serve from the restored store.
	restoredDir := filepath.Join(root, "restored-registry")
	stdout.Reset()
	stderr.Reset()
	code = run(context.Background(), []string{
		"restore",
		"-in", archiveFile,
		"-data", restoredDir,
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("restore exit %d, stderr: %s", code, stderr.String())
	}
	restoredStore, err := store.Open(restoredDir)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := restoredStore.ListRetainedDigests("apps/assay")
	if err != nil || len(retained) != 1 || retained[0] != built.ManifestDigest {
		t.Fatalf("restored retained digests = %v (err=%v), want [%s]", retained, err, built.ManifestDigest)
	}
}
