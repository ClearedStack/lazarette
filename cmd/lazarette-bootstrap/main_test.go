package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootstrapCLI(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 {
		t.Fatalf("missing flags: exit %d, want 2", code)
	}

	dir := t.TempDir()
	binFile := filepath.Join(dir, "lazarette")
	outFile := filepath.Join(dir, "lazarette.tar")
	if err := os.WriteFile(binFile, []byte("\x7fELF-static-arm64"), 0o755); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	code := run([]string{"-binary", binFile, "-out", outFile}, &stdout, &stderr)
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
}
