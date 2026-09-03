package config

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func TestHelloCanonicalGolden(t *testing.T) {
	manifest, err := Load(context.Background(), "../../examples/hello/orch.pkl", LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/hello.canonical.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("canonical contract changed (-want +got):\nwant:\n%s\ngot:\n%s", want, got)
	}
	const legacyDigest = "sha256:e1681709e2d1f82a25ec5ee03bfb384544bb67e937a0bbc78fc0d0575083dbd4"
	if digest := DigestCanonicalJSON(got); digest != legacyDigest {
		t.Fatalf("Pkl migration changed the hello digest: got %s, want %s", digest, legacyDigest)
	}
}

func TestCanonicalJSONIsIndependentOfPklOrdering(t *testing.T) {
	first := loadManifest(t, validManifest)
	secondInput := strings.Replace(
		validManifest,
		"        \"api.example.com\"\n        \"www.example.com\"",
		"        \"www.example.com\"\n        \"api.example.com\"",
		1,
	)
	second := loadManifest(t, secondInput)
	firstJSON, err := CanonicalJSON(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := CanonicalJSON(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("canonical documents differ:\n%s\n%s", firstJSON, secondJSON)
	}
	firstDigest, _ := Digest(first)
	secondDigest, _ := Digest(second)
	if firstDigest != secondDigest {
		t.Fatalf("digests differ: %s != %s", firstDigest, secondDigest)
	}
}

func TestDiffReportsSemanticFieldChanges(t *testing.T) {
	before := loadManifest(t, validManifest)
	afterInput := strings.Replace(validManifest, "replicas = 2", "replicas = 3", 1)
	after := loadManifest(t, afterInput)
	changes, err := Diff(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %#v, want one", changes)
	}
	change := changes[0]
	if change.Kind != ChangeModify || change.Path != "services.web.replicas" {
		t.Fatalf("change = %#v, want replicas modification", change)
	}
}

func TestDiffForNewApplicationIsConcise(t *testing.T) {
	manifest := loadManifest(t, validManifest)
	changes, err := Diff(nil, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 3 {
		t.Fatalf("changes = %#v, want application, deployment, and service additions", changes)
	}
	if changes[0].Path != "application" || changes[1].Path != "deploy" || changes[2].Path != "services.web" {
		t.Fatalf("unexpected change paths: %#v", changes)
	}
}
