package host

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"net/url"
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
			a := Artifact{Name: "test", SHA256: digest(data.Bytes()), Archive: "tar.gz", Files: map[string]string{"bin/runtime": "bin/runtime"}, arch: "amd64"}
			if err := extract(a, source, filepath.Join(dir, "output")); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}
func TestPinnedBundleIsComplete(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			list, err := Artifacts(arch)
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 4 {
				t.Fatalf("bundle entries: %d", len(list))
			}
			paths := map[string]bool{}
			for _, a := range list {
				if len(a.SHA256) != 64 || a.Version == "" || a.License == "" || !strings.HasPrefix(a.URL, "https://github.com/") || !strings.Contains(a.URL, arch) || a.arch != arch {
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
		})
	}
	if list, err := Artifacts("riscv64"); err != nil || len(list) != 0 {
		t.Fatalf("unsupported architecture bundle = %v, %v", list, err)
	}
}

func TestOfficialArtifacts(t *testing.T) {
	dir := os.Getenv("ORCH_ARTIFACT_DIR")
	if dir == "" {
		t.Skip("set ORCH_ARTIFACT_DIR to downloaded official assets")
	}
	for _, arch := range []string{"amd64", "arm64"} {
		list, err := Artifacts(arch)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range list {
			t.Run(a.Name, func(t *testing.T) {
				source := filepath.Join(dir, a.Name)
				if _, err := os.Stat(source); os.IsNotExist(err) {
					t.Skip("not downloaded")
				}
				if err := extract(a, source, t.TempDir()); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestExtractionChecksArchitecture(t *testing.T) {
	executable := func(machine uint16) []byte {
		data := make([]byte, 64)
		copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
		binary.LittleEndian.PutUint16(data[16:], 2)
		binary.LittleEndian.PutUint16(data[18:], machine)
		binary.LittleEndian.PutUint32(data[20:], 1)
		binary.LittleEndian.PutUint16(data[52:], 64)
		return data
	}
	for _, tc := range []struct {
		name, bundle string
		machine      uint16
		want         string
	}{
		{"arm64 in amd64 bundle", "amd64", 183, "not an amd64 executable"},
		{"amd64 in arm64 bundle", "arm64", 62, "not an arm64 executable"},
		{"unknown bundle", "", 62, "no executable check"},
		{"amd64", "amd64", 62, ""},
		{"arm64", "arm64", 183, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := executable(tc.machine)
			dir := t.TempDir()
			source := filepath.Join(dir, "runtime")
			if err := os.WriteFile(source, data, 0600); err != nil {
				t.Fatal(err)
			}
			a := Artifact{Archive: "raw", SHA256: digest(data), Files: map[string]string{"runtime": "bin/runtime"}, arch: tc.bundle}
			err := extract(a, source, filepath.Join(dir, "output"))
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("extract error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestArtifactRedirectBounds(t *testing.T) {
	for _, tc := range []struct {
		url       string
		redirects int
		allowed   bool
	}{
		{"https://release-assets.githubusercontent.com/asset", 5, true},
		{"http://release-assets.githubusercontent.com/asset", 1, false},
		{"https://user:password@example.com/asset", 1, false},
		{"https://release-assets.githubusercontent.com/asset", 6, false},
	} {
		u, err := url.Parse(tc.url)
		if err != nil {
			t.Fatal(err)
		}
		err = artifactRedirect(&http.Request{URL: u}, make([]*http.Request, tc.redirects))
		if (err == nil) != tc.allowed {
			t.Fatalf("redirect policy: count=%d allowed=%t error=%v", tc.redirects, tc.allowed, err)
		}
	}
}
