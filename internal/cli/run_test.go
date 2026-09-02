package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orch.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
