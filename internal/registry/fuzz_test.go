package registry

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClearedStack/lazarette/internal/store"
)

func seedFuzzBlob(tb testing.TB, st *store.Store, id string, data []byte) store.Digest {
	tb.Helper()
	d := store.DigestOfBytes(data)
	if err := st.NewUpload(id); err != nil {
		tb.Fatal(err)
	}
	if _, err := st.AppendUpload(id, 0, int64(len(data)), bytes.NewReader(data)); err != nil {
		tb.Fatal(err)
	}
	if err := st.CommitUpload(id, d); err != nil {
		tb.Fatal(err)
	}
	return d
}

func FuzzParseRouteAndRange(f *testing.F) {
	st, err := store.Open(f.TempDir())
	if err != nil {
		f.Fatal(err)
	}
	blobDigest := seedFuzzBlob(f, st, "00000000000000000000000000000001", []byte("0123456789abcdef"))
	h := &Handler{Store: st}

	seeds := []struct {
		path         string
		contentRange string
		byteRange    string
		contentLen   int64
	}{
		{"/v2/", "", "", -1},
		{"/v2/apps/assay/blobs/" + blobDigest.String(), "0-15", "bytes=0-3", 16},
		{"/v2/apps/assay/blobs/uploads/", "10-5", "bytes=100-200", 5},
		{"/v2/apps/assay/manifests/sha256-" + blobDigest.Hex() + ".sig", "0-0", "bytes=-4", 1},
		{"/v2/apps/assay/referrers/" + blobDigest.String(), "999999999999999999-999999999999999999", "invalid", 1},
		{"/v2/apps/assay/tags/list", "", "", 0},
		{"/v2/apps/assay/digests/list", "", "", 0},
		{"/v2/INVALID_CAPS/manifests/latest", "abc-def", "bytes=5-2", -1},
	}
	for _, s := range seeds {
		f.Add(s.path, s.contentRange, s.byteRange, s.contentLen)
	}

	f.Fuzz(func(t *testing.T, reqPath, contentRangeHdr, byteRangeHdr string, contentLen int64) {
		rt := parseRoute(reqPath)
		switch rt.kind {
		case routeUnknown, routeBase, routeBadName:
		default:
			if !validRepo(rt.repo) {
				t.Fatalf("route %s accepted invalid repo %q for path %q", rt.kind, rt.repo, reqPath)
			}
		}

		d, sigTag, bad := parseManifestRef(rt.ref)
		if bad == nil && sigTag == "" && d.IsZero() {
			t.Fatalf("parseManifestRef(%q) returned zero digest without refusal", rt.ref)
		}

		chunkReq := &http.Request{
			Method:        http.MethodPatch,
			URL:           &url.URL{Path: "/v2/apps/fuzz/blobs/uploads/00000000000000000000000000000001"},
			Header:        http.Header{},
			ContentLength: contentLen,
			Body:          io.NopCloser(strings.NewReader("")),
		}
		if contentRangeHdr != "" {
			chunkReq.Header.Set("Content-Range", contentRangeHdr)
		}
		offset, length, rangeBad := chunkBounds(chunkReq)
		if rangeBad == nil && offset >= 0 && length < 1 {
			t.Fatalf("chunkBounds(%q, len=%d) returned non-positive span %d", contentRangeHdr, contentLen, length)
		}

		blobReq := &http.Request{
			Method: http.MethodGet,
			URL:    &url.URL{Path: "/v2/apps/fuzz/blobs/" + blobDigest.String()},
			Header: http.Header{},
			Body:   http.NoBody,
		}
		if byteRangeHdr != "" {
			blobReq.Header.Set("Range", byteRangeHdr)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, blobReq)
		if rec.Code >= 400 {
			checkPublicError(t, "FuzzParseRouteAndRange Range GET", rec.Body.Bytes())
		}
	})
}

func FuzzManifestValidation(f *testing.F) {
	st, err := store.Open(f.TempDir())
	if err != nil {
		f.Fatal(err)
	}

	cfgBytes, err := os.ReadFile(filepath.Join("testdata", "payloads", "config.json"))
	if err != nil {
		f.Fatal(err)
	}
	layerBytes, err := os.ReadFile(filepath.Join("testdata", "payloads", "layer.bin"))
	if err != nil {
		f.Fatal(err)
	}
	sigBytes, err := os.ReadFile(filepath.Join("testdata", "payloads", "sig.bin"))
	if err != nil {
		f.Fatal(err)
	}
	seedFuzzBlob(f, st, "00000000000000000000000000000011", cfgBytes)
	seedFuzzBlob(f, st, "00000000000000000000000000000012", layerBytes)
	seedFuzzBlob(f, st, "00000000000000000000000000000013", sigBytes)
	seedFuzzBlob(f, st, "00000000000000000000000000000014", []byte("{}"))

	baseManifest, err := os.ReadFile(filepath.Join("testdata", "payloads", "manifest.json"))
	if err != nil {
		f.Fatal(err)
	}
	baseDigest := store.DigestOfBytes(baseManifest)
	if err := st.PutManifest("apps/fuzz", baseDigest, baseManifest, store.ManifestOptions{}); err != nil {
		f.Fatal(err)
	}

	h := &Handler{Store: st}

	matches, err := filepath.Glob(filepath.Join("testdata", "payloads", "manifest*.json"))
	if err != nil {
		f.Fatal(err)
	}
	for _, m := range matches {
		raw, err := os.ReadFile(m)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(raw, MediaTypeImageManifest, true, "")
	}
	f.Add(baseManifest, MediaTypeImageManifest, false, "sha256-"+baseDigest.Hex()+".sig")
	f.Add([]byte("{}"), MediaTypeImageIndex, true, "")
	f.Add([]byte("not-json"), "text/plain", false, "latest")

	f.Fuzz(func(t *testing.T, body []byte, contentType string, matchBodyDigest bool, customRef string) {
		if len(body) > 64*1024 {
			return
		}
		ref := customRef
		if matchBodyDigest || ref == "" || strings.ContainsAny(ref, "/?#") {
			ref = store.DigestOfBytes(body).String()
		}
		req := &http.Request{
			Method:        http.MethodPut,
			URL:           &url.URL{Path: "/v2/apps/fuzz/manifests/" + ref},
			Header:        http.Header{},
			ContentLength: int64(len(body)),
			Body:          io.NopCloser(bytes.NewReader(body)),
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code >= 400 {
			checkPublicError(t, "FuzzManifestValidation PUT", rec.Body.Bytes())
		}
	})
}
