package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
)

// MaxRetainedDigests is the number of signed image digests kept per app for
// rollback.
const MaxRetainedDigests = 5

// Layout under the storage root. Every write happens below the root, so the
// rest of the filesystem can be read-only.
//
//	blobs/sha256/<hex>               committed blobs (layers and configs)
//	manifests/<repo>/_sha256/<hex>   committed manifests, per repository
//	manifests/<repo>/_index.json     retained signed digests, .sig tags and referrers
//	uploads/<id>                     blob upload sessions in progress
//	tmp/                             files being written before an atomic rename
//
// A repository name component can never start with an underscore, so
// "_sha256" and "_index.json" cannot collide with a repository path.
const (
	blobsDir     = "blobs/sha256"
	manifestsDir = "manifests"
	uploadsDir   = "uploads"
	tmpDir       = "tmp"
	manifestLeaf = "_sha256"
	indexFile    = "_index.json"
)

var (
	// ErrNotFound means the blob, manifest, repository or upload session does not exist.
	ErrNotFound = errors.New("not found")
	// ErrExists means an upload session with that identifier already exists.
	ErrExists = errors.New("already exists")
	// ErrDigestMismatch means the content does not hash to the digest given.
	ErrDigestMismatch = errors.New("content does not match digest")
	// ErrOffset means a chunk did not start where the session currently ends.
	ErrOffset = errors.New("chunk does not start at the end of the upload")
	// ErrLength means a chunk was not the length the caller declared.
	ErrLength = errors.New("chunk is not the declared length")
	// ErrBadID means the upload identifier is not in the accepted form.
	ErrBadID = errors.New("upload identifier is malformed")
)

// BodyError wraps a failure to read the incoming data, as opposed to a
// failure of the storage underneath.
type BodyError struct{ Err error }

func (e *BodyError) Error() string { return "reading incoming data: " + e.Err.Error() }
func (e *BodyError) Unwrap() error { return e.Err }

// ReferrerEntry is one OCI v1.1 referrer descriptor stored for a subject.
type ReferrerEntry struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// ManifestOptions carries optional index updates committed alongside a manifest.
type ManifestOptions struct {
	// Tag binds a cosign signature tag ("sha256-<hex>.sig") in the repository
	// to the manifest digest being stored.
	Tag string
	// Subject and Referrer record the stored manifest in the repository's
	// referrers list for Subject.
	Subject  *Digest
	Referrer *ReferrerEntry
	// SignedSubject records *SignedSubject in the repository's 5-digest
	// signed retention window (oldest first, newest last).
	SignedSubject *Digest
}

type repoIndex struct {
	Retained  []string                   `json:"retained,omitempty"`
	Tags      map[string]string          `json:"tags,omitempty"`
	Referrers map[string][]ReferrerEntry `json:"referrers,omitempty"`
}

var uploadID = regexp.MustCompile(`^[a-f0-9]{32}$`)

// Store is a disk-backed, content-addressed store.
type Store struct {
	root string

	// stateMu coordinates commits, index reads/writes and garbage collection.
	// Network body reads happen outside stateMu so a slow client never blocks
	// other requests.
	stateMu sync.RWMutex

	mu    sync.Mutex
	locks map[string]*uploadLock
}

type uploadLock struct {
	mu   sync.Mutex
	refs int
}

// Open prepares the directory layout under root and returns a Store. Files
// left in tmp/ by an interrupted write are removed; they were never visible.
func Open(root string) (*Store, error) {
	for _, d := range []string{blobsDir, manifestsDir, uploadsDir, tmpDir} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			return nil, err
		}
	}
	leftovers, err := os.ReadDir(filepath.Join(root, tmpDir))
	if err != nil {
		return nil, err
	}
	for _, e := range leftovers {
		if err := os.Remove(filepath.Join(root, tmpDir, e.Name())); err != nil {
			return nil, err
		}
	}
	return &Store{root: root, locks: map[string]*uploadLock{}}, nil
}

func (s *Store) blobPath(d Digest) string {
	return filepath.Join(s.root, blobsDir, d.Hex())
}

func (s *Store) repoDir(repo string) string {
	return filepath.Join(s.root, manifestsDir, filepath.FromSlash(repo))
}

func (s *Store) manifestPath(repo string, d Digest) string {
	return filepath.Join(s.repoDir(repo), manifestLeaf, d.Hex())
}

func (s *Store) indexPath(repo string) string {
	return filepath.Join(s.repoDir(repo), indexFile)
}

func (s *Store) uploadPath(id string) string {
	return filepath.Join(s.root, uploadsDir, id)
}

func notFound(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

// BlobSize returns the size of a committed blob.
func (s *Store) BlobSize(d Digest) (int64, error) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

	info, err := os.Stat(s.blobPath(d))
	if err != nil {
		return 0, notFound(err)
	}
	return info.Size(), nil
}

// OpenBlob opens a committed blob for reading. The caller closes it.
func (s *Store) OpenBlob(d Digest) (*os.File, int64, error) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

	f, err := os.Open(s.blobPath(d))
	if err != nil {
		return nil, 0, notFound(err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// lockUpload serialises work on one upload session and returns the unlock.
func (s *Store) lockUpload(id string) func() {
	s.mu.Lock()
	l := s.locks[id]
	if l == nil {
		l = &uploadLock{}
		s.locks[id] = l
	}
	l.refs++
	s.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		s.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(s.locks, id)
		}
		s.mu.Unlock()
	}
}

func (s *Store) hasActiveUploadLock(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.locks[id] != nil
}

// NewUpload starts an empty upload session with the given identifier.
func (s *Store) NewUpload(id string) error {
	if !uploadID.MatchString(id) {
		return ErrBadID
	}
	unlock := s.lockUpload(id)
	defer unlock()

	f, err := os.OpenFile(s.uploadPath(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return ErrExists
		}
		return err
	}
	return f.Close()
}

// UploadSize returns how many bytes an upload session holds so far.
func (s *Store) UploadSize(id string) (int64, error) {
	if !uploadID.MatchString(id) {
		return 0, ErrNotFound
	}
	unlock := s.lockUpload(id)
	defer unlock()

	info, err := os.Stat(s.uploadPath(id))
	if err != nil {
		return 0, notFound(err)
	}
	return info.Size(), nil
}

// AppendUpload adds the bytes of r to an upload session and returns the new
// size. If offset is not negative it must equal the current size. If length
// is not negative, exactly that many bytes must arrive. On any failure the
// session is put back to the size it had before the call.
func (s *Store) AppendUpload(id string, offset, length int64, r io.Reader) (int64, error) {
	if !uploadID.MatchString(id) {
		return 0, ErrNotFound
	}
	unlock := s.lockUpload(id)
	defer unlock()

	f, err := os.OpenFile(s.uploadPath(id), os.O_WRONLY, 0)
	if err != nil {
		return 0, notFound(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	before := info.Size()
	if offset >= 0 && offset != before {
		return before, ErrOffset
	}
	if _, err := f.Seek(before, io.SeekStart); err != nil {
		return before, err
	}
	n, err := copyBody(f, r)
	if err == nil && length >= 0 && n != length {
		err = ErrLength
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		if terr := f.Truncate(before); terr != nil {
			return before, fmt.Errorf("%w (and the partial chunk could not be removed: %v)", err, terr)
		}
		return before, err
	}
	return before + n, nil
}

// DropUpload ends an upload session and discards what it holds.
func (s *Store) DropUpload(id string) error {
	if !uploadID.MatchString(id) {
		return ErrNotFound
	}
	unlock := s.lockUpload(id)
	defer unlock()
	return notFound(os.Remove(s.uploadPath(id)))
}

// copyBody copies src to dst with a fixed 32 KiB buffer and tells a failed
// read (BodyError) apart from a failed write.
func copyBody(dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, &BodyError{Err: rerr}
		}
	}
}

// CommitUpload checks that the session's content hashes to d and, if so,
// makes it a committed blob by an atomic rename. The session ends either
// way: on a mismatch its content is discarded.
func (s *Store) CommitUpload(id string, d Digest) error {
	if !uploadID.MatchString(id) {
		return ErrNotFound
	}
	unlock := s.lockUpload(id)
	defer unlock()

	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	path := s.uploadPath(id)
	f, err := os.Open(path)
	if err != nil {
		return notFound(err)
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	f.Close()
	if err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != d.Hex() {
		if err := os.Remove(path); err != nil {
			return err
		}
		return ErrDigestMismatch
	}
	if err := os.Chmod(path, 0o440); err != nil {
		return err
	}
	if err := os.Rename(path, s.blobPath(d)); err != nil {
		return err
	}
	return syncDir(filepath.Dir(s.blobPath(d)))
}

// PutManifest stores manifest bytes for a repository after checking they
// hash to d, and atomically applies any index updates in opts.
func (s *Store) PutManifest(repo string, d Digest, data []byte, opts ManifestOptions) error {
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != d.Hex() {
		return ErrDigestMismatch
	}

	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	final := s.manifestPath(repo, d)
	if err := os.MkdirAll(filepath.Dir(final), 0o750); err != nil {
		return err
	}
	if err := s.writeAtomic(final, data, 0o440); err != nil {
		return err
	}

	if opts.Tag == "" && (opts.Subject == nil || opts.Referrer == nil) && opts.SignedSubject == nil {
		return nil
	}

	idx, err := s.readIndexLocked(repo)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if opts.Tag != "" {
		if idx.Tags == nil {
			idx.Tags = map[string]string{}
		}
		idx.Tags[opts.Tag] = d.String()
	}
	if opts.Subject != nil && opts.Referrer != nil {
		if idx.Referrers == nil {
			idx.Referrers = map[string][]ReferrerEntry{}
		}
		subj := opts.Subject.String()
		entry := cloneReferrer(*opts.Referrer)
		list := idx.Referrers[subj]
		replaced := false
		for i := range list {
			if list[i].Digest == entry.Digest {
				list[i] = entry
				replaced = true
				break
			}
		}
		if !replaced {
			list = append(list, entry)
		}
		idx.Referrers[subj] = list
	}
	if opts.SignedSubject != nil {
		idx.Retained = retainDigest(idx.Retained, opts.SignedSubject.String())
	}
	return s.writeIndexLocked(repo, idx)
}

func retainDigest(list []string, digest string) []string {
	out := make([]string, 0, len(list)+1)
	for _, existing := range list {
		if existing != digest {
			out = append(out, existing)
		}
	}
	out = append(out, digest)
	if len(out) > MaxRetainedDigests {
		out = out[len(out)-MaxRetainedDigests:]
	}
	return out
}

func cloneReferrer(r ReferrerEntry) ReferrerEntry {
	out := r
	if len(r.Annotations) > 0 {
		out.Annotations = maps.Clone(r.Annotations)
	} else {
		out.Annotations = nil
	}
	return out
}

func (s *Store) writeAtomic(final string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Join(s.root, tmpDir), "write-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no effect after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return err
	}
	return syncDir(filepath.Dir(final))
}

func (s *Store) readIndexLocked(repo string) (repoIndex, error) {
	var idx repoIndex
	raw, err := os.ReadFile(s.indexPath(repo))
	if err != nil {
		return idx, err
	}
	if err := json.Unmarshal(raw, &idx); err != nil {
		return repoIndex{}, err
	}
	return idx, nil
}

func (s *Store) writeIndexLocked(repo string, idx repoIndex) error {
	if len(idx.Tags) == 0 {
		idx.Tags = nil
	}
	if len(idx.Referrers) == 0 {
		idx.Referrers = nil
	}
	if len(idx.Retained) == 0 {
		idx.Retained = nil
	}
	raw, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(s.repoDir(repo), 0o750); err != nil {
		return err
	}
	return s.writeAtomic(s.indexPath(repo), raw, 0o640)
}

// GetManifest returns the stored bytes of a manifest in a repository.
func (s *Store) GetManifest(repo string, d Digest) ([]byte, error) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

	data, err := os.ReadFile(s.manifestPath(repo, d))
	if err != nil {
		return nil, notFound(err)
	}
	return data, nil
}

// ResolveTag resolves a stored signature tag in repo to its manifest digest
// and raw bytes.
func (s *Store) ResolveTag(repo, tag string) (Digest, []byte, error) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

	idx, err := s.readIndexLocked(repo)
	if err != nil {
		return Digest{}, nil, notFound(err)
	}
	target, ok := idx.Tags[tag]
	if !ok {
		return Digest{}, nil, ErrNotFound
	}
	d, err := ParseDigest(target)
	if err != nil {
		return Digest{}, nil, err
	}
	data, err := os.ReadFile(s.manifestPath(repo, d))
	if err != nil {
		return Digest{}, nil, notFound(err)
	}
	return d, data, nil
}

// IsTagTarget reports whether d is currently stored as the signature manifest
// of any tag in repo.
func (s *Store) IsTagTarget(repo string, d Digest) (bool, error) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

	idx, err := s.readIndexLocked(repo)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	want := d.String()
	for _, target := range idx.Tags {
		if target == want {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) hasRepoLocked(repo string) (bool, error) {
	entries, err := os.ReadDir(filepath.Join(s.repoDir(repo), manifestLeaf))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

// ListTags returns the sorted signature tags stored in repo, or ErrNotFound if
// repo holds no manifests.
func (s *Store) ListTags(repo string) ([]string, error) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

	exists, err := s.hasRepoLocked(repo)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	idx, err := s.readIndexLocked(repo)
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	tags := make([]string, 0, len(idx.Tags))
	for t := range idx.Tags {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	return tags, nil
}

// ListRetainedDigests returns the retained signed image digests for repo in
// oldest-to-newest order (at most MaxRetainedDigests), or ErrNotFound if repo
// holds no manifests.
func (s *Store) ListRetainedDigests(repo string) ([]string, error) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

	exists, err := s.hasRepoLocked(repo)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	idx, err := s.readIndexLocked(repo)
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]string, len(idx.Retained))
	copy(out, idx.Retained)
	return out, nil
}

// ListReferrers returns the stored OCI v1.1 referrers for subject in repo,
// optionally filtered by artifactType. An unknown repository or subject yields
// an empty slice, matching the OCI distribution specification.
func (s *Store) ListReferrers(repo string, subject Digest, artifactType string) ([]ReferrerEntry, error) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

	idx, err := s.readIndexLocked(repo)
	if errors.Is(err, fs.ErrNotExist) {
		return []ReferrerEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	src := idx.Referrers[subject.String()]
	out := make([]ReferrerEntry, 0, len(src))
	for _, r := range src {
		if artifactType != "" && r.ArtifactType != artifactType {
			continue
		}
		out = append(out, cloneReferrer(r))
	}
	return out, nil
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
