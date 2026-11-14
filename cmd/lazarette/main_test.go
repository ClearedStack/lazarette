package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunValidatesFlags(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(context.Background(), nil, &stderr, nil); code != 2 {
		t.Errorf("missing -data: exit %d, want 2", code)
	}
	stderr.Reset()
	if code := run(context.Background(), []string{"-data", t.TempDir(), "extra"}, &stderr, nil); code != 2 {
		t.Errorf("extra arg: exit %d, want 2", code)
	}
	stderr.Reset()
	if code := run(context.Background(), []string{"-version"}, &stderr, nil); code != 0 || !strings.Contains(stderr.String(), version) {
		t.Errorf("-version: exit %d, output %q", code, stderr.String())
	}
}

func TestRunServesAndRunsScheduledGarbageCollection(t *testing.T) {
	dir := t.TempDir()
	// Pre-populate an unreferenced blob so the scheduled GC pass removes it.
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o750); err != nil {
		t.Fatal(err)
	}
	staleBlob := filepath.Join(dir, "blobs", "sha256", strings.Repeat("a", 64))
	if err := os.WriteFile(staleBlob, []byte("unreferenced"), 0o440); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	done := make(chan int, 1)
	var stderr bytes.Buffer
	go func() {
		done <- run(ctx, []string{
			"-addr", "127.0.0.1:0",
			"-data", dir,
			"-gc-interval", "25ms",
			"-gc-grace", "0s",
		}, &stderr, ready)
	}()

	addr := <-ready
	resp, err := http.Get("http://" + addr + "/v2/")
	if err != nil {
		t.Fatalf("GET /v2/: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v2/ status = %d, want 200", resp.StatusCode)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(staleBlob); os.IsNotExist(err) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(staleBlob); !os.IsNotExist(err) {
		t.Errorf("scheduled GC did not remove unreferenced blob")
	}

	cancel()
	if code := <-done; code != 0 {
		t.Errorf("exit code = %d, want 0; stderr:\n%s", code, stderr.String())
	}
}
