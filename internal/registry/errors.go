package registry

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// The three public error codes. A refusal carries exactly one of them, or
// none when it is an ordinary "that is not here" answer.
const (
	codeTimeout      = "E_TIMEOUT"       // something outside took too long
	codeParse        = "E_PARSE"         // the data was broken
	codeNeedsSubtool = "E_NEEDS_SUBTOOL" // the data was valid and unfamiliar
)

// refusal is one fixed answer. Every message is a constant written here:
// nothing from the request, the operating system or the network is ever
// copied into an error, so an error cannot name a machine, an address or a
// credential.
type refusal struct {
	status  int
	code    string // code from the OCI distribution specification
	public  string // one of the three public codes, or ""
	message string
}

var (
	errNameInvalid = refusal{http.StatusBadRequest, "NAME_INVALID", codeParse,
		"repository name is not a valid OCI repository name"}
	errRepoUnknown = refusal{http.StatusNotFound, "NAME_UNKNOWN", "",
		"repository is not known"}
	errDigestMalformed = refusal{http.StatusBadRequest, "DIGEST_INVALID", codeParse,
		"digest is malformed; expected sha256 followed by 64 lower-case hex characters"}
	errDigestUnfamiliar = refusal{http.StatusBadRequest, "UNSUPPORTED", codeNeedsSubtool,
		"digest uses an algorithm other than sha256, which is not handled"}
	errDigestMissing = refusal{http.StatusBadRequest, "DIGEST_INVALID", codeParse,
		"the digest query parameter is required to finish an upload"}
	errBlobMismatch = refusal{http.StatusBadRequest, "DIGEST_INVALID", codeParse,
		"uploaded content does not match the digest given; nothing was stored"}
	errBlobUnknown = refusal{http.StatusNotFound, "BLOB_UNKNOWN", "",
		"blob is not in this registry"}
	errBlobRange = refusal{http.StatusRequestedRangeNotSatisfiable, "SIZE_INVALID", codeParse,
		"Range header is malformed or asks for bytes past the end of the blob"}
	errUploadUnknown = refusal{http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "",
		"upload session is not known"}
	errRangeMalformed = refusal{http.StatusBadRequest, "BLOB_UPLOAD_INVALID", codeParse,
		"Content-Range header is malformed; expected first byte, hyphen, last byte"}
	errRangeOrder = refusal{http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID", codeParse,
		"chunk does not start where the upload currently ends"}
	errChunkLength = refusal{http.StatusBadRequest, "BLOB_UPLOAD_INVALID", codeParse,
		"chunk is not the length its headers declare; the chunk was discarded"}
	errBodyBroken = refusal{http.StatusBadRequest, "BLOB_UPLOAD_INVALID", codeParse,
		"request body ended early or could not be read; nothing was stored"}
	errBodyTimeout = refusal{http.StatusRequestTimeout, "BLOB_UPLOAD_INVALID", codeTimeout,
		"request body stalled for too long; nothing from this request was stored"}

	errManifestUnknown = refusal{http.StatusNotFound, "MANIFEST_UNKNOWN", "",
		"manifest is not in this repository"}
	errTagReference = refusal{http.StatusBadRequest, "UNSUPPORTED", codeNeedsSubtool,
		"manifests are addressed by sha256 digest only; tag references are not handled"}
	errManifestMismatch = refusal{http.StatusBadRequest, "DIGEST_INVALID", codeParse,
		"manifest content does not match the digest in the request path; nothing was stored"}
	errManifestTooLarge = refusal{http.StatusRequestEntityTooLarge, "MANIFEST_INVALID", codeParse,
		"manifest is larger than 4 MiB"}
	errManifestBroken = refusal{http.StatusBadRequest, "MANIFEST_INVALID", codeParse,
		"manifest is not a well-formed OCI image manifest"}
	errManifestTypeConflict = refusal{http.StatusBadRequest, "MANIFEST_INVALID", codeParse,
		"manifest mediaType field and Content-Type header disagree"}
	errManifestUnfamiliar = refusal{http.StatusUnsupportedMediaType, "UNSUPPORTED", codeNeedsSubtool,
		"manifest media type is not an OCI image manifest or OCI image index, the only types handled"}
	errManifestBlobUnknown = refusal{http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", codeParse,
		"manifest refers to a blob that has not been pushed; the manifest was not stored"}
	errManifestBlobSize = refusal{http.StatusBadRequest, "MANIFEST_INVALID", codeParse,
		"manifest gives a size for a blob that differs from the stored blob; the manifest was not stored"}
	errIndexManifestUnknown = refusal{http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", codeParse,
		"image index refers to a manifest that has not been pushed; the index was not stored"}
	errIndexManifestSize = refusal{http.StatusBadRequest, "MANIFEST_INVALID", codeParse,
		"image index gives a size or media type for a manifest that differs from the stored manifest; the index was not stored"}
	errSubjectUnknown = refusal{http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", codeParse,
		"manifest subject refers to a manifest that has not been pushed; the manifest was not stored"}
	errSubjectMismatch = refusal{http.StatusBadRequest, "MANIFEST_INVALID", codeParse,
		"manifest subject size or media type differs from the stored manifest; the manifest was not stored"}
	errSignatureTargetUnknown = refusal{http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", codeParse,
		"signature tag names an image digest that has not been pushed; the signature was not stored"}
	errSignatureTargetInvalid = refusal{http.StatusBadRequest, "MANIFEST_INVALID", codeParse,
		"signature tag must target an image manifest or image index, not another signature or referrer"}
	errSignatureIndexRefused = refusal{http.StatusBadRequest, "MANIFEST_INVALID", codeParse,
		"signature tag requires an OCI image manifest, not an image index"}
	errSignatureSubjectMismatch = refusal{http.StatusBadRequest, "MANIFEST_INVALID", codeParse,
		"signature manifest subject does not match the digest in the signature tag"}
	errManifestTimeout = refusal{http.StatusRequestTimeout, "MANIFEST_INVALID", codeTimeout,
		"request body stalled for too long; the manifest was not stored"}
	errManifestBodyBroken = refusal{http.StatusBadRequest, "MANIFEST_INVALID", codeParse,
		"request body ended early or could not be read; the manifest was not stored"}

	errReferrersQuery = refusal{http.StatusBadRequest, "UNSUPPORTED", codeParse,
		"referrers query accepts at most one non-empty artifactType parameter"}
	errRouteUnfamiliar = refusal{http.StatusNotFound, "UNSUPPORTED", codeNeedsSubtool,
		"this endpoint is not handled"}
	errMethodUnfamiliar = refusal{http.StatusMethodNotAllowed, "UNSUPPORTED", codeNeedsSubtool,
		"this method is not handled on this endpoint"}
	errStorage = refusal{http.StatusInternalServerError, "UNKNOWN", "",
		"storage failed; nothing from this request was committed"}
)

type errorDetail struct {
	Lazarette string `json:"lazarette"`
}

type errorEntry struct {
	Code    string       `json:"code"`
	Message string       `json:"message"`
	Detail  *errorDetail `json:"detail,omitempty"`
}

type errorBody struct {
	Errors []errorEntry `json:"errors"`
}

// write sends the refusal in the error format of the OCI distribution
// specification. The public code, when there is one, is in detail.lazarette.
func (e refusal) write(w http.ResponseWriter, r *http.Request) {
	entry := errorEntry{Code: e.code, Message: e.message}
	if e.public != "" {
		entry.Detail = &errorDetail{Lazarette: e.public}
	}
	body, _ := json.Marshal(errorBody{Errors: []errorEntry{entry}})
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(e.status)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}
