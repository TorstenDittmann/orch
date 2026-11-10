package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstalledContainerdVersion(t *testing.T) {
	for output, want := range map[string]string{
		"containerd github.com/containerd/containerd/v2 v2.3.4 1a2b3c4\n": "2.3.4",
		"containerd containerd.io 2.3.7 deadbeef":                         "2.3.7",
		"garbage": "",
	} {
		if got := installedContainerdVersion(output); got != want {
			t.Errorf("installedContainerdVersion(%q) = %q, want %q", output, got, want)
		}
	}
}

func bundledContainerd(t *testing.T) Artifact {
	t.Helper()
	artifacts, err := Artifacts("amd64")
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range artifacts {
		if strings.HasPrefix(artifact.Name, "containerd-static-") {
			return artifact
		}
	}
	t.Fatal("no containerd in bundle")
	return Artifact{}
}

func TestRuntimeRefreshSkipsCurrentContainerd(t *testing.T) {
	bundled := bundledContainerd(t)
	var calls []string
	refresh := runtimeRefresh{prefix: t.TempDir(), stateDir: t.TempDir(), arch: "amd64",
		run: func(_ context.Context, name string, args ...string) (string, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			return "containerd github.com/containerd/containerd/v2 " + bundled.Version + " abc\n", nil
		},
		fetch: func(context.Context, Artifact, string) error {
			t.Fatal("a current containerd must not be downloaded")
			return nil
		}}
	changed, err := refresh.apply(context.Background(), map[string]string{})
	if err != nil || changed {
		t.Fatalf("apply = %v, %v", changed, err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want only the version check", calls)
	}
}

func TestRuntimeRefreshRefusesChangedFiles(t *testing.T) {
	prefix := t.TempDir()
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, name := range []string{"containerd", "containerd-shim-runc-v2"} {
		path := filepath.Join(prefix, "bin", name)
		if err := os.WriteFile(path, []byte("old "+name), 0o755); err != nil {
			t.Fatal(err)
		}
		files[path] = strings.Repeat("0", 64)
	}
	refresh := runtimeRefresh{prefix: prefix, stateDir: t.TempDir(), arch: "amd64",
		run: func(context.Context, string, ...string) (string, error) {
			return "containerd github.com/containerd/containerd/v2 v2.3.4 abc\n", nil
		},
		fetch: func(context.Context, Artifact, string) error {
			t.Fatal("changed files must be refused before any download")
			return nil
		}}
	if _, err := refresh.apply(context.Background(), files); err == nil || !strings.Contains(err.Error(), "owned content changed") {
		t.Fatalf("apply error = %v", err)
	}
	delete(files, filepath.Join(prefix, "bin", "containerd"))
	shim := filepath.Join(prefix, "bin", "containerd-shim-runc-v2")
	digest, err := hashFile(shim)
	if err != nil {
		t.Fatal(err)
	}
	files[shim] = digest
	if _, err := refresh.apply(context.Background(), files); err == nil || !strings.Contains(err.Error(), "unowned") {
		t.Fatalf("apply error = %v", err)
	}
}
