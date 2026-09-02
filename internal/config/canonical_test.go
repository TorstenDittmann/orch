package config

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestHelloCanonicalGolden(t *testing.T) {
	data, err := os.ReadFile("../../examples/hello/orch.toml")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := Parse(data)
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
}

func TestCanonicalJSONIsIndependentOfTOMLOrdering(t *testing.T) {
	first, err := Parse([]byte(validManifest))
	if err != nil {
		t.Fatal(err)
	}
	secondInput := strings.Replace(
		validManifest,
		"domains = [\"api.example.com\", \"www.example.com\"]",
		"domains = [\"www.example.com\", \"api.example.com\"]",
		1,
	)
	second, err := Parse([]byte(secondInput))
	if err != nil {
		t.Fatal(err)
	}
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
	before, err := Parse([]byte(validManifest))
	if err != nil {
		t.Fatal(err)
	}
	afterInput := strings.Replace(validManifest, "replicas = 2", "replicas = 3", 1)
	after, err := Parse([]byte(afterInput))
	if err != nil {
		t.Fatal(err)
	}
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
	manifest, err := Parse([]byte(validManifest))
	if err != nil {
		t.Fatal(err)
	}
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
