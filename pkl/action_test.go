package pklschema

import (
	"os"
	"strings"
	"testing"
)

// The GitHub Action only runs a Pkl download whose checksum it knows, so it
// must know the checksums of the version orch requires.
func TestActionKnowsTheRequiredPklChecksums(t *testing.T) {
	action, err := os.ReadFile("../action.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range []string{"pkl-linux-amd64", "pkl-linux-aarch64", "pkl-macos-amd64", "pkl-macos-aarch64"} {
		if !strings.Contains(string(action), RequiredPklVersion+"/"+asset+") pkl_sha256=") {
			t.Errorf("action.yml has no checksum for Pkl %s %s", RequiredPklVersion, asset)
		}
	}
}
