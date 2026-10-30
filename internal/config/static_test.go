package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadStaticService(t *testing.T) {
	manifest := loadManifest(t, `amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

name = "docs"

services {
  ["web"] {
    static = new Static { dir = "dist"; spa = true }
    http { domains { "docs.example.com" } }
  }
}
`)
	web := manifest.Services["web"]
	if web.Static == nil || !web.Static.SPA || !filepath.IsAbs(web.Static.Dir) || filepath.Base(web.Static.Dir) != "dist" {
		t.Fatalf("static = %#v", web.Static)
	}
	if web.Image != StaticImage("docs", "web") {
		t.Fatalf("image = %q", web.Image)
	}
	if web.HTTP == nil || web.HTTP.Port != 8080 || web.HTTP.HealthPath != "/.orch/health" || len(web.HTTP.Domains) != 1 {
		t.Fatalf("http = %#v", web.HTTP)
	}
	canonical, err := CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), "dist") {
		t.Fatalf("the static folder leaks into the canonical manifest:\n%s", canonical)
	}
}

func TestStaticServiceRejectsImageAndCommand(t *testing.T) {
	_, err := loadManifestError(t, `amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

name = "docs"

services {
  ["web"] {
    image = "nginx:1"
    command { "nginx" }
    static = new Static { dir = "dist" }
  }
}
`)
	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("Load() error = %v, want validation error", err)
	}
	for _, path := range []string{"services.web.image", "services.web.static"} {
		if !hasProblem(validation, path) {
			t.Errorf("missing problem at %s in %v", path, validation)
		}
	}
}

func TestServiceNeedsImageOrStatic(t *testing.T) {
	_, err := loadManifestError(t, `amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

name = "docs"

services { ["web"] {} }
`)
	var validation *ValidationError
	if !errors.As(err, &validation) || !hasProblem(validation, "services.web.image") {
		t.Fatalf("Load() error = %v, want services.web.image problem", err)
	}
}

func TestStaticDirStaysInsideEvaluationRoot(t *testing.T) {
	for _, dir := range []string{"..", "../outside", "/etc", "dist/../../outside"} {
		_, err := loadManifestError(t, `amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

name = "docs"

services {
  ["web"] {
    static = new Static { dir = "`+dir+`" }
  }
}
`)
		var validation *ValidationError
		if !errors.As(err, &validation) || !hasProblem(validation, "services.web.static.dir") {
			t.Errorf("dir %q: Load() error = %v, want services.web.static.dir problem", dir, err)
		}
	}
}

func TestStaticDirRefusesLinkOutOfRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "dist")); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(root, "orch.pkl")
	if err := os.WriteFile(filename, []byte(`amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

name = "docs"

services {
  ["web"] {
    static = new Static { dir = "dist" }
  }
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(context.Background(), filename, LoadOptions{})
	var validation *ValidationError
	if !errors.As(err, &validation) || !hasProblem(validation, "services.web.static.dir") {
		t.Fatalf("Load() error = %v, want services.web.static.dir problem", err)
	}
}
