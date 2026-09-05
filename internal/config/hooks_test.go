package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTypedPreDeployHookAndCanonicalValidation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "orch.pkl")
	source := `amends "orch:/v1alpha1/Application.pkl"
name = "hello"
services { ["web"] { image = "example/web:latest" } }
deploy { preDeploy = new PreDeployHook { service = "web"; command { "./migrate" } } }
`
	source = strings.ReplaceAll(source, ";", "\n")
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	manifest, err := Load(context.Background(), path, LoadOptions{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Deploy.PreDeploy == nil || manifest.Deploy.PreDeploy.Timeout != "15m" {
		t.Fatal("hook defaults missing")
	}
	canonical, err := CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCanonicalJSON(canonical); err != nil {
		t.Fatal(err)
	}
	manifest.Deploy.PreDeploy.Service = "missing"
	if err := Validate(manifest); err == nil {
		t.Fatal("missing hook service accepted")
	}
}
