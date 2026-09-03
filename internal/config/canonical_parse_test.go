package config

import (
	"bytes"
	"testing"
)

func TestParseCanonicalJSONRoundTrip(t *testing.T) {
	manifest := loadManifest(t, validManifest)
	canonical, err := CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseCanonicalJSON(canonical)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := CanonicalJSON(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(roundTrip, canonical) {
		t.Fatalf("round trip differs:\n%s\n%s", canonical, roundTrip)
	}
}

func TestParseCanonicalJSONRejectsNoncanonicalAndUnknownData(t *testing.T) {
	manifest := loadManifest(t, validManifest)
	canonical, err := CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	compact := bytes.ReplaceAll(canonical, []byte("  "), nil)
	unknown := bytes.Replace(canonical, []byte("\"schema\": \"v1alpha1\""), []byte("\"schema\": \"v1alpha1\", \"unknown\": true"), 1)
	trailing := append(append([]byte(nil), canonical...), []byte("{}")...)
	for name, data := range map[string][]byte{"compact": compact, "unknown": unknown, "trailing": trailing} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCanonicalJSON(data); err == nil {
				t.Fatal("ParseCanonicalJSON() error = nil")
			}
		})
	}
}
