package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoverySetManifestUsesStableFieldNames(t *testing.T) {
	data, err := json.Marshal(recoverySetManifest{Version: 1, ClusterID: "cluster", Name: "name", Server: "https://example.com:9443", Schema: 11, CreatedAt: "now"})
	if err != nil {
		t.Fatal(err)
	}
	value := string(data)
	for _, field := range []string{`"cluster_id"`, `"name"`, `"server"`, `"schema_version"`, `"created_at"`} {
		if !strings.Contains(value, field) {
			t.Errorf("manifest missing %s: %s", field, value)
		}
	}
}

func TestNormalizeControllerOptionsRejectsUnsafeIntent(t *testing.T) {
	valid := ControllerOptions{Name: "production", Server: "https://10.0.0.10:9443", Listen: "0.0.0.0:9443", AdminDir: "/private/admin", RecoveryDir: "/private/recovery"}
	tests := []struct {
		name   string
		mutate func(*ControllerOptions)
	}{
		{"name", func(o *ControllerOptions) { o.Name = "Not Valid" }},
		{"http", func(o *ControllerOptions) { o.Server = "http://10.0.0.10:9443" }},
		{"credentials", func(o *ControllerOptions) { o.Server = "https://user@example.com:9443" }},
		{"server path", func(o *ControllerOptions) { o.Server = "https://example.com:9443/api" }},
		{"loopback endpoint", func(o *ControllerOptions) { o.Server = "https://127.0.0.1:9443" }},
		{"privileged listen", func(o *ControllerOptions) { o.Listen = "0.0.0.0:443" }},
		{"listen hostname", func(o *ControllerOptions) { o.Listen = "localhost:9443" }},
		{"relative admin", func(o *ControllerOptions) { o.AdminDir = "admin" }},
		{"same outputs", func(o *ControllerOptions) { o.RecoveryDir = o.AdminDir }},
		{"nested outputs", func(o *ControllerOptions) { o.RecoveryDir = o.AdminDir + "/recovery" }},
		{"state overlap", func(o *ControllerOptions) { o.AdminDir = ControllerDataDir + "/admin" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := valid
			test.mutate(&options)
			if _, err := normalizeControllerOptions(options, true); err == nil {
				t.Fatal("unsafe controller options succeeded")
			}
		})
	}
	if normalized, err := normalizeControllerOptions(valid, true); err != nil || normalized != valid {
		t.Fatalf("valid options = %#v, %v", normalized, err)
	}
}

func TestControllerUnitUsesSeparatePathsAndHardening(t *testing.T) {
	data, err := controllerUnit("0.0.0.0:9443")
	if err != nil {
		t.Fatal(err)
	}
	unit := string(data)
	for _, required := range []string{"User=orch-controller", "ExecStart=/opt/orch-controller/bin/orch controller serve --data-dir /var/lib/orch-controller --listen 0.0.0.0:9443", "NoNewPrivileges=yes", "ProtectSystem=strict", "ReadWritePaths=/var/lib/orch-controller", "UMask=0077"} {
		if !strings.Contains(unit, required) {
			t.Errorf("unit missing %q", required)
		}
	}
	if strings.Contains(unit, Prefix+"/bin/orch") || strings.Contains(unit, AgentDir) {
		t.Fatal("controller unit collides with node installation paths")
	}
	if hostPathsOverlap(ControllerPrefix, Prefix) {
		t.Fatal("controller and node installation prefixes overlap")
	}
}

func TestValidatePrivateTreeRejectsLinksAndLooseModes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "value"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateTree(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "value"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateTree(root); err == nil {
		t.Fatal("world-readable private file accepted")
	}
	if err := os.Chmod(filepath.Join(root, "value"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("value", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateTree(root); err == nil {
		t.Fatal("private-tree symlink accepted")
	}
}

func TestInvalidControllerDoctorDoesNotMutateOutputs(t *testing.T) {
	root := t.TempDir()
	options := ControllerOptions{Name: "test", Server: "http://invalid", Listen: "0.0.0.0:443", AdminDir: filepath.Join(root, "admin"), RecoveryDir: filepath.Join(root, "recovery")}
	if _, err := BuildControllerPlan(context.Background(), options); err == nil {
		t.Fatal("invalid plan succeeded")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("planning mutated filesystem: %v", entries)
	}
}
