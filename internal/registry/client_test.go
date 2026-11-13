package registry

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/google/go-containerregistry/pkg/v1/validate"

	"github.com/clearedstack/lazarette/internal/store"
)

// These tests drive the registry with an existing client,
// github.com/google/go-containerregistry (the library behind crane and ko),
// over real HTTP on the loopback interface. Nothing here speaks the registry
// protocol by hand.

func serve(t *testing.T, dir string) string {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&Handler{Store: st})
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// madeUpImage builds an OCI image out of random bytes.
func madeUpImage(t *testing.T) v1.Image {
	t.Helper()
	img, err := random.Image(64*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	return mutate.ConfigMediaType(img, types.OCIConfigJSON)
}

func digestRef(t *testing.T, host, repo string, img v1.Image) name.Digest {
	t.Helper()
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.NewDigest(host+"/"+repo+"@"+d.String(), name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestExistingClientPushAndPullByDigest(t *testing.T) {
	dir := t.TempDir()
	host := serve(t, dir)
	img := madeUpImage(t)
	ref := digestRef(t, host, "apps/example", img)

	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("push by digest: %v", err)
	}

	pulled, err := remote.Image(ref)
	if err != nil {
		t.Fatalf("pull by digest: %v", err)
	}
	// validate.Image downloads the config and every layer and checks each
	// digest and size against the manifest.
	if err := validate.Image(pulled); err != nil {
		t.Fatalf("pulled image does not validate: %v", err)
	}
	wantManifest, _ := img.RawManifest()
	gotManifest, err := pulled.RawManifest()
	if err != nil {
		t.Fatal(err)
	}
	if string(gotManifest) != string(wantManifest) {
		t.Errorf("manifest changed on the way through the registry")
	}
	if mt, _ := pulled.MediaType(); mt != types.OCIManifestSchema1 {
		t.Errorf("pulled media type %q", mt)
	}

	// The image is on disk: one file per layer and config, one manifest.
	blobs, err := os.ReadDir(filepath.Join(dir, "blobs", "sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 4 {
		t.Errorf("%d blobs on disk, want 4 (3 layers and 1 config)", len(blobs))
	}
	manifestFile := filepath.Join(dir, "manifests", "apps", "example", "_sha256", ref.Identifier()[len("sha256:"):])
	if onDisk, err := os.ReadFile(manifestFile); err != nil || string(onDisk) != string(wantManifest) {
		t.Errorf("manifest on disk is missing or differs: %v", err)
	}
	for _, d := range []string{"uploads", "tmp"} {
		left, err := os.ReadDir(filepath.Join(dir, d))
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 0 {
			t.Errorf("%d files left in %s after the push", len(left), d)
		}
	}

	// Pushing the same image again is accepted and changes nothing.
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("second push: %v", err)
	}
}

func TestExistingClientPullAfterRestart(t *testing.T) {
	dir := t.TempDir()
	img := madeUpImage(t)

	first := serve(t, dir)
	if err := remote.Write(digestRef(t, first, "apps/example", img), img); err != nil {
		t.Fatalf("push: %v", err)
	}

	// A second server over the same directory stands in for the process
	// starting again with the same volume.
	second := serve(t, dir)
	pulled, err := remote.Image(digestRef(t, second, "apps/example", img))
	if err != nil {
		t.Fatalf("pull after restart: %v", err)
	}
	if err := validate.Image(pulled); err != nil {
		t.Fatalf("image pulled after restart does not validate: %v", err)
	}
}

func TestExistingClientTagPushIsRefused(t *testing.T) {
	host := serve(t, t.TempDir())
	img := madeUpImage(t)
	tag, err := name.NewTag(host+"/apps/example:latest", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	err = remote.Write(tag, img)
	if err == nil {
		t.Fatal("push by tag was accepted")
	}
	// The client first asks with HEAD whether the tag exists, and stops at
	// the refusal. (The refusal body, with E_NEEDS_SUBTOOL, is pinned by the
	// saved example 09-manifest-tag-refused.)
	var terr *transport.Error
	if !errors.As(err, &terr) || terr.StatusCode != http.StatusBadRequest {
		t.Errorf("want a 400 refusal, got %v", err)
	}
	if _, err := remote.Image(digestRef(t, host, "apps/example", img)); err == nil {
		t.Error("the refused push left a manifest that can be pulled")
	}
}

func TestExistingClientPullOfUnknownDigest(t *testing.T) {
	host := serve(t, t.TempDir())
	_, err := remote.Image(digestRef(t, host, "apps/example", madeUpImage(t)))
	if err == nil || !strings.Contains(err.Error(), "MANIFEST_UNKNOWN") {
		t.Errorf("want MANIFEST_UNKNOWN, got %v", err)
	}
}
