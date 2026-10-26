package staticsite

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/images"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func fakeCaddy(t *testing.T) {
	t.Helper()
	previous := caddyBinary
	caddyBinary = func(_ context.Context, architecture string) (io.ReadCloser, int64, error) {
		data := []byte("caddy for " + architecture)
		return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
	}
	t.Cleanup(func() { caddyBinary = previous })
}

func writeSiteFiles(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, contents := range map[string]string{
		"index.html":               "<h1>home</h1>",
		"assets/app.js":            "console.log(1)",
		".env":                     "SECRET=1",
		".git/config":              "[core]",
		".well-known/security.txt": "Contact: mailto:security@example.com",
	} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestBuildImportsAsMultiPlatformImage(t *testing.T) {
	fakeCaddy(t)
	site := config.Static{Dir: writeSiteFiles(t)}
	built, err := Build(context.Background(), site, 8080, []string{"amd64", "arm64"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := images.OpenStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := os.Open(built.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	stored, platforms, err := store.Import(context.Background(), "", config.StaticImage("docs", "web"), built.ID, archive)
	if err != nil {
		t.Fatal(err)
	}
	if stored != built.ID {
		t.Fatalf("stored digest = %s, want %s", stored, built.ID)
	}
	if !reflect.DeepEqual(platforms, []string{"linux/amd64", "linux/arm64"}) {
		t.Fatalf("platforms = %v", platforms)
	}
}

func TestBuildIsReproducible(t *testing.T) {
	fakeCaddy(t)
	site := config.Static{Dir: writeSiteFiles(t)}
	first, err := Build(context.Background(), site, 8080, []string{"amd64"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(context.Background(), site, 8080, []string{"amd64"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("unchanged site built as %s and %s", first.ID, second.ID)
	}
	site.SPA = true
	spa, err := Build(context.Background(), site, 8080, []string{"amd64"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if spa.ID == first.ID {
		t.Fatal("changing spa kept the same image")
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(site.Dir, "index.html"), later, later); err != nil {
		t.Fatal(err)
	}
	touched, err := Build(context.Background(), site, 8080, []string{"amd64"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if touched.ID == spa.ID {
		t.Fatal("a changed file kept the same image")
	}
}

func TestBuildPublishesOnlyVisibleFiles(t *testing.T) {
	fakeCaddy(t)
	site := config.Static{Dir: writeSiteFiles(t)}
	built, err := Build(context.Background(), site, 9000, []string{"arm64"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := imageFiles(t, built.Path)
	for _, name := range []string{"usr/bin/caddy", "etc/caddy/Caddyfile", "srv/index.html", "srv/assets/app.js", "srv/.well-known/security.txt", "tmp/"} {
		if _, ok := files[name]; !ok {
			t.Errorf("image lacks %s", name)
		}
	}
	for name := range files {
		if strings.Contains(name, ".env") || strings.Contains(name, ".git") {
			t.Errorf("image publishes hidden file %s", name)
		}
	}
	if files["usr/bin/caddy"] != "caddy for arm64" {
		t.Errorf("caddy = %q", files["usr/bin/caddy"])
	}
	if caddyfile := files["etc/caddy/Caddyfile"]; !strings.Contains(caddyfile, ":9000 {") || !strings.Contains(caddyfile, "handle "+HealthPath) {
		t.Errorf("Caddyfile:\n%s", caddyfile)
	}
}

func TestBuildNeedsTheFolder(t *testing.T) {
	fakeCaddy(t)
	_, err := Build(context.Background(), config.Static{Dir: filepath.Join(t.TempDir(), "dist")}, 8080, []string{"amd64"}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "build the site first") {
		t.Fatalf("Build() error = %v", err)
	}
}

func TestCaddyfileModes(t *testing.T) {
	spa := Caddyfile(config.Static{SPA: true}, 8080, true)
	if !strings.Contains(spa, "try_files {path} {path}/ /index.html") || strings.Contains(spa, "404.html") {
		t.Errorf("spa Caddyfile:\n%s", spa)
	}
	plain := Caddyfile(config.Static{}, 8080, true)
	if strings.Contains(plain, "try_files") || !strings.Contains(plain, "rewrite * /404.html") {
		t.Errorf("Caddyfile:\n%s", plain)
	}
	if bare := Caddyfile(config.Static{}, 8080, false); strings.Contains(bare, "404.html") {
		t.Errorf("Caddyfile without a 404 page:\n%s", bare)
	}
}

func TestPinnedCaddyMatchesIngress(t *testing.T) {
	for _, architecture := range Architectures {
		release, err := pinnedCaddy(architecture)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(release.URL, "/v2.11.3/") || len(release.SHA256) != 64 {
			t.Fatalf("%s release = %#v", architecture, release)
		}
	}
}

// imageFiles returns the files of the archive's first image, by path.
func imageFiles(t *testing.T, archive string) map[string]string {
	t.Helper()
	blobs := map[string][]byte{}
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		blobs[header.Name] = data
	}
	blob := func(value digest.Digest) []byte {
		return blobs["blobs/sha256/"+value.Encoded()]
	}
	var root, index ocispec.Index
	var manifest ocispec.Manifest
	if err := json.Unmarshal(blobs["index.json"], &root); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(blob(root.Manifests[0].Digest), &index); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(blob(index.Manifests[0].Digest), &manifest); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, layer := range manifest.Layers {
		zipped, err := gzip.NewReader(bytes.NewReader(blob(layer.Digest)))
		if err != nil {
			t.Fatal(err)
		}
		layerReader := tar.NewReader(zipped)
		for {
			header, err := layerReader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(layerReader)
			files[header.Name] = string(data)
		}
	}
	return files
}
