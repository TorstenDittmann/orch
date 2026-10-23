package images_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/images"
	"github.com/TorstenDittmann/orch/internal/images/imagetest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

var localID = "sha256:" + strings.Repeat("a", 64)

func TestStoreImportsServesAndCollectsUploads(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	store, err := images.OpenStore(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if digest, err := store.Lookup(ctx, localID); err != nil || digest != "" {
		t.Fatalf("Lookup() before upload = %q, %v", digest, err)
	}
	archive, layer := imagetest.DockerArchive(t, "myapp:latest", "arm64", "hello")
	digest, platforms, err := store.Import(ctx, "myapp", localID, bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(digest, "sha256:") || len(platforms) != 1 || platforms[0] != "linux/arm64" {
		t.Fatalf("Import() = %q %v", digest, platforms)
	}
	if found, err := store.Lookup(ctx, localID); err != nil || found != digest {
		t.Fatalf("Lookup() = %q, %v; want %q", found, err, digest)
	}

	server := httptest.NewServer(store.RegistryHandler())
	defer server.Close()
	get := func(path string) (*http.Response, []byte) {
		t.Helper()
		response, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response, body
	}
	response, body := get("/v2/library/myapp/manifests/" + digest)
	if response.StatusCode != http.StatusOK || response.Header.Get("Docker-Content-Digest") != digest ||
		response.Header.Get("Content-Type") != "application/vnd.docker.distribution.manifest.v2+json" {
		t.Fatalf("manifest response = %d %v", response.StatusCode, response.Header)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil || len(manifest.Layers) != 1 {
		t.Fatalf("manifest = %s, %v", body, err)
	}
	// Any repository name serves the same content.
	response, compressed := get("/v2/other/name/blobs/" + manifest.Layers[0].Digest.String())
	if response.StatusCode != http.StatusOK {
		t.Fatalf("blob status = %d", response.StatusCode)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	if uncompressed, err := io.ReadAll(reader); err != nil || !bytes.Equal(uncompressed, layer) {
		t.Fatal("served layer does not match the uploaded layer")
	}
	if response, _ := get("/v2/library/myapp/manifests/latest"); response.StatusCode != http.StatusNotFound {
		t.Fatalf("tag status = %d, want 404", response.StatusCode)
	}

	// Recent uploads survive collection even when unreferenced; old ones
	// survive only while a deployment references them.
	if err := store.Collect(ctx, nil); err != nil || !store.Has(ctx, digest) {
		t.Fatalf("Collect() removed a fresh upload: %v", err)
	}
	now = now.Add(2 * time.Hour)
	if err := store.Collect(ctx, map[string]bool{digest: true}); err != nil || !store.Has(ctx, digest) {
		t.Fatalf("Collect() removed a referenced upload: %v", err)
	}
	if response, _ := get("/v2/x/blobs/" + manifest.Layers[0].Digest.String()); response.StatusCode != http.StatusOK {
		t.Fatal("Collect() removed a layer of a referenced upload")
	}
	now = now.Add(2 * time.Hour)
	if err := store.Collect(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if store.Has(ctx, digest) {
		t.Fatal("Collect() kept an unreferenced old upload")
	}
	if response, _ := get("/v2/x/blobs/" + manifest.Layers[0].Digest.String()); response.StatusCode != http.StatusNotFound {
		t.Fatal("Collect() kept the layer of a removed upload")
	}
	if found, err := store.Lookup(ctx, localID); err != nil || found != "" {
		t.Fatalf("Lookup() after collection = %q, %v", found, err)
	}
}

func TestStoreRejectsUnusableArchives(t *testing.T) {
	ctx := context.Background()
	store, err := images.OpenStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	archive, _ := imagetest.DockerArchive(t, "myapp:latest", "s390x", "hello")
	if _, _, err := store.Import(ctx, "myapp:latest", localID, bytes.NewReader(archive)); !errors.Is(err, images.ErrPlatform) {
		t.Fatalf("Import(s390x) error = %v, want ErrPlatform", err)
	}
	archive, _ = imagetest.DockerArchive(t, "myapp:latest", "amd64", "hello")
	if _, _, err := store.Import(ctx, "myapp:latest", localID, strings.NewReader("not a tar")); !errors.Is(err, images.ErrArchive) {
		t.Fatalf("Import(garbage) error = %v, want ErrArchive", err)
	}
	if _, _, err := store.Import(ctx, "myapp:latest", "not-an-id", bytes.NewReader(archive)); !errors.Is(err, images.ErrArchive) {
		t.Fatalf("Import(bad id) error = %v, want ErrArchive", err)
	}
}
