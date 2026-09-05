package host

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func digest(data []byte) string { s := sha256.Sum256(data); return hex.EncodeToString(s[:]) }
func TestVerifiedDownloadRejectsCorruptionWithoutLeavingOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact")
	if err := saveVerified(strings.NewReader("tampered"), path, digest([]byte("expected"))); err == nil {
		t.Fatal("accepted corruption")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("partial artifact left: %v", err)
	}
	if err := saveVerified(strings.NewReader("expected"), path, digest([]byte("expected"))); err != nil {
		t.Fatal(err)
	}
	if err := saveVerified(strings.NewReader("expected"), path, digest([]byte("expected"))); err == nil {
		t.Fatal("overwrote existing artifact")
	}
}
func TestExtractionRejectsUnsafeArchives(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind byte
		body string
	}{
		{"../escape", tar.TypeReg, "x"}, {"/absolute", tar.TypeReg, "x"}, {"link", tar.TypeSymlink, ""}, {"fifo", tar.TypeFifo, ""}, {"unexpected", tar.TypeReg, "x"}, {"bin/runtime", tar.TypeReg, "not an executable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var data bytes.Buffer
			gz := gzip.NewWriter(&data)
			tw := tar.NewWriter(gz)
			if err := tw.WriteHeader(&tar.Header{Name: tc.name, Typeflag: tc.kind, Mode: 0755, Size: int64(len(tc.body)), Linkname: "/etc/shadow"}); err != nil {
				t.Fatal(err)
			}
			tw.Write([]byte(tc.body))
			tw.Close()
			gz.Close()
			dir := t.TempDir()
			source := filepath.Join(dir, "archive")
			os.WriteFile(source, data.Bytes(), 0600)
			a := Artifact{Name: "test", SHA256: digest(data.Bytes()), Archive: "tar.gz", Files: map[string]string{"bin/runtime": "bin/runtime"}}
			if err := extract(a, source, filepath.Join(dir, "output")); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}
func TestPinnedBundleIsComplete(t *testing.T) {
	list, err := Artifacts()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Fatalf("bundle entries: %d", len(list))
	}
	paths := map[string]bool{}
	for _, a := range list {
		if len(a.SHA256) != 64 || a.Version == "" || a.License == "" || !strings.HasPrefix(a.URL, "https://github.com/") {
			t.Fatalf("invalid pin: %+v", a)
		}
		for _, p := range a.Files {
			if paths[p] {
				t.Fatalf("duplicate path %s", p)
			}
			paths[p] = true
		}
	}
	for _, p := range []string{"bin/containerd", "bin/containerd-shim-runc-v2", "bin/runc", "bin/caddy", "cni/bridge", "cni/host-local", "cni/loopback"} {
		if !paths[p] {
			t.Fatalf("missing %s", p)
		}
	}
}

func TestOfficialArtifacts(t *testing.T) {
	dir := os.Getenv("ORCH_ARTIFACT_DIR")
	if dir == "" {
		t.Skip("set ORCH_ARTIFACT_DIR to downloaded official assets")
	}
	list, err := Artifacts()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list {
		t.Run(a.Name, func(t *testing.T) {
			if err := extract(a, filepath.Join(dir, a.Name), t.TempDir()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
