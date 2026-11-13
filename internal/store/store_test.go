package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

const id1 = "00000000000000000000000000000001"

func digestOf(s string) Digest {
	sum := sha256.Sum256([]byte(s))
	return Digest{hex: hex.EncodeToString(sum[:])}
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

	if err := s.PutManifest("apps/example", digestOf("something else"), data); err != ErrDigestMismatch {
		t.Fatalf("mismatched manifest: %v", err)
	}
	if err := s.PutManifest("apps/example", d, data); err != nil {
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
