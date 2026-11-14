package store

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// GCStats reports what one garbage collection pass removed.
type GCStats struct {
	ManifestsRemoved int
	BlobsRemoved     int
	UploadsRemoved   int
	BytesFreed       int64
}

type storedManifestMeta struct {
	size    int64
	modTime time.Time
	data    []byte
}

type manifestRefs struct {
	Config    *refDescriptor  `json:"config"`
	Layers    []refDescriptor `json:"layers"`
	Manifests []refDescriptor `json:"manifests"`
	Subject   *refDescriptor  `json:"subject"`
}

type refDescriptor struct {
	Digest string `json:"digest"`
}

// CollectGarbage removes abandoned upload sessions, manifests outside each
// repository's 5-signed-digest retention window, and unreferenced blobs.
//
// When grace is positive, any upload session, manifest or blob whose
// modification time is at or after now.Add(-grace) is spared so an in-flight
// build and signature push is never interrupted.
func (s *Store) CollectGarbage(now time.Time, grace time.Duration) (GCStats, error) {
	if grace < 0 {
		grace = 0
	}
	cutoff := now.Add(-grace)
	spared := func(modTime time.Time) bool {
		return grace > 0 && !modTime.Before(cutoff)
	}

	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	var stats GCStats

	// 1. Prune unlocked, abandoned upload sessions.
	uploadsRoot := filepath.Join(s.root, uploadsDir)
	uploads, err := os.ReadDir(uploadsRoot)
	if err != nil {
		return stats, err
	}
	uploadsChanged := false
	for _, e := range uploads {
		if e.IsDir() || s.hasActiveUploadLock(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return stats, err
		}
		if spared(info.ModTime()) {
			continue
		}
		if err := os.Remove(filepath.Join(uploadsRoot, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return stats, err
		}
		stats.UploadsRemoved++
		stats.BytesFreed += info.Size()
		uploadsChanged = true
	}
	if uploadsChanged {
		if err := syncDir(uploadsRoot); err != nil {
			return stats, err
		}
	}

	// 2. Discover all repositories under manifests/.
	repos, err := s.discoverReposLocked()
	if err != nil {
		return stats, err
	}

	// 3. Prune manifests, tags and referrers per repository, and collect
	// every blob digest still referenced by a kept manifest.
	referencedBlobs := map[string]bool{}
	for _, repo := range repos {
		leafDir := filepath.Join(s.repoDir(repo), manifestLeaf)
		onDisk := map[string]storedManifestMeta{}
		entries, err := os.ReadDir(leafDir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return stats, err
		}
		for _, e := range entries {
			if e.IsDir() || !sha256Hex.MatchString(e.Name()) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				return stats, err
			}
			data, err := os.ReadFile(filepath.Join(leafDir, e.Name()))
			if err != nil {
				return stats, err
			}
			onDisk["sha256:"+e.Name()] = storedManifestMeta{
				size:    info.Size(),
				modTime: info.ModTime(),
				data:    data,
			}
		}

		idx, idxErr := s.readIndexLocked(repo)
		hasIndexFile := idxErr == nil
		if idxErr != nil && !errors.Is(idxErr, fs.ErrNotExist) {
			return stats, idxErr
		}

		keep := map[string]bool{}
		var queue []string
		mark := func(digest string) {
			if keep[digest] {
				return
			}
			if _, ok := onDisk[digest]; !ok {
				return
			}
			keep[digest] = true
			queue = append(queue, digest)
		}

		for _, d := range idx.Retained {
			mark(d)
		}
		for d, meta := range onDisk {
			if spared(meta.modTime) {
				mark(d)
			}
		}

		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]

			if parsed, err := ParseDigest(cur); err == nil {
				if sigDigest, ok := idx.Tags["sha256-"+parsed.Hex()+".sig"]; ok {
					mark(sigDigest)
				}
			}
			for _, ref := range idx.Referrers[cur] {
				mark(ref.Digest)
			}

			var refs manifestRefs
			if json.Unmarshal(onDisk[cur].data, &refs) == nil {
				for _, child := range refs.Manifests {
					mark(child.Digest)
				}
				if refs.Subject != nil {
					mark(refs.Subject.Digest)
				}
			}
		}

		leafChanged := false
		for d, meta := range onDisk {
			if keep[d] {
				continue
			}
			parsed, err := ParseDigest(d)
			if err != nil {
				continue
			}
			if err := os.Remove(filepath.Join(leafDir, parsed.Hex())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return stats, err
			}
			stats.ManifestsRemoved++
			stats.BytesFreed += meta.size
			leafChanged = true
		}
		if leafChanged {
			if err := syncDir(leafDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return stats, err
			}
		}

		cleaned := cleanIndex(idx, keep)
		if len(keep) == 0 {
			if hasIndexFile {
				if err := os.Remove(s.indexPath(repo)); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return stats, err
				}
			}
			_ = os.Remove(leafDir)
			s.pruneEmptyRepoDirs(repo)
			continue
		}
		if hasIndexFile || len(cleaned.Retained) > 0 || len(cleaned.Tags) > 0 || len(cleaned.Referrers) > 0 {
			if err := s.writeIndexLocked(repo, cleaned); err != nil {
				return stats, err
			}
		}

		for d := range keep {
			var refs manifestRefs
			if json.Unmarshal(onDisk[d].data, &refs) != nil {
				continue
			}
			if refs.Config != nil {
				if bd, err := ParseDigest(refs.Config.Digest); err == nil {
					referencedBlobs[bd.Hex()] = true
				}
			}
			for _, layer := range refs.Layers {
				if bd, err := ParseDigest(layer.Digest); err == nil {
					referencedBlobs[bd.Hex()] = true
				}
			}
		}
	}

	// 4. Prune unreferenced blobs outside the grace window.
	blobsRoot := filepath.Join(s.root, blobsDir)
	blobs, err := os.ReadDir(blobsRoot)
	if err != nil {
		return stats, err
	}
	blobsChanged := false
	for _, e := range blobs {
		if e.IsDir() || referencedBlobs[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return stats, err
		}
		if spared(info.ModTime()) {
			continue
		}
		if err := os.Remove(filepath.Join(blobsRoot, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return stats, err
		}
		stats.BlobsRemoved++
		stats.BytesFreed += info.Size()
		blobsChanged = true
	}
	if blobsChanged {
		if err := syncDir(blobsRoot); err != nil {
			return stats, err
		}
	}

	return stats, nil
}

func (s *Store) discoverReposLocked() ([]string, error) {
	return discoverRepos(filepath.Join(s.root, manifestsDir))
}

func discoverRepos(base string) ([]string, error) {
	seen := map[string]bool{}
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == base {
			return nil
		}
		if d.IsDir() && d.Name() == manifestLeaf {
			rel, rerr := filepath.Rel(base, filepath.Dir(path))
			if rerr == nil && rel != "." {
				seen[filepath.ToSlash(rel)] = true
			}
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == indexFile {
			rel, rerr := filepath.Rel(base, filepath.Dir(path))
			if rerr == nil && rel != "." {
				seen[filepath.ToSlash(rel)] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	repos := make([]string, 0, len(seen))
	for r := range seen {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	return repos, nil
}

func (s *Store) pruneEmptyRepoDirs(repo string) {
	base := filepath.Join(s.root, manifestsDir)
	dir := s.repoDir(repo)
	for dir != base && strings.HasPrefix(dir, base+string(filepath.Separator)) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

func cleanIndex(idx repoIndex, keep map[string]bool) repoIndex {
	var out repoIndex
	for _, d := range idx.Retained {
		if keep[d] {
			out.Retained = append(out.Retained, d)
		}
	}
	if len(out.Retained) > MaxRetainedDigests {
		out.Retained = out.Retained[len(out.Retained)-MaxRetainedDigests:]
	}
	for tag, target := range idx.Tags {
		if !keep[target] {
			continue
		}
		hexPart, ok := strings.CutPrefix(tag, "sha256-")
		if !ok {
			continue
		}
		hexPart, ok = strings.CutSuffix(hexPart, ".sig")
		if !ok || !keep["sha256:"+hexPart] {
			continue
		}
		if out.Tags == nil {
			out.Tags = map[string]string{}
		}
		out.Tags[tag] = target
	}
	for subj, list := range idx.Referrers {
		if !keep[subj] {
			continue
		}
		var kept []ReferrerEntry
		for _, r := range list {
			if keep[r.Digest] {
				kept = append(kept, cloneReferrer(r))
			}
		}
		if len(kept) > 0 {
			if out.Referrers == nil {
				out.Referrers = map[string][]ReferrerEntry{}
			}
			out.Referrers[subj] = kept
		}
	}
	return out
}
