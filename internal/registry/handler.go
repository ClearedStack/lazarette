// Package registry serves the part of the OCI distribution API that pushes
// and pulls one image by sha256 digest.
package registry

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/clearedstack/lazarette/internal/store"
)

const (
	// MediaTypeImageManifest is the only manifest media type handled.
	MediaTypeImageManifest = "application/vnd.oci.image.manifest.v1+json"

	maxManifestBytes = 4 << 20

	// DefaultIdleTimeout is how long a request body may make no progress
	// before the request is refused with E_TIMEOUT.
	DefaultIdleTimeout = 30 * time.Second
)

// Handler is the registry HTTP handler.
type Handler struct {
	// Store is where blobs and manifests are kept. Required.
	Store *store.Store
	// IdleTimeout bounds how long a request body may stall. Zero means
	// DefaultIdleTimeout.
	IdleTimeout time.Duration
	// NewUploadID returns a fresh upload session identifier of 32 lower-case
	// hex characters. Nil means random identifiers.
	NewUploadID func() (string, error)
	// Log receives one line per request and the cause of storage failures.
	// Nil means no logging. Log lines are for the operator and are never
	// sent to a client.
	Log *log.Logger
}

func randomUploadID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// statusWriter remembers what was answered, for the request log.
type statusWriter struct {
	http.ResponseWriter
	status int
	public string
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (h *Handler) refuse(w http.ResponseWriter, r *http.Request, e refusal) {
	if sw, ok := w.(*statusWriter); ok {
		sw.public = e.public
	}
	if e.public == codeTimeout {
		// The connection's read side is past its deadline and cannot be reused.
		w.Header().Set("Connection", "close")
	}
	e.write(w, r)
}

// storageFailure answers with the fixed storage refusal and logs the cause
// for the operator. The cause never reaches the client.
func (h *Handler) storageFailure(w http.ResponseWriter, r *http.Request, err error) {
	if h.Log != nil {
		h.Log.Printf("storage failure: %v", err)
	}
	h.refuse(w, r, errStorage)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt := parseRoute(r.URL.Path)
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	h.serve(sw, r, rt)
	if h.Log != nil {
		public := sw.public
		if public == "" {
			public = "-"
		}
		h.Log.Printf("%s %s %d %s", r.Method, rt.kind, sw.status, public)
	}
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, rt route) {
	switch rt.kind {
	case routeBase:
		if !h.allow(w, r, http.MethodGet, http.MethodHead) {
			return
		}
		h.serveBase(w, r)
	case routeBlob:
		if !h.allow(w, r, http.MethodGet, http.MethodHead) {
			return
		}
		h.serveBlob(w, r, rt)
	case routeUploads:
		if !h.allow(w, r, http.MethodPost) {
			return
		}
		h.startUpload(w, r, rt)
	case routeUpload:
		if !h.allow(w, r, http.MethodGet, http.MethodPatch, http.MethodPut) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			h.uploadStatus(w, r, rt)
		case http.MethodPatch:
			h.patchUpload(w, r, rt)
		case http.MethodPut:
			h.finishUpload(w, r, rt)
		}
	case routeManifest:
		if !h.allow(w, r, http.MethodGet, http.MethodHead, http.MethodPut) {
			return
		}
		if r.Method == http.MethodPut {
			h.putManifest(w, r, rt)
		} else {
			h.getManifest(w, r, rt)
		}
	case routeBadName:
		h.refuse(w, r, errNameInvalid)
	default:
		h.refuse(w, r, errRouteUnfamiliar)
	}
}

// allow refuses any method not in the list.
func (h *Handler) allow(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	allowed := ""
	for i, m := range methods {
		if r.Method == m {
			return true
		}
		if i > 0 {
			allowed += ", "
		}
		allowed += m
	}
	w.Header().Set("Allow", allowed)
	h.refuse(w, r, errMethodUnfamiliar)
	return false
}

func (h *Handler) serveBase(w http.ResponseWriter, r *http.Request) {
	const body = "{}\n"
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		io.WriteString(w, body)
	}
}

// parseDigest turns a digest string into a Digest or the matching refusal.
func parseDigest(s string) (store.Digest, *refusal) {
	d, err := store.ParseDigest(s)
	switch {
	case err == nil:
		return d, nil
	case errors.Is(err, store.ErrDigestUnfamiliar):
		return d, &errDigestUnfamiliar
	default:
		return d, &errDigestMalformed
	}
}

func (h *Handler) serveBlob(w http.ResponseWriter, r *http.Request, rt route) {
	d, bad := parseDigest(rt.ref)
	if bad != nil {
		h.refuse(w, r, *bad)
		return
	}
	f, _, err := h.Store.OpenBlob(d)
	if errors.Is(err, store.ErrNotFound) {
		h.refuse(w, r, errBlobUnknown)
		return
	}
	if err != nil {
		h.storageFailure(w, r, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", d.String())
	w.Header().Set("ETag", `"`+d.String()+`"`)
	// ServeContent answers HEAD, whole-blob GET and byte-range GET. Its own
	// text for a range it cannot serve is replaced by a fixed refusal.
	rw := &rangeWriter{ResponseWriter: w}
	http.ServeContent(rw, r, "", time.Time{}, f)
	if rw.unsatisfiable {
		w.Header().Del("X-Content-Type-Options")
		h.refuse(w, r, errBlobRange)
	}
}

// rangeWriter holds back a 416 answer so the handler can write its own.
type rangeWriter struct {
	http.ResponseWriter
	unsatisfiable bool
}

func (w *rangeWriter) WriteHeader(status int) {
	if status == http.StatusRequestedRangeNotSatisfiable {
		w.unsatisfiable = true
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *rangeWriter) Write(p []byte) (int, error) {
	if w.unsatisfiable {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}

func uploadLocation(repo, id string) string {
	return "/v2/" + repo + "/blobs/uploads/" + id
}

func blobLocation(repo string, d store.Digest) string {
	return "/v2/" + repo + "/blobs/" + d.String()
}

// uploadProgress writes the headers that describe an open upload session.
func uploadProgress(w http.ResponseWriter, repo, id string, size int64, status int) {
	last := size - 1
	if last < 0 {
		last = 0
	}
	w.Header().Set("Location", uploadLocation(repo, id))
	w.Header().Set("Range", "0-"+strconv.FormatInt(last, 10))
	w.Header().Set("Docker-Upload-UUID", id)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
}

func blobCreated(w http.ResponseWriter, repo string, d store.Digest) {
	w.Header().Set("Location", blobLocation(repo, d))
	w.Header().Set("Docker-Content-Digest", d.String())
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

// startUpload opens an upload session. With a digest query parameter the
// request body is the whole blob and is committed at once. A cross-repository
// mount request (mount= and from=) is answered with an ordinary upload
// session, which the distribution specification allows.
func (h *Handler) startUpload(w http.ResponseWriter, r *http.Request, rt route) {
	query := r.URL.Query()
	var d store.Digest
	monolithic := query.Has("digest")
	if monolithic {
		var bad *refusal
		if d, bad = parseDigest(query.Get("digest")); bad != nil {
			h.refuse(w, r, *bad)
			return
		}
	}
	newID := h.NewUploadID
	if newID == nil {
		newID = randomUploadID
	}
	id, err := newID()
	if err == nil {
		err = h.Store.NewUpload(id)
	}
	if err != nil {
		h.storageFailure(w, r, err)
		return
	}
	if !monolithic {
		uploadProgress(w, rt.repo, id, 0, http.StatusAccepted)
		return
	}
	if _, err := h.Store.AppendUpload(id, 0, r.ContentLength, h.body(w, r)); err != nil {
		// The client never learned of this session, so it ends here.
		if derr := h.Store.DropUpload(id); derr != nil && h.Log != nil {
			h.Log.Printf("storage failure: %v", derr)
		}
		h.appendFailure(w, r, err)
		return
	}
	h.commit(w, r, rt.repo, id, d)
}

func (h *Handler) uploadStatus(w http.ResponseWriter, r *http.Request, rt route) {
	size, err := h.Store.UploadSize(rt.ref)
	if errors.Is(err, store.ErrNotFound) {
		h.refuse(w, r, errUploadUnknown)
		return
	}
	if err != nil {
		h.storageFailure(w, r, err)
		return
	}
	uploadProgress(w, rt.repo, rt.ref, size, http.StatusNoContent)
}

var contentRange = regexp.MustCompile(`^([0-9]{1,18})-([0-9]{1,18})$`)

// chunkBounds reads Content-Range and the declared body length. offset is -1 when no
// range was given and length is -1 when no length can be worked out.
func chunkBounds(r *http.Request) (offset, length int64, bad *refusal) {
	length = r.ContentLength // -1 when the client did not state one
	v := r.Header.Get("Content-Range")
	if v == "" {
		return -1, length, nil
	}
	m := contentRange.FindStringSubmatch(v)
	if m == nil {
		return 0, 0, &errRangeMalformed
	}
	first, _ := strconv.ParseInt(m[1], 10, 64)
	last, _ := strconv.ParseInt(m[2], 10, 64)
	if last < first {
		return 0, 0, &errRangeMalformed
	}
	span := last - first + 1
	if length >= 0 && length != span {
		return 0, 0, &errChunkLength
	}
	return first, span, nil
}

func (h *Handler) patchUpload(w http.ResponseWriter, r *http.Request, rt route) {
	offset, length, bad := chunkBounds(r)
	if bad != nil {
		h.refuse(w, r, *bad)
		return
	}
	size, err := h.Store.AppendUpload(rt.ref, offset, length, h.body(w, r))
	if errors.Is(err, store.ErrOffset) {
		last := size - 1
		if last < 0 {
			last = 0
		}
		w.Header().Set("Location", uploadLocation(rt.repo, rt.ref))
		w.Header().Set("Range", "0-"+strconv.FormatInt(last, 10))
		h.refuse(w, r, errRangeOrder)
		return
	}
	if err != nil {
		h.appendFailure(w, r, err)
		return
	}
	uploadProgress(w, rt.repo, rt.ref, size, http.StatusAccepted)
}

func (h *Handler) finishUpload(w http.ResponseWriter, r *http.Request, rt route) {
	query := r.URL.Query()
	if !query.Has("digest") {
		h.refuse(w, r, errDigestMissing)
		return
	}
	d, bad := parseDigest(query.Get("digest"))
	if bad != nil {
		h.refuse(w, r, *bad)
		return
	}
	offset, length, bad := chunkBounds(r)
	if bad != nil {
		h.refuse(w, r, *bad)
		return
	}
	// A final chunk may come with the closing PUT.
	if _, err := h.Store.AppendUpload(rt.ref, offset, length, h.body(w, r)); err != nil {
		if errors.Is(err, store.ErrOffset) {
			h.refuse(w, r, errRangeOrder)
			return
		}
		h.appendFailure(w, r, err)
		return
	}
	h.commit(w, r, rt.repo, rt.ref, d)
}

func (h *Handler) commit(w http.ResponseWriter, r *http.Request, repo, id string, d store.Digest) {
	err := h.Store.CommitUpload(id, d)
	switch {
	case err == nil:
		blobCreated(w, repo, d)
	case errors.Is(err, store.ErrDigestMismatch):
		h.refuse(w, r, errBlobMismatch)
	case errors.Is(err, store.ErrNotFound):
		h.refuse(w, r, errUploadUnknown)
	default:
		h.storageFailure(w, r, err)
	}
}

func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (h *Handler) appendFailure(w http.ResponseWriter, r *http.Request, err error) {
	var body *store.BodyError
	switch {
	case errors.Is(err, store.ErrNotFound):
		h.refuse(w, r, errUploadUnknown)
	case errors.Is(err, store.ErrLength):
		h.refuse(w, r, errChunkLength)
	case errors.As(err, &body) && isTimeout(body.Err):
		h.refuse(w, r, errBodyTimeout)
	case errors.As(err, &body):
		h.refuse(w, r, errBodyBroken)
	default:
		h.storageFailure(w, r, err)
	}
}

// idleBody pushes the connection's read deadline forward before every read,
// so a body may take as long as it likes while it keeps moving and fails
// once it stalls for longer than the idle timeout.
type idleBody struct {
	body io.Reader
	rc   *http.ResponseController
	idle time.Duration
}

func (b *idleBody) Read(p []byte) (int, error) {
	// Not every ResponseWriter has deadlines; without them there is no
	// connection to stall, so the error is of no interest.
	_ = b.rc.SetReadDeadline(time.Now().Add(b.idle))
	return b.body.Read(p)
}

func (h *Handler) body(w http.ResponseWriter, r *http.Request) io.Reader {
	idle := h.IdleTimeout
	if idle <= 0 {
		idle = DefaultIdleTimeout
	}
	return &idleBody{body: r.Body, rc: http.NewResponseController(w), idle: idle}
}

// manifestDigest reads the manifest reference. Only sha256 digests are
// handled; a tag is well-formed and unfamiliar.
func manifestDigest(ref string) (store.Digest, *refusal) {
	d, err := store.ParseDigest(ref)
	switch {
	case err == nil:
		return d, nil
	case errors.Is(err, store.ErrDigestUnfamiliar):
		return d, &errDigestUnfamiliar
	case tagName.MatchString(ref):
		return d, &errTagReference
	default:
		return d, &errDigestMalformed
	}
}

// Tag grammar from the OCI distribution specification.
var tagName = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)

func (h *Handler) getManifest(w http.ResponseWriter, r *http.Request, rt route) {
	d, bad := manifestDigest(rt.ref)
	if bad != nil {
		h.refuse(w, r, *bad)
		return
	}
	data, err := h.Store.GetManifest(rt.repo, d)
	if errors.Is(err, store.ErrNotFound) {
		h.refuse(w, r, errManifestUnknown)
		return
	}
	if err != nil {
		h.storageFailure(w, r, err)
		return
	}
	w.Header().Set("Content-Type", MediaTypeImageManifest)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Docker-Content-Digest", d.String())
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(data)
	}
}

type descriptor struct {
	Digest string `json:"digest"`
	Size   *int64 `json:"size"`
}

type imageManifest struct {
	SchemaVersion int           `json:"schemaVersion"`
	MediaType     string        `json:"mediaType"`
	Config        *descriptor   `json:"config"`
	Layers        *[]descriptor `json:"layers"`
}

func (h *Handler) putManifest(w http.ResponseWriter, r *http.Request, rt route) {
	d, bad := manifestDigest(rt.ref)
	if bad != nil {
		h.refuse(w, r, *bad)
		return
	}
	data, err := io.ReadAll(io.LimitReader(h.body(w, r), maxManifestBytes+1))
	switch {
	case err != nil && isTimeout(err):
		h.refuse(w, r, errManifestTimeout)
		return
	case err != nil:
		h.refuse(w, r, errManifestBodyBroken)
		return
	case len(data) > maxManifestBytes:
		h.refuse(w, r, errManifestTooLarge)
		return
	}
	if bad := h.checkManifest(data, r.Header.Get("Content-Type")); bad != nil {
		h.refuse(w, r, *bad)
		return
	}
	err = h.Store.PutManifest(rt.repo, d, data)
	switch {
	case errors.Is(err, store.ErrDigestMismatch):
		h.refuse(w, r, errManifestMismatch)
		return
	case err != nil:
		h.storageFailure(w, r, err)
		return
	}
	w.Header().Set("Location", "/v2/"+rt.repo+"/manifests/"+d.String())
	w.Header().Set("Docker-Content-Digest", d.String())
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

// checkManifest decides whether the bytes are an OCI image manifest whose
// config and layers are all already stored at the sizes it states.
func (h *Handler) checkManifest(data []byte, contentType string) *refusal {
	var m imageManifest
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&m); err != nil {
		return &errManifestBroken
	}
	if _, err := dec.Token(); err != io.EOF {
		return &errManifestBroken // something follows the JSON value
	}
	mediaType := m.MediaType
	switch {
	case mediaType == "":
		mediaType = contentType
	case contentType != "" && contentType != mediaType:
		return &errManifestTypeConflict
	}
	if mediaType == "" {
		return &errManifestBroken
	}
	if mediaType != MediaTypeImageManifest {
		return &errManifestUnfamiliar
	}
	if m.SchemaVersion != 2 || m.Config == nil || m.Layers == nil {
		return &errManifestBroken
	}
	for _, desc := range append([]descriptor{*m.Config}, *m.Layers...) {
		if desc.Size == nil || *desc.Size < 0 {
			return &errManifestBroken
		}
		d, bad := parseDigest(desc.Digest)
		if bad != nil {
			return bad
		}
		size, err := h.Store.BlobSize(d)
		if errors.Is(err, store.ErrNotFound) {
			return &errManifestBlobUnknown
		}
		if err != nil {
			if h.Log != nil {
				h.Log.Printf("storage failure: %v", err)
			}
			return &errStorage
		}
		if size != *desc.Size {
			return &errManifestBlobSize
		}
	}
	return nil
}
