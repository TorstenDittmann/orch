package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clusterctl "change.codes/torsten/better-swarm/internal/cluster"
	"change.codes/torsten/better-swarm/internal/store"
)

const cliManifest = `schema = "v1alpha1"
name = "hello"

[services.web]
image = "example/web:1"

[services.web.http]
port = 8080
domains = ["hello.example.com"]
health_path = "/healthz"
`

func TestValidate(t *testing.T) {
	path := writeManifest(t, cliManifest)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"validate", "--file", path}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("Run() code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "valid hello (1 services) sha256:") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestValidateJSONError(t *testing.T) {
	path := writeManifest(t, strings.Replace(cliManifest, "health_path = \"/healthz\"\n", "", 1))
	var stdout, stderr bytes.Buffer
	code := Run([]string{"validate", "--file", path, "--json"}, &stdout, &stderr)
	if code != ExitInvalid {
		t.Fatalf("Run() code = %d, want %d", code, ExitInvalid)
	}
	var payload struct {
		Problems []struct {
			Path string `json:"path"`
		} `json:"problems"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &payload); err != nil {
		t.Fatalf("stderr is not JSON: %v\n%s", err, stderr.String())
	}
	if len(payload.Problems) != 1 || payload.Problems[0].Path != "services.web.http.health_path" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestValidateJSONDecodeErrorIncludesUnknownField(t *testing.T) {
	path := writeManifest(t, strings.Replace(cliManifest, "image =", "imag =", 1))
	var stdout, stderr bytes.Buffer
	code := Run([]string{"validate", "--file", path, "--json"}, &stdout, &stderr)
	if code != ExitInvalid {
		t.Fatalf("Run() code = %d, want %d", code, ExitInvalid)
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &payload); err != nil {
		t.Fatalf("stderr is not JSON: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(payload.Error, "imag") || !strings.Contains(payload.Error, "unknown field") {
		t.Fatalf("error = %q, want unknown field context", payload.Error)
	}
}

func TestPlanAgainstManifest(t *testing.T) {
	before := writeManifest(t, cliManifest)
	after := writeManifest(t, strings.Replace(cliManifest, "image = \"example/web:1\"", "image = \"example/web:2\"", 1))
	var stdout, stderr bytes.Buffer
	code := Run([]string{"plan", "--file", after, "--against", before}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("Run() code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "~ services.web.image: \"example/web:1\" -> \"example/web:2\"") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "1 to change") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestPlanJSONUsesEmptyChangeArray(t *testing.T) {
	path := writeManifest(t, cliManifest)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"plan", "--file", path, "--against", path, "--json"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("Run() code = %d, stderr = %s", code, stderr.String())
	}
	var payload struct {
		Changes []json.RawMessage `json:"changes"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.Changes == nil || len(payload.Changes) != 0 {
		t.Fatalf("changes = %#v, want non-nil empty array", payload.Changes)
	}
}

func TestSchema(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"schema"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("Run() code = %d, stderr = %s", code, stderr.String())
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatalf("stdout is not valid JSON: %s", stdout.String())
	}
}

func TestRenderShowsResolvedDefaults(t *testing.T) {
	path := writeManifest(t, cliManifest)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"render", "--file", path}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("Run() code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"cpu": "100m"`) || !strings.Contains(stdout.String(), `"auto_rollback": true`) {
		t.Fatalf("resolved defaults missing from stdout: %s", stdout.String())
	}
}

func TestClusterInitIsIdempotentAndDoesNotPrintSecrets(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dataDir := filepath.Join(root, "state")
	contextPath := filepath.Join(root, "config", "context.json")
	args := []string{
		"cluster", "init", "--name", "test", "--server", "https://controller.example:9443",
		"--data-dir", dataDir, "--context", contextPath,
	}
	var stdout, stderr bytes.Buffer
	if code := Run(args, &stdout, &stderr); code != ExitOK {
		t.Fatalf("Run() code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Initialized cluster test") || !strings.Contains(stdout.String(), contextPath) {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "BEGIN ") || strings.Contains(stderr.String(), "BEGIN ") {
		t.Fatal("cluster init output contains PEM material")
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run(append(args, "--json"), &stdout, &stderr); code != ExitOK {
		t.Fatalf("repeat Run() code = %d, stderr = %s", code, stderr.String())
	}
	var payload struct {
		Created   bool   `json:"created"`
		ClusterID string `json:"cluster_id"`
		DataDir   string `json:"data_dir"`
		Context   string `json:"context"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.Created || payload.ClusterID == "" || payload.DataDir != dataDir || payload.Context != contextPath {
		t.Fatalf("payload = %#v", payload)
	}
	if strings.Contains(stdout.String(), "BEGIN ") {
		t.Fatal("cluster init JSON contains PEM material")
	}
}

func TestClusterBackupReportsExactClusterAndRefusesOverwrite(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dataDir := filepath.Join(root, "state")
	contextPath := filepath.Join(root, "config", "context.json")
	var stdout, stderr bytes.Buffer
	initArgs := []string{
		"cluster", "init", "--name", "test", "--server", "https://controller.example",
		"--data-dir", dataDir, "--context", contextPath, "--json",
	}
	if code := Run(initArgs, &stdout, &stderr); code != ExitOK {
		t.Fatalf("init code = %d, stderr = %s", code, stderr.String())
	}
	var initialized struct {
		ClusterID string `json:"cluster_id"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &initialized); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "backups", "test.db")
	stdout.Reset()
	stderr.Reset()
	backupArgs := []string{"cluster", "backup", "--data-dir", dataDir, "--output", output, "--json"}
	if code := Run(backupArgs, &stdout, &stderr); code != ExitOK {
		t.Fatalf("backup code = %d, stderr = %s", code, stderr.String())
	}
	var backedUp struct {
		ClusterID string `json:"cluster_id"`
		Output    string `json:"output"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &backedUp); err != nil {
		t.Fatal(err)
	}
	if backedUp.ClusterID != initialized.ClusterID || backedUp.Output != output {
		t.Fatalf("backup payload = %#v, initialized = %#v", backedUp, initialized)
	}
	backupStore, err := store.Open(context.Background(), output)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := backupStore.Cluster(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := backupStore.Close(); err != nil {
		t.Fatal(err)
	}
	if stored.ID != initialized.ClusterID {
		t.Fatalf("backup cluster id = %q, want %q", stored.ID, initialized.ClusterID)
	}
	original, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run(backupArgs, &stdout, &stderr); code != ExitRuntime {
		t.Fatalf("no-clobber code = %d, want %d; stderr = %s", code, ExitRuntime, stderr.String())
	}
	after, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("backup destination changed after no-clobber error")
	}
}

func TestClusterCommandsClassifyInvalidInput(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing flags", args: []string{"cluster", "init", "--json"}},
		{name: "invalid name", args: []string{"cluster", "init", "--name", "Not Valid", "--server", "https://controller.example", "--data-dir", filepath.Join(root, "bad-name-state"), "--context", filepath.Join(root, "bad-name-config", "context.json"), "--json"}},
		{name: "non HTTPS", args: []string{"cluster", "init", "--name", "test", "--server", "http://controller.example", "--data-dir", filepath.Join(root, "http-state"), "--context", filepath.Join(root, "http-config", "context.json"), "--json"}},
		{name: "overlap", args: []string{"cluster", "init", "--name", "test", "--server", "https://controller.example", "--data-dir", filepath.Join(root, "overlap"), "--context", filepath.Join(root, "overlap", "context.json"), "--json"}},
		{name: "backup inside data", args: []string{"cluster", "backup", "--data-dir", filepath.Join(root, "state"), "--output", filepath.Join(root, "state", "backup.db"), "--json"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if code := Run(test.args, &stdout, &stderr); code != ExitInvalid {
				t.Fatalf("Run() code = %d, want %d; stderr = %s", code, ExitInvalid, stderr.String())
			}
			if !json.Valid(stderr.Bytes()) {
				t.Fatalf("stderr is not JSON: %s", stderr.String())
			}
		})
	}
}

func TestUnknownClusterSubcommandIsUsageError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"cluster", "launch"}, &stdout, &stderr); code != ExitInvalid {
		t.Fatalf("Run() code = %d, want %d", code, ExitInvalid)
	}
	if !strings.Contains(stderr.String(), "unknown cluster command") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestClusterBackupMissingStateIsOperationalFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dataDir := filepath.Join(root, "state")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"cluster", "backup", "--data-dir", dataDir, "--output", filepath.Join(root, "backup", "test.db")}, &stdout, &stderr)
	if code != ExitRuntime {
		t.Fatalf("Run() code = %d, want %d", code, ExitRuntime)
	}
	if !strings.Contains(stderr.String(), clusterctl.DatabaseFilename) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orch.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
