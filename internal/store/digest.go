// Package store keeps blobs and manifests on disk, addressed by sha256 digest.
package store

import (
	"errors"
	"regexp"
	"strings"
)

// Errors returned when a digest string is not usable.
var (
	// ErrDigestMalformed means the text is not a digest at all.
	ErrDigestMalformed = errors.New("digest is malformed")
	// ErrDigestUnfamiliar means the text is a well-formed digest that uses an
	// algorithm other than sha256.
	ErrDigestUnfamiliar = errors.New("digest algorithm is not sha256")
)

var (
	sha256Hex     = regexp.MustCompile(`^[a-f0-9]{64}$`)
	digestGrammar = regexp.MustCompile(`^[a-z0-9]+(?:[+._-][a-z0-9]+)*:[a-zA-Z0-9=_-]+$`)
)

// Digest is a validated sha256 digest. The zero value is not valid.
type Digest struct {
	hex string
}

// ParseDigest accepts exactly "sha256:" followed by 64 lower-case hex
// characters. Any other well-formed digest is reported as unfamiliar, and
// everything else as malformed.
func ParseDigest(s string) (Digest, error) {
	if !digestGrammar.MatchString(s) {
		return Digest{}, ErrDigestMalformed
	}
	algorithm, encoded, _ := strings.Cut(s, ":")
	if algorithm != "sha256" {
		return Digest{}, ErrDigestUnfamiliar
	}
	if !sha256Hex.MatchString(encoded) {
		return Digest{}, ErrDigestMalformed
	}
	return Digest{hex: encoded}, nil
}

// String returns the digest in "sha256:<hex>" form.
func (d Digest) String() string { return "sha256:" + d.hex }

// Hex returns the 64 hex characters without the algorithm prefix.
func (d Digest) Hex() string { return d.hex }
