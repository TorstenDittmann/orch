package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/config"
)

type fakeUploader struct {
	registry  map[string]bool
	uploaded  map[string]string
	uploads   []string
	lookups   []string
	nodes     []string
	lookupErr error
}

func (f *fakeUploader) LookupImage(_ context.Context, image, localID string) (*orchv1alpha1.LookupImageResponse, error) {
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	f.lookups = append(f.lookups, image)
	if image != "" && f.registry[image] {
		return &orchv1alpha1.LookupImageResponse{InRegistry: true}, nil
	}
	return &orchv1alpha1.LookupImageResponse{Digest: f.uploaded[localID]}, nil
}

func (f *fakeUploader) UploadImage(_ context.Context, image, localID string, archive io.Reader) (*orchv1alpha1.UploadImageResponse, error) {
	data, err := io.ReadAll(archive)
	if err != nil {
		return nil, err
	}
	if string(data) != "archive of "+image {
		return nil, errors.New("unexpected archive")
	}
	f.uploads = append(f.uploads, image)
	digest := "sha256:" + strings.Repeat(localID[len(localID)-1:], 64)
	return &orchv1alpha1.UploadImageResponse{Digest: digest}, nil
}

func (f *fakeUploader) ListNodes(context.Context) ([]*orchv1alpha1.Node, error) {
	var nodes []*orchv1alpha1.Node
	for _, architecture := range f.nodes {
		nodes = append(nodes, &orchv1alpha1.Node{Architecture: architecture})
	}
	return nodes, nil
}

type fakeEngine map[string]localImage

func (f fakeEngine) inspect(_ context.Context, image string) (localImage, bool) {
	local, ok := f[image]
	return local, ok
}

func (f fakeEngine) save(_ context.Context, image string) io.ReadCloser {
	return io.NopCloser(strings.NewReader("archive of " + image))
}

func TestUploadLocalImagesPinsOnlyImagesWithoutARegistry(t *testing.T) {
	manifest := &config.Manifest{
		Services: map[string]config.Service{
			"web":    {Image: "myapp:latest"},
			"worker": {Image: "myapp:latest"},
			"cache":  {Image: "redis:7"},
			"api":    {Image: "ghcr.io/acme/api:v1"},
			"proxy":  {Image: "caddy:2"},
		},
		CronJobs: map[string]config.CronJob{"cleanup": {Image: "tools:dev"}},
	}
	engine := fakeEngine{
		"myapp:latest":        {ID: "sha256:" + strings.Repeat("a", 64), OS: "linux", Architecture: "amd64"},
		"redis:7":             {ID: "sha256:" + strings.Repeat("b", 64), OS: "linux", Architecture: "amd64", RepoDigests: []string{"redis@sha256:x"}},
		"ghcr.io/acme/api:v1": {ID: "sha256:" + strings.Repeat("c", 64), OS: "linux", Architecture: "amd64", RepoDigests: []string{"ghcr.io/acme/api@sha256:y"}},
		"tools:dev":           {ID: "sha256:" + strings.Repeat("e", 64), OS: "linux", Architecture: "amd64"},
	}
	remote := &fakeUploader{
		registry: map[string]bool{"redis:7": true},
		uploaded: map[string]string{"sha256:" + strings.Repeat("e", 64): "sha256:" + strings.Repeat("9", 64)},
		nodes:    []string{"amd64"},
	}
	var progress bytes.Buffer
	if err := uploadLocalImages(context.Background(), remote, engine, manifest, &progress); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"web":    "docker.io/library/myapp@sha256:" + strings.Repeat("a", 64),
		"worker": "docker.io/library/myapp@sha256:" + strings.Repeat("a", 64),
		"cache":  "redis:7",
		"api":    "ghcr.io/acme/api@sha256:" + strings.Repeat("c", 64),
		"proxy":  "caddy:2",
	}
	for name, image := range want {
		if got := manifest.Services[name].Image; got != image {
			t.Errorf("service %s image = %q, want %q", name, got, image)
		}
	}
	if got := manifest.CronJobs["cleanup"].Image; got != "docker.io/library/tools@sha256:"+strings.Repeat("9", 64) {
		t.Errorf("cron image = %q, want the earlier upload", got)
	}
	if strings.Join(remote.uploads, ",") != "ghcr.io/acme/api:v1,myapp:latest" {
		t.Errorf("uploads = %v", remote.uploads)
	}
	// Images that were never pushed or pulled skip the registry check.
	for _, image := range remote.lookups {
		if image == "myapp:latest" || image == "tools:dev" {
			t.Errorf("checked the registry for never-pushed %s", image)
		}
	}
	if !strings.Contains(progress.String(), "Uploading local image myapp:latest") || !strings.Contains(progress.String(), "Using local image tools:dev, uploaded before") {
		t.Errorf("progress = %q", progress.String())
	}
}

func TestUploadLocalImagesRejectsTheWrongArchitecture(t *testing.T) {
	manifest := &config.Manifest{Services: map[string]config.Service{"web": {Image: "myapp:latest"}}}
	engine := fakeEngine{"myapp:latest": {ID: "sha256:" + strings.Repeat("a", 64), OS: "linux", Architecture: "arm64"}}
	remote := &fakeUploader{nodes: []string{"amd64"}}
	err := uploadLocalImages(context.Background(), remote, engine, manifest, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--platform linux/amd64") || len(remote.uploads) != 0 {
		t.Fatalf("error = %v, uploads = %v", err, remote.uploads)
	}
}

func TestUploadLocalImagesSkipsControllersWithoutUploads(t *testing.T) {
	manifest := &config.Manifest{Services: map[string]config.Service{"web": {Image: "myapp:latest"}}}
	engine := fakeEngine{"myapp:latest": {ID: "sha256:" + strings.Repeat("a", 64)}}
	remote := &fakeUploader{lookupErr: connect.NewError(connect.CodeUnimplemented, errors.New("old controller"))}
	if err := uploadLocalImages(context.Background(), remote, engine, manifest, io.Discard); err != nil {
		t.Fatal(err)
	}
	if manifest.Services["web"].Image != "myapp:latest" {
		t.Fatal("an older controller must get the manifest unchanged")
	}
	if err := uploadLocalImages(context.Background(), remote, nil, manifest, io.Discard); err != nil {
		t.Fatal("no local engine must not fail a deploy")
	}
}
