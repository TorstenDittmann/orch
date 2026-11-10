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
	undo, err := refresh.apply(context.Background(), map[string]string{})
	if err != nil || undo != nil {
		t.Fatalf("apply = %v, %v", undo, err)
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

// A rolled-back upgrade must leave the containerd the previous orch accepts.
func TestRuntimeUndoRestoresThePreviousContainerd(t *testing.T) {
	prefix, stage := t.TempDir(), t.TempDir()
	targets := []string{"bin/containerd-shim-runc-v2", "bin/containerd"}
	files, previous := map[string]string{}, map[string]string{}
	for _, target := range targets {
		for dir, content := range map[string]string{prefix: "new " + target, filepath.Join(stage, "previous"): "old " + target} {
			path := filepath.Join(dir, target)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		path := filepath.Join(prefix, target)
		files[path], previous[path] = "new-digest", "old-digest"
	}
	var calls []string
	undo := &runtimeUndo{prefix: prefix, stage: stage, targets: targets, previous: previous,
		run: func(_ context.Context, name string, args ...string) (string, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			return "", nil
		}}
	if err := undo.revert(context.Background(), files); err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		path := filepath.Join(prefix, target)
		if content, err := os.ReadFile(path); err != nil || string(content) != "old "+target {
			t.Fatalf("%s = %q, %v", target, content, err)
		}
		if files[path] != "old-digest" {
			t.Fatalf("manifest digest for %s = %q", target, files[path])
		}
	}
	if len(calls) != 1 || calls[0] != "systemctl restart orch-containerd.service" {
		t.Fatalf("calls = %v", calls)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("backup was kept after the revert: %v", err)
	}
}
