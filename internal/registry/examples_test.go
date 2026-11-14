package registry

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/lazarette/internal/store"
)

// The saved examples live in testdata/cases. Each file is one rule: a list
// of requests sent in order to a fresh, empty registry, and for each request
// the exact status, the exact set of response headers and the exact body
// expected. Request and response bodies too long to read inline are files in
// testdata/payloads.
//
// To re-record the expected responses after an intended change:
//
//	go test ./internal/registry -run TestSavedExamples -update
//
// and review the diff by hand.
var update = flag.Bool("update", false, "rewrite the expected responses in testdata/cases")

type exampleCase struct {
	Rule  string        `json:"rule"`
	Steps []exampleStep `json:"steps"`
}

type exampleStep struct {
	Note           string          `json:"note,omitempty"`
	CollectGarbage bool            `json:"collectGarbage,omitempty"`
	Request        exampleRequest  `json:"request"`
	Response       exampleResponse `json:"response"`
}

type exampleRequest struct {
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	Headers  map[string]string `json:"headers,omitempty"`
	Body     string            `json:"body,omitempty"`
	BodyFile string            `json:"bodyFile,omitempty"`
}

type exampleResponse struct {
	Status   int               `json:"status"`
	Headers  map[string]string `json:"headers"`
	Body     string            `json:"body,omitempty"`
	BodyFile string            `json:"bodyFile,omitempty"`
}

func payload(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "payloads", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// sequentialIDs gives upload sessions predictable identifiers, so that the
// saved examples are the same on every run.
func sequentialIDs() func() (string, error) {
	n := 0
	return func() (string, error) {
		n++
		return fmt.Sprintf("%032x", n), nil
	}
}

func newTestHandler(t *testing.T, dir string) *Handler {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &Handler{Store: st, NewUploadID: sequentialIDs()}
}

func TestSavedExamples(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "cases", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no saved examples found")
	}
	sort.Strings(files)
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".json")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var c exampleCase
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&c); err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			h := newTestHandler(t, t.TempDir())
			for i := range c.Steps {
				if c.Steps[i].CollectGarbage {
					if _, err := h.Store.CollectGarbage(time.Now(), 0); err != nil {
						t.Fatalf("step %d CollectGarbage: %v", i+1, err)
					}
				}
				runStep(t, h, i, &c.Steps[i])
			}
			if *update {
				out, err := json.MarshalIndent(c, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, append(out, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func runStep(t *testing.T, h *Handler, i int, step *exampleStep) {
	t.Helper()
	body := []byte(step.Request.Body)
	if step.Request.BodyFile != "" {
		body = payload(t, step.Request.BodyFile)
	}
	req := httptest.NewRequest(step.Request.Method, step.Request.Path, bytes.NewReader(body))
	for k, v := range step.Request.Headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()

	gotHeaders := map[string]string{}
	for k, v := range res.Header {
		gotHeaders[k] = strings.Join(v, ", ")
	}
	gotBody := rec.Body.Bytes()

	where := fmt.Sprintf("step %d (%s %s)", i+1, step.Request.Method, step.Request.Path)
	want := &step.Response
	if *update {
		want.Status = res.StatusCode
		want.Headers = gotHeaders
		if want.BodyFile == "" {
			want.Body = string(gotBody)
		}
	}
	if res.StatusCode != want.Status {
		t.Errorf("%s: status %d, want %d", where, res.StatusCode, want.Status)
	}
	if !reflect.DeepEqual(gotHeaders, want.Headers) {
		t.Errorf("%s: headers\n got  %v\n want %v", where, gotHeaders, want.Headers)
	}
	wantBody := []byte(want.Body)
	if want.BodyFile != "" {
		wantBody = payload(t, want.BodyFile)
	}
	if !bytes.Equal(gotBody, wantBody) {
		t.Errorf("%s: body\n got  %q\n want %q", where, gotBody, wantBody)
	}
	if res.StatusCode >= 400 && step.Request.Method != http.MethodHead {
		checkPublicError(t, where, gotBody)
	}
}

// checkPublicError holds every refusal to the rule on public error codes:
// the only codes that may appear in detail.lazarette are the three allowed.
func checkPublicError(t *testing.T, where string, body []byte) {
	t.Helper()
	var parsed errorBody
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Errors) != 1 {
		t.Errorf("%s: refusal body is not one error in the OCI error format: %q", where, body)
		return
	}
	if d := parsed.Errors[0].Detail; d != nil {
		switch d.Lazarette {
		case codeTimeout, codeParse, codeNeedsSubtool:
		default:
			t.Errorf("%s: public code %q is not one of the three allowed", where, d.Lazarette)
		}
	}
}
