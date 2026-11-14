package registry

import (
	"regexp"
	"strings"
)

type routeKind int

const (
	routeUnknown     routeKind = iota
	routeBase                  // /v2/
	routeBlob                  // /v2/<name>/blobs/<digest>
	routeUploads               // /v2/<name>/blobs/uploads/
	routeUpload                // /v2/<name>/blobs/uploads/<id>
	routeManifest              // /v2/<name>/manifests/<reference>
	routeReferrers             // /v2/<name>/referrers/<digest>
	routeTagsList              // /v2/<name>/tags/list
	routeDigestsList           // /v2/<name>/digests/list
	routeBadName               // one of the above with an invalid <name>
)

func (k routeKind) String() string {
	switch k {
	case routeBase:
		return "base"
	case routeBlob:
		return "blob"
	case routeUploads:
		return "uploads"
	case routeUpload:
		return "upload"
	case routeManifest:
		return "manifest"
	case routeReferrers:
		return "referrers"
	case routeTagsList:
		return "tags-list"
	case routeDigestsList:
		return "digests-list"
	case routeBadName:
		return "bad-name"
	}
	return "unknown"
}

type route struct {
	kind routeKind
	repo string
	ref  string // digest, upload id or manifest reference
}

// Repository name grammar from the OCI distribution specification.
var repoName = regexp.MustCompile(`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*)*$`)

const maxRepoName = 255

func validRepo(name string) bool {
	return len(name) <= maxRepoName && repoName.MatchString(name)
}

// parseRoute maps a request path to one of the handled endpoints. Anything
// it does not recognise is routeUnknown; nothing is guessed.
func parseRoute(path string) route {
	if path == "/v2" || path == "/v2/" {
		return route{kind: routeBase}
	}
	rest, ok := strings.CutPrefix(path, "/v2/")
	if !ok {
		return route{kind: routeUnknown}
	}
	var rt route
	if repo, ok := strings.CutSuffix(rest, "/blobs/uploads/"); ok {
		rt = route{kind: routeUploads, repo: repo}
	} else if repo, ok := strings.CutSuffix(rest, "/blobs/uploads"); ok {
		rt = route{kind: routeUploads, repo: repo}
	} else if repo, ref, ok := cutLast(rest, "/blobs/uploads/"); ok {
		rt = route{kind: routeUpload, repo: repo, ref: ref}
	} else if repo, ok := strings.CutSuffix(rest, "/tags/list"); ok {
		rt = route{kind: routeTagsList, repo: repo}
	} else if repo, ok := strings.CutSuffix(rest, "/digests/list"); ok {
		rt = route{kind: routeDigestsList, repo: repo}
	} else if repo, ref, ok := cutLast(rest, "/referrers/"); ok {
		rt = route{kind: routeReferrers, repo: repo, ref: ref}
	} else if repo, ref, ok := cutLast(rest, "/manifests/"); ok {
		rt = route{kind: routeManifest, repo: repo, ref: ref}
	} else if repo, ref, ok := cutLast(rest, "/blobs/"); ok {
		rt = route{kind: routeBlob, repo: repo, ref: ref}
	} else {
		return route{kind: routeUnknown}
	}
	if !validRepo(rt.repo) {
		return route{kind: routeBadName}
	}
	return rt
}

// cutLast splits s around the last sep, and succeeds only when what follows
// is a single non-empty path segment.
func cutLast(s, sep string) (before, after string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return "", "", false
	}
	before, after = s[:i], s[i+len(sep):]
	if after == "" || strings.Contains(after, "/") {
		return "", "", false
	}
	return before, after, true
}
