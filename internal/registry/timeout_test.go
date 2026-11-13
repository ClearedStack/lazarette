package registry

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clearedstack/lazarette/internal/store"
)

const stalledBlob = "sha256:5e930cbbf825c94c16e1bc9392c3cb32c38e07925d71cc7dea08d6e8745b5cb7"

// stall sends request headers that promise 1000 bytes, sends only the given
// part of the body, then waits for the answer without sending more.
func stall(t *testing.T, addr, method, path, part string) (*http.Response, string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(conn, method+" "+path+" HTTP/1.1\r\nHost: registry.test\r\nContent-Type: application/octet-stream\r\nContent-Length: 1000\r\n\r\n"+part)
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to a stalled request: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res, string(body)
}

func TestStalledBodyIsRefusedWithTimeout(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{Store: st, IdleTimeout: 150 * time.Millisecond, NewUploadID: sequentialIDs()}
	srv := httptest.NewServer(h)
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	want := payload(t, "timeout-blob.json")
	wantManifest := payload(t, "timeout-manifest.json")

	// A whole-blob push that stalls: nothing is stored.
	res, body := stall(t, addr, "POST", "/v2/apps/example/blobs/uploads/?digest="+stalledBlob, "partial bytes")
	if res.StatusCode != http.StatusRequestTimeout || body != string(want) {
		t.Errorf("stalled POST: status %d body %q, want 408 %q", res.StatusCode, body, want)
	}
	if blobs, _ := os.ReadDir(filepath.Join(dir, "blobs", "sha256")); len(blobs) != 0 {
		t.Errorf("a stalled push left %d blobs", len(blobs))
	}
	if sessions, _ := os.ReadDir(filepath.Join(dir, "uploads")); len(sessions) != 0 {
		t.Errorf("a stalled push left %d upload sessions", len(sessions))
	}

	// A chunk that stalls is dropped and the session keeps its earlier size.
	id, _ := h.NewUploadID()
	if err := st.NewUpload(id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendUpload(id, 0, -1, strings.NewReader("kept")); err != nil {
		t.Fatal(err)
	}
	res, body = stall(t, addr, "PATCH", "/v2/apps/example/blobs/uploads/"+id, "partial bytes")
	if res.StatusCode != http.StatusRequestTimeout || body != string(want) {
		t.Errorf("stalled PATCH: status %d body %q, want 408 %q", res.StatusCode, body, want)
	}
	if size, err := st.UploadSize(id); err != nil || size != 4 {
		t.Errorf("after a stalled chunk the session holds %d bytes (%v), want 4", size, err)
	}

	// A manifest that stalls is not stored.
	res, body = stall(t, addr, "PUT", "/v2/apps/example/manifests/"+stalledBlob, `{"schemaVersion":2,`)
	if res.StatusCode != http.StatusRequestTimeout || body != string(wantManifest) {
		t.Errorf("stalled manifest PUT: status %d body %q, want 408 %q", res.StatusCode, body, wantManifest)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifests", "apps")); !os.IsNotExist(err) {
		t.Errorf("a stalled manifest push left something on disk")
	}
}

// A body that keeps moving is not cut off, however slow each step is
// compared with the idle timeout taken as a whole-request limit.
func TestSlowButMovingBodyIsAccepted(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&Handler{Store: st, IdleTimeout: 400 * time.Millisecond})
	defer srv.Close()

	content := payload(t, "layer.bin")
	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < len(content); i += 23 { // six writes, 100 ms apart
			end := min(i+23, len(content))
			pw.Write(content[i:end])
			time.Sleep(100 * time.Millisecond)
		}
		pw.Close()
	}()
	req, _ := http.NewRequest("POST", srv.URL+"/v2/apps/example/blobs/uploads/?digest="+stalledBlob, pr)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Errorf("slow upload: status %d, want 201", res.StatusCode)
	}
}
