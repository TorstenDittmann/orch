package config

import (
	"errors"
	"os"
	"strings"
	"testing"
)

const validManifest = `schema = "v1alpha1"
name = "hello"

[services.web]
image = "ghcr.io/acme/hello:1.2.3"
replicas = 2

[services.web.resources]
cpu = "1"
memory = "256Mi"

[services.web.http]
port = 8080
domains = ["api.example.com", "www.example.com"]
health_path = "/healthz"

[services.web.env]
APP_ENV = "production"
`

func TestParseAppliesDefaultsAndNormalizes(t *testing.T) {
	manifest, err := Parse([]byte(validManifest))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	web := manifest.Services["web"]
	if web.Replicas != 2 {
		t.Fatalf("replicas = %d, want 2", web.Replicas)
	}
	if web.Restart != DefaultRestart {
		t.Fatalf("restart = %q, want %q", web.Restart, DefaultRestart)
	}
	if web.Resources.CPU != "1000m" {
		t.Fatalf("cpu = %q, want 1000m", web.Resources.CPU)
	}
	if manifest.Deploy.HealthDeadline != DefaultHealthDeadline || !manifest.Deploy.AutoRollback {
		t.Fatalf("unexpected deployment defaults: %#v", manifest.Deploy)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	input := strings.Replace(validManifest, "replicas = 2", "repilcas = 2", 1)
	_, err := Parse([]byte(input))
	if err == nil {
		t.Fatal("Parse() error = nil, want strict decoding error")
	}
	if !strings.Contains(err.Error(), "repilcas") {
		t.Fatalf("error %q does not identify unknown field", err)
	}
}

func TestValidationReportsActionablePaths(t *testing.T) {
	tests := []struct {
		name string
		body string
		path string
	}{
		{
			name: "missing route health",
			body: strings.Replace(validManifest, "health_path = \"/healthz\"\n", "", 1),
			path: "services.web.http.health_path",
		},
		{
			name: "invalid cpu",
			body: strings.Replace(validManifest, "cpu = \"1\"\n", "cpu = \"0.5\"\n", 1),
			path: "services.web.resources.cpu",
		},
		{
			name: "overflowing memory",
			body: strings.Replace(validManifest, "memory = \"256Mi\"\n", "memory = \"9223372036854775808Ki\"\n", 1),
			path: "services.web.resources.memory",
		},
		{
			name: "invalid image reference",
			body: strings.Replace(validManifest, "ghcr.io/acme/hello:1.2.3", "bad image reference", 1),
			path: "services.web.image",
		},
		{
			name: "duplicate plain and secret env",
			body: validManifest + "\n[services.web.secret_env]\nAPP_ENV = \"app-env\"\n",
			path: "services.web.secret_env.APP_ENV",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(test.body))
			if err == nil {
				t.Fatal("Parse() error = nil, want validation error")
			}
			var validation *ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("error type = %T, want *ValidationError: %v", err, err)
			}
			if !hasProblem(validation, test.path) {
				t.Fatalf("problems = %#v, want path %q", validation.Problems, test.path)
			}
		})
	}
}

func TestNumericDomainLabelIsAccepted(t *testing.T) {
	input := strings.Replace(validManifest, "api.example.com", "3.example.com", 1)
	if _, err := Parse([]byte(input)); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
}

func TestExamplesValidate(t *testing.T) {
	for _, path := range []string{"../../examples/hello/orch.toml", "../../examples/runtime/orch.toml", "../../examples/stateful/orch.toml"} {
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(data); err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
		})
	}
}

func TestDuplicateDomainAcrossServicesIsRejected(t *testing.T) {
	input := `schema = "v1alpha1"
name = "duplicate-route"

[services.api]
image = "example/api:1"
[services.api.http]
port = 8080
domains = ["shared.example.com"]
health_path = "/healthz"

[services.web]
image = "example/web:1"
[services.web.http]
port = 8080
domains = ["shared.example.com"]
health_path = "/healthz"
`
	_, err := Parse([]byte(input))
	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("Parse() error = %v, want *ValidationError", err)
	}
	if !hasProblem(validation, "services.web.http.domains[0]") {
		t.Fatalf("problems = %#v, want duplicate route problem", validation.Problems)
	}
}

func TestValidateNilManifest(t *testing.T) {
	var validation *ValidationError
	if !errors.As(Validate(nil), &validation) {
		t.Fatal("Validate(nil) did not return *ValidationError")
	}
	if !hasProblem(validation, "manifest") {
		t.Fatalf("problems = %#v, want manifest problem", validation.Problems)
	}
}

func TestLocalVolumeRequiresSingleReplica(t *testing.T) {
	input := `schema = "v1alpha1"
name = "database"

[services.db]
image = "postgres:17"
replicas = 2

[[services.db.mounts]]
source = "data"
target = "/var/lib/postgresql/data"

[volumes.data]
driver = "local"
`
	_, err := Parse([]byte(input))
	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("Parse() error = %v, want *ValidationError", err)
	}
	if !hasProblem(validation, "services.db.replicas") {
		t.Fatalf("problems = %#v, want local-volume replica problem", validation.Problems)
	}
}

func TestUnsafeMountAndSecretTargetsAreRejected(t *testing.T) {
	input := `schema = "v1alpha1"
name = "unsafe"

[services.web]
image = "example/web:1"

[[services.web.mounts]]
source = "data"
target = "/proc"

[[services.web.secret_files]]
source = "api-key"
target = "/tmp/key"

[volumes.data]
`
	_, err := Parse([]byte(input))
	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("Parse() error = %v, want *ValidationError", err)
	}
	for _, want := range []string{"services.web.mounts[0].target", "services.web.secret_files[0].target"} {
		if !hasProblem(validation, want) {
			t.Errorf("problems = %#v, want path %q", validation.Problems, want)
		}
	}
}

func hasProblem(validation *ValidationError, path string) bool {
	for _, problem := range validation.Problems {
		if problem.Path == path {
			return true
		}
	}
	return false
}
