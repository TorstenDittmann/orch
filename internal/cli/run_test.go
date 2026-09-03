package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	controlclient "change.codes/torsten/better-swarm/internal/client"
	clusterctl "change.codes/torsten/better-swarm/internal/cluster"
	"change.codes/torsten/better-swarm/internal/control"
	"change.codes/torsten/better-swarm/internal/store"
)

const cliManifest = `amends "orch:/v1alpha1/Application.pkl"

name = "hello"
services {
  ["web"] {
    image = "example/web:1"
    http = new Http {
      port = 8080
      domains { "hello.example.com" }
      healthPath = "/healthz"
    }
  }
}
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

func TestAuthoringCommandsUseExplicitCompositionRoot(t *testing.T) {
	root := t.TempDir()
	applicationDir := filepath.Join(root, "environments", "production")
	if err := os.MkdirAll(applicationDir, 0o700); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, "base.pkl")
	if err := os.WriteFile(base, []byte(cliManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(applicationDir, "orch.pkl")
	if err := os.WriteFile(target, []byte(`amends "../../base.pkl"

services {
  ["web"] { replicas = 2 }
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	commands := map[string][]string{
		"validate": {"validate", "--file", target},
		"render":   {"render", "--file", target},
		"plan":     {"plan", "--file", target, "--against", base},
	}
	for name, args := range commands {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(args, &stdout, &stderr); code != ExitInvalid {
				t.Fatalf("without root = %d, want %d", code, ExitInvalid)
			}
			stdout.Reset()
			stderr.Reset()
			withRoot := append(append([]string(nil), args...), "--root", root)
			if code := Run(withRoot, &stdout, &stderr); code != ExitOK {
				t.Fatalf("with root = %d, stderr = %s", code, stderr.String())
			}
		})
	}
}

func TestValidateJSONError(t *testing.T) {
	path := writeManifest(t, strings.Replace(cliManifest, "      healthPath = \"/healthz\"\n", "", 1))
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
	if !strings.Contains(payload.Error, "imag") || !strings.Contains(payload.Error, "orch.pkl") {
		t.Fatalf("error = %q, want typed Pkl source context", payload.Error)
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

func TestPlanTreatsCompositionRefactorAsNoChange(t *testing.T) {
	root := t.TempDir()
	direct := filepath.Join(root, "direct.pkl")
	base := filepath.Join(root, "base.pkl")
	composed := filepath.Join(root, "composed.pkl")
	for _, path := range []string{direct, base} {
		if err := os.WriteFile(path, []byte(cliManifest), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(composed, []byte(`amends "base.pkl"`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"plan", "--file", composed, "--against", direct, "--root", root}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("Run() code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No changes.") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestSchema(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"schema"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("Run() code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "module orch.Application") || !strings.Contains(stdout.String(), "class Service") {
		t.Fatalf("stdout is not the Pkl application schema: %s", stdout.String())
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

func TestAuthoringCommandsEvaluateEachReferencedManifestOnce(t *testing.T) {
	logPath := installCountingPkl(t)
	before := writeManifest(t, cliManifest)
	after := writeManifest(t, strings.Replace(cliManifest, "example/web:1", "example/web:2", 1))
	tests := []struct {
		name        string
		args        []string
		evaluations int
	}{
		{name: "validate", args: []string{"validate", "--file", before}, evaluations: 1},
		{name: "render", args: []string{"render", "--file", before}, evaluations: 1},
		{name: "plan", args: []string{"plan", "--file", after, "--against", before}, evaluations: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			start := countPklEvaluations(t, logPath)
			var stdout, stderr bytes.Buffer
			if code := Run(test.args, &stdout, &stderr); code != ExitOK {
				t.Fatalf("Run() code = %d, stderr = %s", code, stderr.String())
			}
			if got := countPklEvaluations(t, logPath) - start; got != test.evaluations {
				t.Fatalf("Pkl evaluations = %d, want %d", got, test.evaluations)
			}
		})
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

func TestRemoteStatusDeployAndEvents(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "state")
	contextPath := filepath.Join(root, "config", "context.json")
	initialized, err := clusterctl.Bootstrap(context.Background(), clusterctl.BootstrapOptions{
		Name: "test", Server: "https://127.0.0.1", DataDir: dataDir,
		ContextPath: contextPath, Now: time.Now, Random: rand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := control.Start(context.Background(), control.Options{DataDir: dataDir, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	rewriteCLIContextServer(t, contextPath, "https://"+server.Addr().String())

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"status", "--context", contextPath, "--json"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("status code = %d, stderr = %s", code, stderr.String())
	}
	var status struct {
		ClusterID string `json:"cluster_id"`
		Name      string `json:"name"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.ClusterID != initialized.Cluster.ID || status.Name != "test" {
		t.Fatalf("status = %#v", status)
	}

	applicationDir := filepath.Join(root, "application")
	if err := os.Mkdir(applicationDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "base.pkl"), []byte(cliManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(applicationDir, "orch.pkl")
	if err := os.WriteFile(manifestPath, []byte(`amends "../base.pkl"`), 0o600); err != nil {
		t.Fatal(err)
	}
	evaluationLog := installCountingPkl(t)
	var rendered bytes.Buffer
	if code := Run([]string{"render", "--file", manifestPath, "--root", root}, &rendered, &stderr); code != ExitOK {
		t.Fatalf("render code = %d, stderr = %s", code, stderr.String())
	}
	stderr.Reset()
	beforeDeployEvaluations := countPklEvaluations(t, evaluationLog)
	stdout.Reset()
	deployArgs := []string{
		"deploy", "--file", manifestPath, "--root", root, "--context", contextPath,
		"--idempotency-key", "idempotency-key-one", "--json",
	}
	if code := Run(deployArgs, &stdout, &stderr); code != ExitOK {
		t.Fatalf("deploy code = %d, stderr = %s", code, stderr.String())
	}
	if got := countPklEvaluations(t, evaluationLog) - beforeDeployEvaluations; got != 1 {
		t.Fatalf("deploy Pkl evaluations = %d, want 1", got)
	}
	firstOutput := append([]byte(nil), stdout.Bytes()...)
	var deployment struct {
		Application    string `json:"application"`
		Generation     uint64 `json:"generation"`
		RevisionID     string `json:"revision_id"`
		Changed        bool   `json:"changed"`
		EventSequence  uint64 `json:"event_sequence"`
		Status         string `json:"status"`
		IdempotencyKey string `json:"idempotency_key"`
		Message        string `json:"message"`
	}
	if err := json.Unmarshal(firstOutput, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Application != "hello" || deployment.Generation != 1 || deployment.RevisionID == "" || !deployment.Changed || deployment.EventSequence != 2 || deployment.Status != "PENDING" || deployment.IdempotencyKey != "idempotency-key-one" || !strings.Contains(deployment.Message, "app status") {
		t.Fatalf("deployment = %#v", deployment)
	}
	if bytes.Contains(firstOutput, []byte("BEGIN ")) {
		t.Fatal("deploy output contains credential material")
	}
	applicationClient, err := controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(applicationClient.CloseIdleConnections)
	application, err := applicationClient.GetApplication(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if application.GetCanonicalManifestJson() != rendered.String() {
		t.Fatalf("deploy did not send rendered bytes\nrendered:\n%s\nstored:\n%s", rendered.String(), application.GetCanonicalManifestJson())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run(deployArgs, &stdout, &stderr); code != ExitOK {
		t.Fatalf("replay code = %d, stderr = %s", code, stderr.String())
	}
	if !bytes.Equal(stdout.Bytes(), firstOutput) {
		t.Fatalf("replay output differs\nfirst: %s\nagain: %s", firstOutput, stdout.Bytes())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"events", "--context", contextPath, "--limit", "10", "--json"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("events code = %d, stderr = %s", code, stderr.String())
	}
	var events struct {
		Events []struct {
			Sequence uint64          `json:"sequence"`
			Kind     string          `json:"kind"`
			Payload  json.RawMessage `json:"payload"`
		} `json:"events"`
		Next uint64 `json:"next_after_sequence"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &events); err != nil {
		t.Fatal(err)
	}
	if len(events.Events) != 2 || events.Events[1].Kind != "application.desired.updated" || events.Next != 2 || !json.Valid(events.Events[1].Payload) {
		t.Fatalf("events = %#v", events)
	}
}

func TestRemoteCommandsClassifyLocalAndTransportFailures(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"deploy", "--context", "missing", "--idempotency-key", "short", "--json"}, &stdout, &stderr); code != ExitInvalid {
		t.Fatalf("missing manifest code = %d, want %d", code, ExitInvalid)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"events", "--context", "missing", "--limit", "0", "--json"}, &stdout, &stderr); code != ExitInvalid {
		t.Fatalf("bad events limit code = %d, want %d", code, ExitInvalid)
	}
	if !json.Valid(stderr.Bytes()) {
		t.Fatalf("events error is not JSON: %s", stderr.String())
	}
	largeManifest := filepath.Join(t.TempDir(), "large.pkl")
	if err := os.WriteFile(largeManifest, bytes.Repeat([]byte("x"), (1<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"deploy", "--file", largeManifest, "--context", "missing", "--json"}, &stdout, &stderr); code != ExitInvalid || !strings.Contains(stderr.String(), "exceeds") {
		t.Fatalf("large manifest code = %d, stderr = %s", code, stderr.String())
	}
}

func TestRuntimeCommandsClassifyInvalidInputAndStayInPublicHelp(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{name: "agent init", args: []string{"agent", "init", "--name", "node-1"}},
		{name: "agent init name", args: []string{"agent", "init", "--name", "Node_1", "--context", "missing", "--data-dir", "/tmp/orch-agent-invalid"}},
		{name: "agent serve", args: []string{"agent", "serve", "--data-dir", "/tmp/agent"}},
		{name: "nodes", args: []string{"nodes"}},
		{name: "app status", args: []string{"app", "status", "hello"}},
		{name: "app status name", args: []string{"app", "status", "Not_Valid", "--context", "missing"}},
		{name: "zero log tail", args: []string{"logs", "hello", "worker", "--context", "missing", "--tail", "0"}},
		{name: "log service name", args: []string{"logs", "hello", "Bad_Service", "--context", "missing"}},
		{name: "exec separator", args: []string{"exec", "hello", "worker", "--context", "missing"}},
		{name: "exec timeout", args: []string{"exec", "hello", "worker", "--context", "missing", "--timeout", "301", "--", "/bin/true"}},
		{name: "exec empty argument", args: []string{"exec", "hello", "worker", "--context", "missing", "--", ""}},
		{name: "remove key", args: []string{"remove", "hello", "--context", "missing", "--idempotency-key", "short"}},
		{name: "remove name", args: []string{"remove", "Not_Valid", "--context", "missing"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(test.args, &stdout, &stderr); code != ExitInvalid {
				t.Fatalf("Run(%v) = %d, want %d; stderr = %s", test.args, code, ExitInvalid, stderr.String())
			}
		})
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"help"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("help = %d, %s", code, stderr.String())
	}
	for _, command := range []string{"agent init", "nodes --context", "app status", "logs APP", "exec APP", "remove APP"} {
		if !strings.Contains(stdout.String(), command) {
			t.Errorf("help does not contain %q", command)
		}
	}
	if strings.Contains(stdout.String(), "runtime-log-sink") {
		t.Fatal("private log-sink command was exposed in public help")
	}
}

func TestControllerServeStopsCleanlyOnContextCancellation(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "state")
	if _, err := clusterctl.Bootstrap(context.Background(), clusterctl.BootstrapOptions{
		Name: "test", Server: "https://127.0.0.1", DataDir: dataDir,
		ContextPath: filepath.Join(root, "config", "context.json"), Now: time.Now, Random: rand.Reader,
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	output := newReadyWriter()
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- RunContext(ctx, []string{
			"controller", "serve", "--data-dir", dataDir, "--listen", "127.0.0.1:0",
		}, output, &stderr)
	}()
	select {
	case <-output.ready:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatalf("controller did not become ready; stderr = %s", stderr.String())
	}
	cancel()
	select {
	case code := <-done:
		if code != ExitOK {
			t.Fatalf("controller exit code = %d, stderr = %s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("controller did not stop after cancellation")
	}
	if !strings.Contains(output.String(), "Controller listening on https://127.0.0.1:") {
		t.Fatalf("controller output = %q", output.String())
	}
}

func TestNewIdempotencyKey(t *testing.T) {
	t.Parallel()
	key, err := newIdempotencyKey(bytes.NewReader(make([]byte, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if key != "idem_00000000000000000000000000000000" || !cliIdempotencyKeyPattern.MatchString(key) {
		t.Fatalf("key = %q", key)
	}
	if _, err := newIdempotencyKey(bytes.NewReader(nil)); err == nil {
		t.Fatal("newIdempotencyKey() ignored short randomness")
	}
}

type readyWriter struct {
	sync.Mutex
	buffer bytes.Buffer
	ready  chan struct{}
	once   sync.Once
}

func newReadyWriter() *readyWriter {
	return &readyWriter{ready: make(chan struct{})}
}

func (writer *readyWriter) Write(contents []byte) (int, error) {
	writer.Lock()
	defer writer.Unlock()
	written, err := writer.buffer.Write(contents)
	if strings.Contains(writer.buffer.String(), "Controller listening") {
		writer.once.Do(func() { close(writer.ready) })
	}
	return written, err
}

func (writer *readyWriter) String() string {
	writer.Lock()
	defer writer.Unlock()
	return writer.buffer.String()
}

func rewriteCLIContextServer(t *testing.T, path, endpoint string) {
	t.Helper()
	local, err := clusterctl.LoadContext(path)
	if err != nil {
		t.Fatal(err)
	}
	local.Server = endpoint
	contents, err := json.MarshalIndent(local, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(contents, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orch.pkl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func installCountingPkl(t *testing.T) string {
	t.Helper()
	realPkl, err := exec.LookPath("pkl")
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	wrapper := filepath.Join(directory, "pkl")
	logPath := filepath.Join(directory, "evaluations.log")
	script := `#!/bin/sh
if [ "$1" != "--version" ]; then
  printf 'evaluate\n' >> "$ORCH_PKL_EVALUATION_LOG"
fi
exec "$ORCH_REAL_PKL" "$@"
`
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_REAL_PKL", realPkl)
	t.Setenv("ORCH_PKL_EVALUATION_LOG", logPath)
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func countPklEvaluations(t *testing.T, logPath string) int {
	t.Helper()
	contents, err := os.ReadFile(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(contents, []byte("evaluate\n"))
}
