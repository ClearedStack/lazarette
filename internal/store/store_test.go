package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

const id1 = "00000000000000000000000000000001"

func digestOf(s string) Digest {
	sum := sha256.Sum256([]byte(s))
	return Digest{hex: hex.EncodeToString(sum[:])}
}

func storeBlob(t *testing.T, s *Store, id, content string) Digest {
	t.Helper()
	d := digestOf(content)
	if err := s.NewUpload(id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendUpload(id, 0, -1, strings.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitUpload(id, d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestParseDigest(t *testing.T) {
	good := "sha256:" + strings.Repeat("ab", 32)
	cases := []struct {
		in   string
		want error
	}{
		{good, nil},
		{"sha256:" + strings.Repeat("AB", 32), ErrDigestMalformed},
		{"sha256:abc", ErrDigestMalformed},
		{"sha256:" + strings.Repeat("ab", 33), ErrDigestMalformed},
		{"sha512:" + strings.Repeat("ab", 64), ErrDigestUnfamiliar},
		{"blake3:abcd", ErrDigestUnfamiliar},
		{"latest", ErrDigestMalformed},
		{"", ErrDigestMalformed},
		{"sha256:../../etc", ErrDigestMalformed},
	}
	for _, c := range cases {
		d, err := ParseDigest(c.in)
		if err != c.want {
			t.Errorf("ParseDigest(%q) error = %v, want %v", c.in, err, c.want)
		}
		if err == nil && d.String() != c.in {
			t.Errorf("ParseDigest(%q).String() = %q", c.in, d.String())
		}
	}
}

func TestUploadCommitAndRead(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := "made-up blob"
	d := digestOf(content)

	if err := s.NewUpload(id1); err != nil {
		t.Fatal(err)
	}
	if err := s.NewUpload(id1); err != ErrExists {
		t.Errorf("second NewUpload: %v, want ErrExists", err)
	}
	if n, err := s.AppendUpload(id1, 0, 5, strings.NewReader(content[:5])); err != nil || n != 5 {
		t.Fatalf("first chunk: %d, %v", n, err)
	}
	if n, err := s.AppendUpload(id1, 3, -1, strings.NewReader("x")); err != ErrOffset || n != 5 {
		t.Errorf("out-of-order chunk: %d, %v", n, err)
	}
	if n, err := s.AppendUpload(id1, -1, -1, strings.NewReader(content[5:])); err != nil || n != int64(len(content)) {
		t.Fatalf("second chunk: %d, %v", n, err)
	}
	if _, err := s.BlobSize(d); err != ErrNotFound {
		t.Errorf("blob visible before commit: %v", err)
	}
	if err := s.CommitUpload(id1, d); err != nil {
		t.Fatal(err)
	}
	f, size, err := s.OpenBlob(d)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, _ := io.ReadAll(f)
	if string(got) != content || size != int64(len(content)) {
		t.Errorf("read back %q (%d)", got, size)
	}
	if _, err := s.UploadSize(id1); err != ErrNotFound {
		t.Errorf("session still present after commit: %v", err)
	}
}

func TestCommitWithWrongDigestStoresNothing(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root)
	s.NewUpload(id1)
	s.AppendUpload(id1, 0, -1, strings.NewReader("some bytes"))
	if err := s.CommitUpload(id1, digestOf("other bytes")); err != ErrDigestMismatch {
		t.Fatalf("commit: %v, want ErrDigestMismatch", err)
	}
	for _, d := range []string{blobsDir, uploadsDir} {
		if entries, _ := os.ReadDir(filepath.Join(root, d)); len(entries) != 0 {
			t.Errorf("%s holds %d files after a refused commit", d, len(entries))
		}
	}
}

func TestFailedChunkIsRolledBack(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.NewUpload(id1)
	s.AppendUpload(id1, 0, -1, strings.NewReader("kept"))

	broken := io.MultiReader(strings.NewReader("lost"), iotest.ErrReader(errors.New("made-up read failure")))
	_, err := s.AppendUpload(id1, -1, -1, broken)
	var be *BodyError
	if !errors.As(err, &be) {
		t.Fatalf("broken read: %v, want a BodyError", err)
	}
	if _, err := s.AppendUpload(id1, -1, 10, strings.NewReader("short")); err != ErrLength {
		t.Fatalf("short chunk: %v, want ErrLength", err)
	}
	if n, _ := s.UploadSize(id1); n != 4 {
		t.Errorf("session holds %d bytes after two failed chunks, want 4", n)
	}
}

func TestUploadIdentifiersCannotLeaveTheStore(t *testing.T) {
	s, _ := Open(t.TempDir())
	for _, id := range []string{"", "../escape", "short", strings.Repeat("g", 32), strings.Repeat("A", 32)} {
		if err := s.NewUpload(id); err != ErrBadID {
			t.Errorf("NewUpload(%q): %v, want ErrBadID", id, err)
		}
		if _, err := s.AppendUpload(id, -1, -1, strings.NewReader("x")); err != ErrNotFound {
			t.Errorf("AppendUpload(%q): %v, want ErrNotFound", id, err)
		}
	}
}

func TestManifestsArePerRepositoryAndSurviveReopen(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root)
	data := []byte(`{"made-up":"manifest"}`)
	d := digestOf(string(data))

	if err := s.PutManifest("apps/example", digestOf("something else"), data, ManifestOptions{}); err != ErrDigestMismatch {
		t.Fatalf("mismatched manifest: %v", err)
	}
	if err := s.PutManifest("apps/example", d, data, ManifestOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetManifest("apps/other", d); err != ErrNotFound {
		t.Errorf("manifest visible in another repository: %v", err)
	}

	// An interrupted write leaves a file in tmp/; opening the store again
	// clears it and keeps what was committed.
	stray := filepath.Join(root, tmpDir, "manifest-stray")
	os.WriteFile(stray, []byte("half"), 0o640)
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.GetManifest("apps/example", d); err != nil || string(got) != string(data) {
		t.Errorf("after reopen: %q, %v", got, err)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Errorf("stray temporary file survived reopen")
	}
}

func TestRetentionTagsAndReferrersPersistAcrossReopen(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.ListRetainedDigests("apps/unknown"); err != ErrNotFound {
		t.Errorf("unknown repo digests: %v, want ErrNotFound", err)
	}
	if _, err := s.ListTags("apps/unknown"); err != ErrNotFound {
		t.Errorf("unknown repo tags: %v, want ErrNotFound", err)
	}

	var imgs []Digest
	for i := 0; i < 6; i++ {
		raw := []byte(`{"image":` + string(rune('0'+i)) + `}`)
		img := DigestOfBytes(raw)
		if err := s.PutManifest("apps/example", img, raw, ManifestOptions{}); err != nil {
			t.Fatal(err)
		}
		imgs = append(imgs, img)

		sigRaw := []byte(`{"sig":` + string(rune('0'+i)) + `}`)
		sig := DigestOfBytes(sigRaw)
		tag := "sha256-" + img.Hex() + ".sig"
		ref := ReferrerEntry{
			MediaType:    "application/vnd.oci.image.manifest.v1+json",
			Digest:       sig.String(),
			Size:         int64(len(sigRaw)),
			ArtifactType: "application/vnd.dev.cosign.simplesigning.v1+json",
			Annotations:  map[string]string{"org.opencontainers.image.created": "2026-01-05T09:00:00Z"},
		}
		subj := img
		if err := s.PutManifest("apps/example", sig, sigRaw, ManifestOptions{
			Tag:           tag,
			Subject:       &subj,
			Referrer:      &ref,
			SignedSubject: &subj,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Re-signing imgs[2] moves it to the newest slot without exceeding 5.
	reSigRaw := []byte(`{"sig":"resigned"}`)
	reSig := DigestOfBytes(reSigRaw)
	subj2 := imgs[2]
	if err := s.PutManifest("apps/example", reSig, reSigRaw, ManifestOptions{
		Tag:           "sha256-" + subj2.Hex() + ".sig",
		SignedSubject: &subj2,
	}); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}

	gotRetained, err := reopened.ListRetainedDigests("apps/example")
	if err != nil {
		t.Fatal(err)
	}
	wantRetained := []string{
		imgs[1].String(),
		imgs[3].String(),
		imgs[4].String(),
		imgs[5].String(),
		imgs[2].String(),
	}
	if !reflect.DeepEqual(gotRetained, wantRetained) {
		t.Errorf("retained = %v, want %v", gotRetained, wantRetained)
	}

	resolvedDigest, resolvedBytes, err := reopened.ResolveTag("apps/example", "sha256-"+imgs[2].Hex()+".sig")
	if err != nil || resolvedDigest != reSig || string(resolvedBytes) != string(reSigRaw) {
		t.Errorf("ResolveTag = (%v, %q, %v), want (%v, %q, nil)", resolvedDigest, resolvedBytes, err, reSig, reSigRaw)
	}

	refs, err := reopened.ListReferrers("apps/example", imgs[1], "application/vnd.dev.cosign.simplesigning.v1+json")
	if err != nil || len(refs) != 1 {
		t.Fatalf("ListReferrers = %v, %v", refs, err)
	}
	none, err := reopened.ListReferrers("apps/example", imgs[1], "application/vnd.example.other")
	if err != nil || len(none) != 0 {
		t.Errorf("filtered ListReferrers = %v, %v", none, err)
	}
}

func TestCollectGarbageRetainsLastFiveSignedAndSparesGraceWindow(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)

	// Shared base layer used by every build.
	sharedBlob := storeBlob(t, s, "00000000000000000000000000000001", "shared-base-layer")
	os.Chtimes(s.blobPath(sharedBlob), old, old)

	// Push and sign 6 single-arch images in apps/worker; image 0 ages out.
	var imgDigests []Digest
	var layerDigests []Digest
	var sigDigests []Digest
	for i := 0; i < 6; i++ {
		id := strings.Repeat("0", 30) + "1" + string(rune('0'+i))
		layer := storeBlob(t, s, id, "unique-layer-"+string(rune('0'+i)))
		os.Chtimes(s.blobPath(layer), old, old)
		layerDigests = append(layerDigests, layer)

		imgRaw := []byte(`{"schemaVersion":2,"config":{"digest":"` + sharedBlob.String() + `"},"layers":[{"digest":"` + layer.String() + `"}]}`)
		imgD := DigestOfBytes(imgRaw)
		if err := s.PutManifest("apps/worker", imgD, imgRaw, ManifestOptions{}); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(s.manifestPath("apps/worker", imgD), old, old)
		imgDigests = append(imgDigests, imgD)

		sigBlobID := strings.Repeat("0", 30) + "2" + string(rune('0'+i))
		sigBlob := storeBlob(t, s, sigBlobID, "sig-payload-"+string(rune('0'+i)))
		os.Chtimes(s.blobPath(sigBlob), old, old)

		sigRaw := []byte(`{"schemaVersion":2,"config":{"digest":"` + sharedBlob.String() + `"},"layers":[{"digest":"` + sigBlob.String() + `"}]}`)
		sigD := DigestOfBytes(sigRaw)
		subj := imgD
		if err := s.PutManifest("apps/worker", sigD, sigRaw, ManifestOptions{
			Tag:           "sha256-" + imgD.Hex() + ".sig",
			SignedSubject: &subj,
		}); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(s.manifestPath("apps/worker", sigD), old, old)
		sigDigests = append(sigDigests, sigD)
	}

	// An unsigned stale image in apps/abandoned, plus an orphan blob and an abandoned upload.
	orphanBlob := storeBlob(t, s, "00000000000000000000000000000091", "orphan-blob")
	os.Chtimes(s.blobPath(orphanBlob), old, old)
	staleRaw := []byte(`{"schemaVersion":2,"config":{"digest":"` + orphanBlob.String() + `"},"layers":[]}`)
	staleD := DigestOfBytes(staleRaw)
	if err := s.PutManifest("apps/abandoned", staleD, staleRaw, ManifestOptions{}); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(s.manifestPath("apps/abandoned", staleD), old, old)

	abandonedUpload := "00000000000000000000000000000092"
	if err := s.NewUpload(abandonedUpload); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(s.uploadPath(abandonedUpload), old, old)

	// An in-flight unsigned build (recent modTime within 1h grace) must be spared.
	freshBlob := storeBlob(t, s, "00000000000000000000000000000093", "fresh-inflight-blob")
	os.Chtimes(s.blobPath(freshBlob), now.Add(-5*time.Minute), now.Add(-5*time.Minute))
	freshRaw := []byte(`{"schemaVersion":2,"config":{"digest":"` + freshBlob.String() + `"},"layers":[]}`)
	freshD := DigestOfBytes(freshRaw)
	if err := s.PutManifest("apps/inflight", freshD, freshRaw, ManifestOptions{}); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(s.manifestPath("apps/inflight", freshD), now.Add(-5*time.Minute), now.Add(-5*time.Minute))

	stats, err := s.CollectGarbage(now, time.Hour)
	if err != nil {
		t.Fatalf("CollectGarbage: %v", err)
	}
	if stats.ManifestsRemoved != 3 { // imgDigests[0], sigDigests[0], staleD
		t.Errorf("ManifestsRemoved = %d, want 3", stats.ManifestsRemoved)
	}
	if stats.BlobsRemoved != 3 { // layerDigests[0], sigBlob[0], orphanBlob
		t.Errorf("BlobsRemoved = %d, want 3", stats.BlobsRemoved)
	}
	if stats.UploadsRemoved != 1 {
		t.Errorf("UploadsRemoved = %d, want 1", stats.UploadsRemoved)
	}

	// Aged-out image 0 and its .sig tag and unique layer are gone.
	if _, err := s.GetManifest("apps/worker", imgDigests[0]); err != ErrNotFound {
		t.Errorf("aged-out manifest still present: %v", err)
	}
	if _, _, err := s.ResolveTag("apps/worker", "sha256-"+imgDigests[0].Hex()+".sig"); err != ErrNotFound {
		t.Errorf("aged-out .sig tag still present: %v", err)
	}
	if _, err := s.BlobSize(layerDigests[0]); err != ErrNotFound {
		t.Errorf("aged-out layer blob still present: %v", err)
	}

	// Retained images 1..5, their signatures and the shared base blob remain.
	for i := 1; i < 6; i++ {
		if _, err := s.GetManifest("apps/worker", imgDigests[i]); err != nil {
			t.Errorf("retained manifest %d missing: %v", i, err)
		}
		if _, _, err := s.ResolveTag("apps/worker", "sha256-"+imgDigests[i].Hex()+".sig"); err != nil {
			t.Errorf("retained .sig tag %d missing: %v", i, err)
		}
		if _, err := s.BlobSize(layerDigests[i]); err != nil {
			t.Errorf("retained layer %d missing: %v", i, err)
		}
	}
	if _, err := s.BlobSize(sharedBlob); err != nil {
		t.Errorf("shared base blob missing: %v", err)
	}

	// Stale unsigned repository was removed cleanly, while in-flight unsigned push was spared.
	if _, err := s.ListRetainedDigests("apps/abandoned"); err != ErrNotFound {
		t.Errorf("abandoned repository still exists: %v", err)
	}
	if _, err := s.GetManifest("apps/inflight", freshD); err != nil {
		t.Errorf("in-flight manifest removed inside grace window: %v", err)
	}
	if _, err := s.BlobSize(freshBlob); err != nil {
		t.Errorf("in-flight blob removed inside grace window: %v", err)
	}
}
