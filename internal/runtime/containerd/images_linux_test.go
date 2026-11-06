//go:build linux

package containerd

import (
	"reflect"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/images"
)

func TestImageRepository(t *testing.T) {
	for name, want := range map[string]string{
		"ghcr.io/acme/game@sha256:abc":         "ghcr.io/acme/game",
		"ghcr.io/acme/game:v1@sha256:abc":      "ghcr.io/acme/game",
		"localhost:5000/acme/game@sha256:abc":  "localhost:5000/acme/game",
		"localhost:5000/acme/game:v2":          "localhost:5000/acme/game",
		"docker.io/library/redis:7@sha256:abc": "docker.io/library/redis",
	} {
		if got := imageRepository(name); got != want {
			t.Fatalf("imageRepository(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestImagesToPruneKeepsUsedAndRecentImages(t *testing.T) {
	base := time.Date(2026, time.November, 6, 12, 0, 0, 0, time.UTC)
	image := func(name string, age int) images.Image {
		return images.Image{Name: name, UpdatedAt: base.Add(-time.Duration(age) * time.Hour)}
	}
	stored := []images.Image{
		image("ghcr.io/acme/game@sha256:1", 5),
		image("ghcr.io/acme/game@sha256:2", 4),
		image("ghcr.io/acme/game@sha256:3", 3),
		image("ghcr.io/acme/game@sha256:4", 2),
		image("ghcr.io/acme/game@sha256:5", 1),
		image("ghcr.io/acme/api@sha256:1", 9),
		image("ghcr.io/acme/api@sha256:2", 8),
	}
	inUse := map[string]bool{"ghcr.io/acme/game@sha256:1": true}

	got := imagesToPrune(stored, inUse, 2)
	want := []string{"ghcr.io/acme/game@sha256:2", "ghcr.io/acme/game@sha256:3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("imagesToPrune(keep 2) = %v, want %v", got, want)
	}

	got = imagesToPrune(stored, inUse, 0)
	want = []string{
		"ghcr.io/acme/api@sha256:1", "ghcr.io/acme/api@sha256:2",
		"ghcr.io/acme/game@sha256:2", "ghcr.io/acme/game@sha256:3",
		"ghcr.io/acme/game@sha256:4", "ghcr.io/acme/game@sha256:5",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("imagesToPrune(keep 0) = %v, want %v", got, want)
	}
}

func TestIsNoSpace(t *testing.T) {
	if !isNoSpace(errString("failed to extract layer /var/lib/orch-containerd/x: write /y: no space left on device")) {
		t.Fatal("isNoSpace did not match an ENOSPC message")
	}
	if isNoSpace(nil) || isNoSpace(errString("not found")) {
		t.Fatal("isNoSpace matched an unrelated error")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
