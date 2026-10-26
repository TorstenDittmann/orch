package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/staticsite"
)

// siteUploader stores uploads by local ID like the controller does.
type siteUploader struct {
	stored        map[string]string
	uploads       []string
	architectures []string
	unsupported   bool
}

func (s *siteUploader) LookupImage(_ context.Context, image, localID string) (*orchv1alpha1.LookupImageResponse, error) {
	if s.unsupported {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this controller does not store images"))
	}
	if image != "" {
		return nil, errors.New("a static site must not be looked up in a registry")
	}
	return &orchv1alpha1.LookupImageResponse{Digest: s.stored[localID]}, nil
}

func (s *siteUploader) UploadImage(_ context.Context, image, localID string, archive io.Reader) (*orchv1alpha1.UploadImageResponse, error) {
	data, err := io.ReadAll(archive)
	if err != nil {
		return nil, err
	}
	if string(data) != "site "+localID {
		return nil, errors.New("unexpected archive")
	}
	s.uploads = append(s.uploads, image)
	s.stored[localID] = localID
	return &orchv1alpha1.UploadImageResponse{Digest: localID}, nil
}

func (s *siteUploader) ListNodes(context.Context) ([]*orchv1alpha1.Node, error) {
	var nodes []*orchv1alpha1.Node
	for _, architecture := range s.architectures {
		nodes = append(nodes, &orchv1alpha1.Node{Architecture: architecture})
	}
	return nodes, nil
}

func fakeStaticBuild(t *testing.T, built *[]string) {
	t.Helper()
	previous := buildStaticSite
	buildStaticSite = func(_ context.Context, site config.Static, port int, architectures []string, workDir string) (staticsite.Image, error) {
		*built = append(*built, filepath.Base(site.Dir)+":"+strings.Join(architectures, ","))
		id := "sha256:" + strings.Repeat(filepath.Base(site.Dir)[:1], 64)
		path := filepath.Join(workDir, "site.tar")
		if err := os.WriteFile(path, []byte("site "+id), 0o600); err != nil {
			return staticsite.Image{}, err
		}
		return staticsite.Image{ID: id, Path: path, Size: 10}, nil
	}
	t.Cleanup(func() { buildStaticSite = previous })
}

func staticManifest() *config.Manifest {
	return &config.Manifest{Name: "docs", Services: map[string]config.Service{
		"web":  {Image: config.StaticImage("docs", "web"), Static: &config.Static{Dir: "/src/a"}, HTTP: &config.HTTP{Port: 8080, HealthPath: staticsite.HealthPath}},
		"blog": {Image: config.StaticImage("docs", "blog"), Static: &config.Static{Dir: "/src/b"}, HTTP: &config.HTTP{Port: 8080, HealthPath: staticsite.HealthPath}},
		"api":  {Image: "ghcr.io/acme/api:1"},
	}}
}

func TestUploadStaticSitesPinsEachSite(t *testing.T) {
	var built []string
	fakeStaticBuild(t, &built)
	manifest := staticManifest()
	a, b := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	remote := &siteUploader{stored: map[string]string{b: b}, architectures: []string{"arm64"}}
	var progress bytes.Buffer
	if err := uploadStaticSites(context.Background(), remote, manifest, &progress); err != nil {
		t.Fatal(err)
	}
	if got := manifest.Services["web"].Image; got != "orch.local/static/docs/web@"+a {
		t.Errorf("web image = %q", got)
	}
	if got := manifest.Services["blog"].Image; got != "orch.local/static/docs/blog@"+b {
		t.Errorf("blog image = %q", got)
	}
	if got := manifest.Services["api"].Image; got != "ghcr.io/acme/api:1" {
		t.Errorf("api image = %q", got)
	}
	if strings.Join(built, " ") != "b:arm64 a:arm64" {
		t.Errorf("built = %v, want each site for the nodes' architectures", built)
	}
	if strings.Join(remote.uploads, ",") != "orch.local/static/docs/web" {
		t.Errorf("uploads = %v, want only the changed site", remote.uploads)
	}
	if !strings.Contains(progress.String(), "Uploading static site web") || !strings.Contains(progress.String(), "Using static site blog, uploaded before") {
		t.Errorf("progress = %q", progress.String())
	}
	// The pinned image still names the site, so validation accepts it.
	var validation *config.ValidationError
	if errors.As(config.Validate(manifest), &validation) {
		for _, problem := range validation.Problems {
			if strings.HasSuffix(problem.Path, ".image") || strings.HasSuffix(problem.Path, ".static") {
				t.Errorf("pinned site rejected: %s %s", problem.Path, problem.Message)
			}
		}
	}
}

func TestUploadStaticSitesBuildsBothArchitecturesWithoutNodes(t *testing.T) {
	var built []string
	fakeStaticBuild(t, &built)
	manifest := &config.Manifest{Name: "docs", Services: map[string]config.Service{
		"web": {Image: config.StaticImage("docs", "web"), Static: &config.Static{Dir: "/src/a"}, HTTP: &config.HTTP{Port: 8080, HealthPath: staticsite.HealthPath}},
	}}
	if err := uploadStaticSites(context.Background(), &siteUploader{stored: map[string]string{}}, manifest, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.Join(built, " ") != "a:amd64,arm64" {
		t.Errorf("built = %v", built)
	}
}

func TestUploadStaticSitesNeedsUploads(t *testing.T) {
	var built []string
	fakeStaticBuild(t, &built)
	err := uploadStaticSites(context.Background(), &siteUploader{unsupported: true}, staticManifest(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "upgrade it to deploy static sites") {
		t.Fatalf("uploadStaticSites() error = %v", err)
	}
}

func TestUploadStaticSitesChecksACustomHealthPath(t *testing.T) {
	var built []string
	fakeStaticBuild(t, &built)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("home"), 0o644); err != nil {
		t.Fatal(err)
	}
	site := func(path string) *config.Manifest {
		return &config.Manifest{Name: "docs", Services: map[string]config.Service{
			"web": {Image: config.StaticImage("docs", "web"), Static: &config.Static{Dir: dir}, HTTP: &config.HTTP{Port: 8080, HealthPath: path}},
		}}
	}
	remote := &siteUploader{stored: map[string]string{}}
	if err := uploadStaticSites(context.Background(), remote, site("/"), io.Discard); err != nil {
		t.Fatalf("healthPath / with an index.html: %v", err)
	}
	err := uploadStaticSites(context.Background(), remote, site("/healthz"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "healthPath /healthz is not a file") {
		t.Fatalf("uploadStaticSites() error = %v", err)
	}
}
