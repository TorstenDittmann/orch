package config

import (
	"os"
	"strings"
	"testing"
)

func loadHelloCanonical(t *testing.T) *Manifest {
	t.Helper()
	data, err := os.ReadFile("testdata/hello.canonical.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseCanonicalJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestOverrideImagesByName(t *testing.T) {
	manifest := loadHelloCanonical(t)
	if err := OverrideImages(manifest, []string{"web=ghcr.io/acme/hello:2.0.0"}); err != nil {
		t.Fatal(err)
	}
	if got := manifest.Services["web"].Image; got != "ghcr.io/acme/hello:2.0.0" {
		t.Fatalf("web image = %q", got)
	}
	if got := manifest.Services["worker"].Image; got != "ghcr.io/acme/hello:1.2.3" {
		t.Fatalf("worker image changed to %q", got)
	}
}

func TestOverrideImagesByRepositoryReplacesEveryMatch(t *testing.T) {
	manifest := loadHelloCanonical(t)
	digest := "ghcr.io/acme/hello@sha256:" + strings.Repeat("a", 64)
	if err := OverrideImages(manifest, []string{digest}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"web", "worker"} {
		if got := manifest.Services[name].Image; got != digest {
			t.Fatalf("%s image = %q", name, got)
		}
	}
}

func TestOverrideImagesRejectsMismatches(t *testing.T) {
	for name, overrides := range map[string][]string{
		"unknown name":       {"api=ghcr.io/acme/hello:2"},
		"unknown repository": {"ghcr.io/acme/other:2"},
		"invalid reference":  {"web=Not A Reference"},
		"conflict":           {"web=ghcr.io/acme/hello:2", "ghcr.io/acme/hello:3"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := OverrideImages(loadHelloCanonical(t), overrides); err == nil {
				t.Fatalf("OverrideImages(%q) succeeded", overrides)
			}
		})
	}
}
