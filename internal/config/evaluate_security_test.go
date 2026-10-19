package config

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pklschema "github.com/TorstenDittmann/orch/pkl"
)

func TestLoadSupportsDeterministicLocalComposition(t *testing.T) {
	root := t.TempDir()
	base := `amends "https://raw.githubusercontent.com/TorstenDittmann/orch/main/pkl/v1alpha1/Application.pkl"

name = "composed"
services {
  ["web"] {
    image = "example/web:1"
    env { ["MODE"] = "base" }
  }
}
`
	production := `amends "base.pkl"

services {
  ["web"] {
    replicas = 3
    env { ["MODE"] = "production" }
  }
}
`
	writePklFile(t, filepath.Join(root, "base.pkl"), base)
	target := filepath.Join(root, "orch.pkl")
	writePklFile(t, target, production)

	composed, err := Load(context.Background(), target, LoadOptions{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	direct := loadManifest(t, `amends "https://raw.githubusercontent.com/TorstenDittmann/orch/main/pkl/v1alpha1/Application.pkl"

name = "composed"
services {
  ["web"] {
    image = "example/web:1"
    replicas = 3
    env { ["MODE"] = "production" }
  }
}
`)
	composedJSON, err := CanonicalJSON(composed)
	if err != nil {
		t.Fatal(err)
	}
	directJSON, err := CanonicalJSON(direct)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(composedJSON, directJSON) {
		t.Fatalf("composed and direct manifests differ:\n%s\n%s", composedJSON, directJSON)
	}
	if DigestCanonicalJSON(composedJSON) != DigestCanonicalJSON(directJSON) {
		t.Fatal("equivalent composed and direct manifests have different digests")
	}
	changes, err := Diff(direct, composed)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("source-only composition refactor produced changes: %#v", changes)
	}
}

func TestLoadIgnoresPklProjectEvaluatorSettings(t *testing.T) {
	root := t.TempDir()
	writePklFile(t, filepath.Join(root, "PklProject"), "this is deliberately not a valid project")
	entry := filepath.Join(root, "orch.pkl")
	writePklFile(t, entry, validManifest)
	if _, err := Load(context.Background(), entry, LoadOptions{RootDir: root}); err != nil {
		t.Fatalf("Load() consulted PklProject: %v", err)
	}
}

func TestLoadRejectsEntryOutsideRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(parent, "orch.pkl")
	writePklFile(t, entry, validManifest)
	_, err := Load(context.Background(), entry, LoadOptions{RootDir: root})
	if err == nil || !strings.Contains(err.Error(), "outside evaluation root") {
		t.Fatalf("Load() error = %v, want outside-root error", err)
	}
}

func TestLoadRejectsImportsOutsideRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writePklFile(t, filepath.Join(parent, "outside.pkl"), validManifest)
	entry := filepath.Join(root, "orch.pkl")
	writePklFile(t, entry, `amends "../outside.pkl"`)
	_, err := Load(context.Background(), entry, LoadOptions{RootDir: root})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "root") {
		t.Fatalf("Load() error = %v, want root-boundary error", err)
	}
}

func TestLoadRejectsSymlinkImportEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside.pkl")
	writePklFile(t, outside, validManifest)
	if err := os.Symlink(outside, filepath.Join(root, "linked.pkl")); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(root, "orch.pkl")
	writePklFile(t, entry, `amends "linked.pkl"`)
	_, err := Load(context.Background(), entry, LoadOptions{RootDir: root})
	if err == nil {
		t.Fatal("Load() followed an import symlink outside the root")
	}
}

func TestLoadDeniesAllResourcesWithoutRequesting(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	tests := map[string]string{
		"environment": `read("env:HOME").text`,
		"property":    `read("prop:orch_test").text`,
		"file":        `read("file:secret.txt").text`,
		"http":        `read("` + server.URL + `/secret.txt").text`,
		"https":       `read("` + strings.Replace(server.URL, "http://", "https://", 1) + `/secret.txt").text`,
		"package":     `read("package://example.invalid/example@1.0.0#/secret.txt").text`,
	}
	for name, expression := range tests {
		t.Run(name, func(t *testing.T) {
			source := strings.Replace(validManifest, `name = "hello"`, "name = "+expression, 1)
			_, err := loadManifestError(t, source)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "allowlist") {
				t.Fatalf("Load() error = %v, want denied-resource error", err)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("denied resource made %d loopback requests", requests.Load())
	}
}

func TestLoadSupportsHTTPSModules(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path != "/v1alpha1/Application.pkl" {
			http.NotFound(response, request)
			return
		}
		_, _ = response.Write(pklschema.ApplicationSource())
	}))
	defer server.Close()

	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	source := strings.Replace(validManifest, pklschema.ApplicationURL, server.URL+"/v1alpha1/Application.pkl", 1)
	filename := filepath.Join(t.TempDir(), "orch.pkl")
	writePklFile(t, filename, source)
	manifest, err := Load(context.Background(), filename, LoadOptions{caCertificates: certificate})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Name != "hello" {
		t.Fatalf("manifest name = %q, want hello", manifest.Name)
	}
	if requests.Load() != 1 {
		t.Fatalf("HTTPS module made %d requests, want 1", requests.Load())
	}
}

func TestLoadRejectsHTTPAndDependencyModulesWithoutRequesting(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	tests := map[string]string{
		"http":           server.URL + "/remote.pkl",
		"package":        "package://example.invalid/example@1.0.0#/main.pkl",
		"projectpackage": "projectpackage://example.invalid/example@1.0.0#/main.pkl",
		"modulepath":     "modulepath:/main.pkl",
		"repl":           "repl:text",
		"unknown":        "unregistered:/main.pkl",
	}
	for name, module := range tests {
		t.Run(name, func(t *testing.T) {
			source := strings.Replace(validManifest, `name = "hello"`, "import \""+module+"\" as forbidden\n\nname = forbidden.name", 1)
			_, err := loadManifestError(t, source)
			if err == nil {
				t.Fatal("Load() error = nil, want denied-module error")
			}
			message := strings.ToLower(err.Error())
			if !strings.Contains(message, "allowlist") && !strings.Contains(message, "no module loader") {
				t.Fatalf("Load() error = %v, want denied-module error", err)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("denied HTTP import made %d requests", requests.Load())
	}
}

func TestLoadServesPublishedSchemaFromEmbeddedCopy(t *testing.T) {
	// The published URL is rewritten to an in-process mirror; a request that
	// left the machine would need DNS, which the evaluator does not get here.
	if !strings.Contains(validManifest, pklschema.ApplicationURL) {
		t.Fatalf("validManifest does not amend %s", pklschema.ApplicationURL)
	}
	if manifest := loadManifest(t, validManifest); manifest.Name != "hello" {
		t.Fatalf("manifest name = %q, want hello", manifest.Name)
	}
}

func TestLoadAcceptsLegacyOrchSchemeURI(t *testing.T) {
	source := strings.Replace(validManifest, pklschema.ApplicationURL, pklschema.ApplicationURI, 1)
	if manifest := loadManifest(t, source); manifest.Name != "hello" {
		t.Fatalf("manifest name = %q, want hello", manifest.Name)
	}
}

func TestLoadRejectsUnknownPublishedSchemaModule(t *testing.T) {
	source := strings.Replace(validManifest, `name = "hello"`, "import \""+pklschema.SchemaBaseURL+"v1alpha1/Unknown.pkl\" as unknown\n\nname = unknown.name", 1)
	_, err := loadManifestError(t, source)
	if err == nil || !strings.Contains(err.Error(), "Unknown.pkl") {
		t.Fatalf("Load() error = %v, want unknown-schema error", err)
	}
}

func TestLoadRejectsUnknownEmbeddedModule(t *testing.T) {
	source := strings.Replace(validManifest, `name = "hello"`, "import \"orch:/v1alpha1/Unknown.pkl\" as unknown\n\nname = unknown.name", 1)
	_, err := loadManifestError(t, source)
	if err == nil || !strings.Contains(err.Error(), "Unknown.pkl") {
		t.Fatalf("Load() error = %v, want unknown-schema error", err)
	}
}

func TestLoadRequiresPinnedPklVersion(t *testing.T) {
	script := filepath.Join(t.TempDir(), "pkl")
	writeExecutable(t, script, "#!/bin/sh\necho 'Pkl 0.31.0 (test)'\n")
	filename := filepath.Join(t.TempDir(), "orch.pkl")
	writePklFile(t, filename, validManifest)
	_, err := Load(context.Background(), filename, LoadOptions{PklCommand: script})
	if err == nil || !strings.Contains(err.Error(), "0.32.1 is required") || !strings.Contains(err.Error(), "0.31.0") {
		t.Fatalf("Load() error = %v, want version error", err)
	}
}

func TestLoadReportsMissingPkl(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "orch.pkl")
	writePklFile(t, filename, validManifest)
	t.Setenv("PATH", t.TempDir())
	_, err := Load(context.Background(), filename, LoadOptions{})
	if err == nil || !strings.Contains(err.Error(), "Pkl 0.32.1 is required") || !strings.Contains(err.Error(), "mise install") {
		t.Fatalf("Load() error = %v, want actionable missing-Pkl error", err)
	}
}

func TestLoadHonorsEvaluationTimeout(t *testing.T) {
	script := filepath.Join(t.TempDir(), "pkl")
	writeExecutable(t, script, "#!/bin/sh\nexec sleep 5\n")
	filename := filepath.Join(t.TempDir(), "orch.pkl")
	writePklFile(t, filename, validManifest)
	started := time.Now()
	_, err := Load(context.Background(), filename, LoadOptions{PklCommand: script, Timeout: 25 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Load() error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
}

func TestLoadHonorsCallerCancellation(t *testing.T) {
	script := filepath.Join(t.TempDir(), "pkl")
	writeExecutable(t, script, `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo "Pkl 0.32.1"
  exit 0
fi
exec sleep 5
`)
	filename := filepath.Join(t.TempDir(), "orch.pkl")
	writePklFile(t, filename, validManifest)
	for attempt := 0; attempt < 10; attempt++ {
		ctx, cancel := context.WithCancel(context.Background())
		timer := time.AfterFunc(25*time.Millisecond, cancel)
		started := time.Now()
		_, err := Load(ctx, filename, LoadOptions{PklCommand: script, Timeout: 5 * time.Second})
		timer.Stop()
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("attempt %d: Load() error = %v, want context cancellation", attempt, err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("attempt %d: cancellation took %s", attempt, elapsed)
		}
	}
}

func TestLoadClosesEvaluatorProcess(t *testing.T) {
	realPkl, err := exec.LookPath("pkl")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "pkl")
	writeExecutable(t, script, `#!/bin/sh
if [ "$1" = "--version" ]; then
  exec "$ORCH_REAL_PKL" "$@"
fi
"$ORCH_REAL_PKL" "$@"
status=$?
printf 'closed\n' > "$ORCH_PKL_CLOSE_MARKER"
exit "$status"
`)
	t.Setenv("ORCH_REAL_PKL", realPkl)

	for name, source := range map[string]string{
		"success":          validManifest,
		"evaluation error": strings.Replace(validManifest, "replicas = 2", "repilcas = 2", 1),
	} {
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "closed")
			t.Setenv("ORCH_PKL_CLOSE_MARKER", marker)
			filename := filepath.Join(t.TempDir(), "orch.pkl")
			writePklFile(t, filename, source)
			_, loadErr := Load(context.Background(), filename, LoadOptions{PklCommand: script})
			if name == "success" && loadErr != nil {
				t.Fatal(loadErr)
			}
			if name != "success" && loadErr == nil {
				t.Fatal("Load() error = nil")
			}
			closed, readErr := os.ReadFile(marker)
			if readErr != nil || string(closed) != "closed\n" {
				t.Fatalf("evaluator process did not close cleanly: contents=%q err=%v", closed, readErr)
			}
		})
	}
}

func TestLoadRejectsOversizedPrimaryModule(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "orch.pkl")
	writePklFile(t, filename, strings.Repeat("x", MaxManifestBytes+1))
	_, err := Load(context.Background(), filename, LoadOptions{})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Load() error = %v, want size error", err)
	}
}

func TestLoadRejectsOversizedResolvedManifest(t *testing.T) {
	source := `amends "https://raw.githubusercontent.com/TorstenDittmann/orch/main/pkl/v1alpha1/Application.pkl"

local value = "x".repeat(4096)

name = "large"
services {
  ["web"] {
    image = "example/web:1"
    env {
      for (i in IntSeq(1, 256)) {
        ["KEY_\(i)"] = value
      }
    }
  }
}
`
	_, err := loadManifestError(t, source)
	if err == nil || !strings.Contains(err.Error(), "resolved manifest exceeds") {
		t.Fatalf("Load() error = %v, want resolved-size error", err)
	}
}

func writePklFile(t *testing.T, filename, contents string) {
	t.Helper()
	if err := os.WriteFile(filename, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, filename, contents string) {
	t.Helper()
	if err := os.WriteFile(filename, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}
