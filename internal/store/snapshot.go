package store

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	maxSnapshotIndexBytes    = 4 << 20
	maxSnapshotManifestBytes = 4 << 20
	maxSnapshotBlobBytes     = 512 << 20
	maxSnapshotRepoLen       = 255
)

var snapshotRepoGrammar = regexp.MustCompile(`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*)*$`)
var snapshotSigTagGrammar = regexp.MustCompile(`^sha256-([a-f0-9]{64})\.sig$`)

// SnapshotStats reports the objects written to or restored from a store snapshot.
type SnapshotStats struct {
	Repos     int
	Manifests int
	Blobs     int
	Bytes     int64
}

type snapshotEntry struct {
	tarPath string
	disk    string
	mode    int64
	isBlob  bool
	isMan   bool
	hexName string
}

type snapshotManifestCheck struct {
	Config    *snapshotDesc  `json:"config"`
	Layers    []snapshotDesc `json:"layers"`
	Manifests []snapshotDesc `json:"manifests"`
	Subject   *snapshotDesc  `json:"subject"`
}

type snapshotDesc struct {
	Digest string `json:"digest"`
	Size   *int64 `json:"size"`
}

func validSnapshotRepo(name string) bool {
	return len(name) > 0 && len(name) <= maxSnapshotRepoLen && snapshotRepoGrammar.MatchString(name)
}

// WriteSnapshot streams a deterministic gzipped tar archive of all committed
// blobs, manifests and repository indexes in s while holding a read lock on s.
// In-progress uploads and temporary staging files are excluded.
func (s *Store) WriteSnapshot(w io.Writer) (SnapshotStats, error) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

	return writeSnapshotFromRoot(s.root, w)
}

// ListRepos returns the sorted repository names currently stored in s.
func (s *Store) ListRepos() ([]string, error) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

	return s.discoverReposLocked()
}

// WriteSnapshotFromDir streams a deterministic gzipped tar archive from an
// existing store directory on disk without mutating or creating directories in
// root, so it works when the underlying volume is mounted read-only.
func WriteSnapshotFromDir(root string, w io.Writer) (SnapshotStats, error) {
	return writeSnapshotFromRoot(root, w)
}

func writeSnapshotFromRoot(root string, w io.Writer) (SnapshotStats, error) {
	var stats SnapshotStats

	blobsBase := filepath.Join(root, blobsDir)
	if info, err := os.Stat(blobsBase); err != nil || !info.IsDir() {
		if err != nil {
			return stats, fmt.Errorf("stat blobs directory: %w", err)
		}
		return stats, errors.New("blobs path is not a directory")
	}
	manifestsBase := filepath.Join(root, manifestsDir)
	if info, err := os.Stat(manifestsBase); err != nil || !info.IsDir() {
		if err != nil {
			return stats, fmt.Errorf("stat manifests directory: %w", err)
		}
		return stats, errors.New("manifests path is not a directory")
	}

	// Scan manifests before blobs so any concurrent writer on a read-only mount
	// has already committed every blob referenced by a discovered manifest.
	repos, err := discoverRepos(manifestsBase)
	if err != nil {
		return stats, err
	}

	var entries []snapshotEntry
	activeRepos := 0

	for _, repo := range repos {
		if !validSnapshotRepo(repo) {
			return stats, fmt.Errorf("invalid repository path in store: %q", repo)
		}
		repoDisk := filepath.Join(manifestsBase, filepath.FromSlash(repo))
		idxDisk := filepath.Join(repoDisk, indexFile)
		hasRepoContent := false

		if info, err := os.Lstat(idxDisk); err == nil {
			if !info.Mode().IsRegular() {
				return stats, fmt.Errorf("index %s is not a regular file", idxDisk)
			}
			entries = append(entries, snapshotEntry{
				tarPath: path.Join(manifestsDir, repo, indexFile),
				disk:    idxDisk,
				mode:    0o640,
			})
			hasRepoContent = true
		} else if !errors.Is(err, fs.ErrNotExist) {
			return stats, err
		}

		leafDisk := filepath.Join(repoDisk, manifestLeaf)
		leafEntries, err := os.ReadDir(leafDisk)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return stats, err
		}
		for _, e := range leafEntries {
			if !sha256Hex.MatchString(e.Name()) {
				return stats, fmt.Errorf("unexpected manifest filename %q in %s", e.Name(), repo)
			}
			full := filepath.Join(leafDisk, e.Name())
			info, err := os.Lstat(full)
			if err != nil {
				return stats, err
			}
			if !info.Mode().IsRegular() {
				return stats, fmt.Errorf("manifest %s is not a regular file", full)
			}
			entries = append(entries, snapshotEntry{
				tarPath: path.Join(manifestsDir, repo, manifestLeaf, e.Name()),
				disk:    full,
				mode:    0o440,
				isMan:   true,
				hexName: e.Name(),
			})
			hasRepoContent = true
		}
		if hasRepoContent {
			activeRepos++
		}
	}

	blobEntries, err := os.ReadDir(blobsBase)
	if err != nil {
		return stats, err
	}
	for _, e := range blobEntries {
		if !sha256Hex.MatchString(e.Name()) {
			return stats, fmt.Errorf("unexpected blob filename %q", e.Name())
		}
		full := filepath.Join(blobsBase, e.Name())
		info, err := os.Lstat(full)
		if err != nil {
			return stats, err
		}
		if !info.Mode().IsRegular() {
			return stats, fmt.Errorf("blob %s is not a regular file", full)
		}
		entries = append(entries, snapshotEntry{
			tarPath: path.Join(blobsDir, e.Name()),
			disk:    full,
			mode:    0o440,
			isBlob:  true,
			hexName: e.Name(),
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].tarPath < entries[j].tarPath
	})

	zw, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return stats, err
	}
	zw.Header.ModTime = time.Unix(0, 0).UTC()
	zw.Header.OS = 255

	tw := tar.NewWriter(zw)
	epoch := time.Unix(0, 0).UTC()

	for _, ent := range entries {
		data, size, err := readVerifiedSnapshotFile(ent)
		if err != nil {
			return stats, err
		}
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     ent.tarPath,
			Size:     size,
			Mode:     ent.mode,
			Uid:      0,
			Gid:      0,
			ModTime:  epoch,
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return stats, err
		}
		if data != nil {
			if _, err := tw.Write(data); err != nil {
				return stats, err
			}
		} else {
			if err := copyVerifiedBlobToTar(tw, ent.disk, ent.hexName, size); err != nil {
				return stats, err
			}
		}
		stats.Bytes += size
		if ent.isBlob {
			stats.Blobs++
		} else if ent.isMan {
			stats.Manifests++
		}
	}

	if err := tw.Close(); err != nil {
		return stats, err
	}
	if err := zw.Close(); err != nil {
		return stats, err
	}
	stats.Repos = activeRepos
	return stats, nil
}

func readVerifiedSnapshotFile(ent snapshotEntry) ([]byte, int64, error) {
	if ent.isBlob {
		info, err := os.Stat(ent.disk)
		if err != nil {
			return nil, 0, err
		}
		if info.Size() > maxSnapshotBlobBytes {
			return nil, 0, fmt.Errorf("blob %s exceeds %d bytes", ent.hexName, maxSnapshotBlobBytes)
		}
		return nil, info.Size(), nil
	}

	limit := int64(maxSnapshotIndexBytes)
	if ent.isMan {
		limit = maxSnapshotManifestBytes
	}
	f, err := os.Open(ent.disk)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	f.Close()
	if err != nil {
		return nil, 0, err
	}
	if int64(len(data)) > limit {
		return nil, 0, fmt.Errorf("snapshot entry %s exceeds maximum size", ent.tarPath)
	}
	if ent.isMan {
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != ent.hexName {
			return nil, 0, fmt.Errorf("manifest %s failed sha256 verification: %w", ent.tarPath, ErrDigestMismatch)
		}
	} else {
		var idx repoIndex
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&idx); err != nil {
			return nil, 0, fmt.Errorf("index %s is invalid JSON: %w", ent.tarPath, err)
		}
	}
	return data, int64(len(data)), nil
}

func copyVerifiedBlobToTar(tw *tar.Writer, diskPath, wantHex string, expectedSize int64) error {
	f, err := os.Open(diskPath)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tw, h), io.LimitReader(f, expectedSize+1))
	if err != nil {
		return err
	}
	if n != expectedSize {
		return fmt.Errorf("blob %s changed size during snapshot", wantHex)
	}
	if hex.EncodeToString(h.Sum(nil)) != wantHex {
		return fmt.Errorf("blob %s failed sha256 verification: %w", wantHex, ErrDigestMismatch)
	}
	return nil
}

// RestoreSnapshot unpacks a gzipped tar snapshot produced by WriteSnapshot into
// dstDir, verifying every blob and manifest sha256 digest and every repository
// index and descriptor reference before returning success. dstDir must either
// not exist or be an empty directory; on error any partial files written to
// dstDir are removed.
func RestoreSnapshot(r io.Reader, dstDir string) (SnapshotStats, error) {
	var stats SnapshotStats

	existed := false
	if info, err := os.Stat(dstDir); err == nil {
		if !info.IsDir() {
			return stats, errors.New("destination path exists and is not a directory")
		}
		entries, err := os.ReadDir(dstDir)
		if err != nil {
			return stats, err
		}
		if len(entries) > 0 {
			return stats, errors.New("destination directory is not empty")
		}
		existed = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return stats, err
	}

	cleanup := func() {
		if !existed {
			_ = os.RemoveAll(dstDir)
			return
		}
		entries, err := os.ReadDir(dstDir)
		if err != nil {
			return
		}
		for _, e := range entries {
			_ = os.RemoveAll(filepath.Join(dstDir, e.Name()))
		}
	}

	for _, d := range []string{blobsDir, manifestsDir, uploadsDir, tmpDir} {
		if err := os.MkdirAll(filepath.Join(dstDir, d), 0o750); err != nil {
			cleanup()
			return stats, err
		}
	}

	zr, err := gzip.NewReader(r)
	if err != nil {
		cleanup()
		return stats, fmt.Errorf("open gzip stream: %w", err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	seenPaths := map[string]bool{}
	blobSizes := map[string]int64{}
	repoManifests := map[string]map[string][]byte{}
	repoIndexes := map[string]repoIndex{}
	reposSeen := map[string]bool{}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			cleanup()
			return stats, fmt.Errorf("read tar entry: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			cleanup()
			return stats, fmt.Errorf("unsupported tar entry type %d for %q", hdr.Typeflag, hdr.Name)
		}
		if err := validateSnapshotTarPath(hdr.Name); err != nil {
			cleanup()
			return stats, err
		}
		if seenPaths[hdr.Name] {
			cleanup()
			return stats, fmt.Errorf("duplicate tar entry %q", hdr.Name)
		}
		seenPaths[hdr.Name] = true
		if hdr.Size < 0 {
			cleanup()
			return stats, fmt.Errorf("negative size on tar entry %q", hdr.Name)
		}

		kind, repo, hexDigest, err := classifySnapshotPath(hdr.Name)
		if err != nil {
			cleanup()
			return stats, err
		}

		switch kind {
		case "blob":
			if hdr.Size > maxSnapshotBlobBytes {
				cleanup()
				return stats, fmt.Errorf("blob %s exceeds maximum size", hexDigest)
			}
			dst := filepath.Join(dstDir, blobsDir, hexDigest)
			if err := writeAndVerifyBlob(tr, dst, hexDigest, hdr.Size); err != nil {
				cleanup()
				return stats, err
			}
			blobSizes["sha256:"+hexDigest] = hdr.Size
			stats.Blobs++
			stats.Bytes += hdr.Size

		case "manifest":
			if hdr.Size > maxSnapshotManifestBytes {
				cleanup()
				return stats, fmt.Errorf("manifest %s in %s exceeds maximum size", hexDigest, repo)
			}
			data, err := io.ReadAll(io.LimitReader(tr, hdr.Size+1))
			if err != nil || int64(len(data)) != hdr.Size {
				cleanup()
				return stats, fmt.Errorf("read manifest %s in %s: %w", hexDigest, repo, err)
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != hexDigest {
				cleanup()
				return stats, fmt.Errorf("manifest %s in %s: %w", hexDigest, repo, ErrDigestMismatch)
			}
			dst := filepath.Join(dstDir, manifestsDir, filepath.FromSlash(repo), manifestLeaf, hexDigest)
			if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
				cleanup()
				return stats, err
			}
			if err := os.WriteFile(dst, data, 0o440); err != nil {
				cleanup()
				return stats, err
			}
			if repoManifests[repo] == nil {
				repoManifests[repo] = map[string][]byte{}
			}
			repoManifests[repo]["sha256:"+hexDigest] = data
			reposSeen[repo] = true
			stats.Manifests++
			stats.Bytes += hdr.Size

		case "index":
			if hdr.Size > maxSnapshotIndexBytes {
				cleanup()
				return stats, fmt.Errorf("index for %s exceeds maximum size", repo)
			}
			data, err := io.ReadAll(io.LimitReader(tr, hdr.Size+1))
			if err != nil || int64(len(data)) != hdr.Size {
				cleanup()
				return stats, fmt.Errorf("read index for %s: %w", repo, err)
			}
			var idx repoIndex
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&idx); err != nil {
				cleanup()
				return stats, fmt.Errorf("invalid index JSON for %s: %w", repo, err)
			}
			if _, err := dec.Token(); err != io.EOF {
				cleanup()
				return stats, fmt.Errorf("trailing data in index JSON for %s", repo)
			}
			dst := filepath.Join(dstDir, manifestsDir, filepath.FromSlash(repo), indexFile)
			if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
				cleanup()
				return stats, err
			}
			if err := os.WriteFile(dst, data, 0o640); err != nil {
				cleanup()
				return stats, err
			}
			repoIndexes[repo] = idx
			reposSeen[repo] = true
			stats.Bytes += hdr.Size
		}
	}

	if err := verifyRestoredGraph(reposSeen, repoManifests, repoIndexes, blobSizes); err != nil {
		cleanup()
		return stats, err
	}

	stats.Repos = len(reposSeen)
	return stats, nil
}

func validateSnapshotTarPath(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return fmt.Errorf("invalid tar path %q", name)
	}
	if strings.Contains(name, "\\") || strings.Contains(name, "//") {
		return fmt.Errorf("invalid tar path %q", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("disallowed path segment in %q", name)
		}
	}
	if path.Clean(name) != name {
		return fmt.Errorf("non-canonical tar path %q", name)
	}
	return nil
}

func classifySnapshotPath(name string) (kind, repo, hexDigest string, err error) {
	if hexName, ok := strings.CutPrefix(name, blobsDir+"/"); ok {
		if !sha256Hex.MatchString(hexName) {
			return "", "", "", fmt.Errorf("invalid blob path %q", name)
		}
		return "blob", "", hexName, nil
	}
	rest, ok := strings.CutPrefix(name, manifestsDir+"/")
	if !ok {
		return "", "", "", fmt.Errorf("unfamiliar snapshot path %q", name)
	}
	if repoPath, ok := strings.CutSuffix(rest, "/"+indexFile); ok {
		if !validSnapshotRepo(repoPath) {
			return "", "", "", fmt.Errorf("invalid repository in snapshot path %q", name)
		}
		return "index", repoPath, "", nil
	}
	idx := strings.LastIndex(rest, "/"+manifestLeaf+"/")
	if idx > 0 {
		repoPath := rest[:idx]
		hexName := rest[idx+len("/"+manifestLeaf+"/"):]
		if validSnapshotRepo(repoPath) && sha256Hex.MatchString(hexName) {
			return "manifest", repoPath, hexName, nil
		}
	}
	return "", "", "", fmt.Errorf("invalid manifest snapshot path %q", name)
}

func writeAndVerifyBlob(r io.Reader, dstPath, wantHex string, wantSize int64) error {
	f, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o440)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, wantSize+1))
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n != wantSize {
		return fmt.Errorf("blob %s truncated: got %d bytes, want %d", wantHex, n, wantSize)
	}
	if hex.EncodeToString(h.Sum(nil)) != wantHex {
		return fmt.Errorf("blob %s: %w", wantHex, ErrDigestMismatch)
	}
	return nil
}

func verifyRestoredGraph(
	reposSeen map[string]bool,
	repoManifests map[string]map[string][]byte,
	repoIndexes map[string]repoIndex,
	blobSizes map[string]int64,
) error {
	for repo := range reposSeen {
		manifests := repoManifests[repo]
		for manDigest, raw := range manifests {
			var parsed snapshotManifestCheck
			if err := json.Unmarshal(raw, &parsed); err != nil {
				return fmt.Errorf("manifest %s in %s is invalid JSON: %w", manDigest, repo, err)
			}
			var blobDescs []snapshotDesc
			if parsed.Config != nil {
				blobDescs = append(blobDescs, *parsed.Config)
			}
			blobDescs = append(blobDescs, parsed.Layers...)
			for _, bd := range blobDescs {
				if _, err := ParseDigest(bd.Digest); err != nil {
					return fmt.Errorf("manifest %s in %s has invalid blob digest %q: %w", manDigest, repo, bd.Digest, err)
				}
				actual, ok := blobSizes[bd.Digest]
				if !ok {
					return fmt.Errorf("manifest %s in %s references missing blob %s", manDigest, repo, bd.Digest)
				}
				if bd.Size != nil && actual != *bd.Size {
					return fmt.Errorf("manifest %s in %s blob %s size mismatch: got %d, want %d", manDigest, repo, bd.Digest, actual, *bd.Size)
				}
			}

			var manDescs []snapshotDesc
			manDescs = append(manDescs, parsed.Manifests...)
			if parsed.Subject != nil {
				manDescs = append(manDescs, *parsed.Subject)
			}
			for _, md := range manDescs {
				if _, err := ParseDigest(md.Digest); err != nil {
					return fmt.Errorf("manifest %s in %s has invalid manifest ref %q: %w", manDigest, repo, md.Digest, err)
				}
				targetBytes, ok := manifests[md.Digest]
				if !ok {
					return fmt.Errorf("manifest %s in %s references missing manifest %s", manDigest, repo, md.Digest)
				}
				if md.Size != nil && int64(len(targetBytes)) != *md.Size {
					return fmt.Errorf("manifest %s in %s ref %s size mismatch: got %d, want %d", manDigest, repo, md.Digest, len(targetBytes), *md.Size)
				}
			}
		}

		idx, hasIdx := repoIndexes[repo]
		if !hasIdx {
			continue
		}
		if len(idx.Retained) > MaxRetainedDigests {
			return fmt.Errorf("index for %s retains %d digests, max %d", repo, len(idx.Retained), MaxRetainedDigests)
		}
		for _, d := range idx.Retained {
			if _, err := ParseDigest(d); err != nil {
				return fmt.Errorf("index for %s has invalid retained digest %q: %w", repo, d, err)
			}
			if _, ok := manifests[d]; !ok {
				return fmt.Errorf("index for %s retains missing manifest %s", repo, d)
			}
		}
		for tag, target := range idx.Tags {
			m := snapshotSigTagGrammar.FindStringSubmatch(tag)
			if m == nil {
				return fmt.Errorf("index for %s has invalid signature tag %q", repo, tag)
			}
			if _, err := ParseDigest(target); err != nil {
				return fmt.Errorf("index for %s tag %s has invalid target %q: %w", repo, tag, target, err)
			}
			if _, ok := manifests[target]; !ok {
				return fmt.Errorf("index for %s tag %s points to missing manifest %s", repo, tag, target)
			}
			subjDigest := "sha256:" + m[1]
			if _, ok := manifests[subjDigest]; !ok {
				return fmt.Errorf("index for %s tag %s signs missing subject %s", repo, tag, subjDigest)
			}
		}
		for subj, list := range idx.Referrers {
			if _, err := ParseDigest(subj); err != nil {
				return fmt.Errorf("index for %s has invalid referrer subject %q: %w", repo, subj, err)
			}
			if _, ok := manifests[subj]; !ok {
				return fmt.Errorf("index for %s lists referrers for missing subject %s", repo, subj)
			}
			for _, ref := range list {
				if _, err := ParseDigest(ref.Digest); err != nil {
					return fmt.Errorf("index for %s subject %s has invalid referrer digest %q: %w", repo, subj, ref.Digest, err)
				}
				refBytes, ok := manifests[ref.Digest]
				if !ok {
					return fmt.Errorf("index for %s subject %s points to missing referrer %s", repo, subj, ref.Digest)
				}
				if ref.Size != int64(len(refBytes)) {
					return fmt.Errorf("index for %s referrer %s size mismatch: got %d, want %d", repo, ref.Digest, len(refBytes), ref.Size)
				}
			}
		}
	}
	return nil
}
