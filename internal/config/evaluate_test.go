package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const validManifest = `amends "https://raw.githubusercontent.com/TorstenDittmann/orch/main/pkl/v1alpha1/Application.pkl"

name = "hello"

services {
  ["web"] {
    image = "ghcr.io/acme/hello:1.2.3"
    replicas = 2
    resources {
      cpu = "1"
      memory = "256Mi"
    }
    http = new Http {
      port = 8080
      domains {
        "api.example.com"
        "www.example.com"
      }
      healthPath = "/healthz"
    }
    env {
      ["APP_ENV"] = "production"
    }
  }
}
`

func TestLoadAppliesDefaultsAndNormalizes(t *testing.T) {
	manifest := loadManifest(t, validManifest)
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
	if manifest.Deploy.AutoUpdate {
		t.Fatal("autoUpdate is on by default")
	}
}

func TestLoadAutoUpdate(t *testing.T) {
	manifest := loadManifest(t, validManifest+"\ndeploy {\n  autoUpdate = true\n}\n")
	if !manifest.Deploy.AutoUpdate {
		t.Fatal("autoUpdate was dropped")
	}
	canonical, err := CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCanonicalJSON(canonical); err != nil || !strings.Contains(string(canonical), `"auto_update": true`) {
		t.Fatalf("canonical round trip: %v", err)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	input := strings.Replace(validManifest, "replicas = 2", "repilcas = 2", 1)
	_, err := loadManifestError(t, input)
	if err == nil {
		t.Fatal("Load() error = nil, want typed Pkl error")
	}
	if !strings.Contains(err.Error(), "repilcas") {
		t.Fatalf("error %q does not identify unknown field", err)
	}
}

func TestLoadNodeLocalHTTPIsOptIn(t *testing.T) {
	if loadManifest(t, validManifest).Services["web"].HTTP.NodeLocal {
		t.Fatal("HTTP ingress must remain shared by default")
	}
	input := strings.Replace(validManifest, "      port = 8080", "      port = 8080\n      nodeLocal = true", 1)
	if !loadManifest(t, input).Services["web"].HTTP.NodeLocal {
		t.Fatal("http.nodeLocal was lost during Pkl evaluation")
	}
}

func TestPklRejectsTypeLiteralAndBoundErrors(t *testing.T) {
	tests := map[string]struct {
		input string
		field string
	}{
		"type": {
			input: strings.Replace(validManifest, "replicas = 2", `replicas = "two"`, 1),
			field: "replicas",
		},
		"literal": {
			input: strings.Replace(validManifest, "replicas = 2", "replicas = 2\n    restart = \"sometimes\"", 1),
			field: "restart",
		},
		"bound": {
			input: strings.Replace(validManifest, "replicas = 2", "replicas = 101", 1),
			field: "replicas",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := loadManifestError(t, test.input)
			if err == nil || !strings.Contains(err.Error(), test.field) || !strings.Contains(err.Error(), "orch.pkl") {
				t.Fatalf("Load() error = %v, want source-located %s error", err, test.field)
			}
		})
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
			body: strings.Replace(validManifest, "      healthPath = \"/healthz\"\n", "", 1),
			path: "services.web.http.health_path",
		},
		{
			name: "overflowing memory",
			body: strings.Replace(validManifest, "memory = \"256Mi\"", "memory = \"9223372036854775808Ki\"", 1),
			path: "services.web.resources.memory",
		},
		{
			name: "invalid image reference",
			body: strings.Replace(validManifest, "ghcr.io/acme/hello:1.2.3", "bad image reference", 1),
			path: "services.web.image",
		},
		{
			name: "duplicate plain and secret env",
			body: strings.Replace(validManifest, "    env {\n      [\"APP_ENV\"] = \"production\"\n    }", "    env {\n      [\"APP_ENV\"] = \"production\"\n    }\n    secretEnv {\n      [\"APP_ENV\"] = \"app-env\"\n    }", 1),
			path: "services.web.secret_env.APP_ENV",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadManifestError(t, test.body)
			if err == nil {
				t.Fatal("Load() error = nil, want validation error")
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

func TestPklConstraintReportsSourceLocation(t *testing.T) {
	input := strings.Replace(validManifest, "cpu = \"1\"", "cpu = \"0.5\"", 1)
	_, err := loadManifestError(t, input)
	if err == nil || !strings.Contains(err.Error(), "Type constraint") || !strings.Contains(err.Error(), "resources.cpu") || !strings.Contains(err.Error(), "orch.pkl") {
		t.Fatalf("Load() error = %v, want source-located Pkl constraint failure", err)
	}
}

func TestNumericDomainLabelIsAccepted(t *testing.T) {
	input := strings.Replace(validManifest, "api.example.com", "3.example.com", 1)
	loadManifest(t, input)
}

func TestPublicHTTPIngressIsTypedAndRequiresHealth(t *testing.T) {
	input := strings.Replace(validManifest, "      port = 8080\n", "      port = 8080\n      public = true\n", 1)
	manifest := loadManifest(t, input)
	if !manifest.Services["web"].HTTP.Public {
		t.Fatal("public HTTP ingress was not resolved")
	}

	withoutDomains := strings.Replace(input, `      domains {
        "api.example.com"
        "www.example.com"
      }
`, "", 1)
	withoutHealth := strings.Replace(withoutDomains, "      healthPath = \"/healthz\"\n", "", 1)
	_, err := loadManifestError(t, withoutHealth)
	var validation *ValidationError
	if !errors.As(err, &validation) || !hasProblem(validation, "services.web.http.health_path") {
		t.Fatalf("Load() error = %v, want public ingress health-path problem", err)
	}
}

func TestExamplesValidate(t *testing.T) {
	for _, filename := range []string{"../../examples/hello/orch.pkl", "../../examples/runtime/orch.pkl", "../../examples/stateful/orch.pkl", "../../examples/http/orch.pkl"} {
		t.Run(filename, func(t *testing.T) {
			if _, err := Load(context.Background(), filename, LoadOptions{RootDir: "../.."}); err != nil {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

func TestUnnormalizedPublicDomainsAreRejected(t *testing.T) {
	for _, domain := range []string{"API.example.com", "api.example.com."} {
		t.Run(domain, func(t *testing.T) {
			input := strings.Replace(validManifest, "api.example.com", domain, 1)
			_, err := loadManifestError(t, input)
			var validation *ValidationError
			if !errors.As(err, &validation) || !hasProblem(validation, "services.web.http.domains[0]") {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

func TestInvalidDomainALabelIsRejected(t *testing.T) {
	input := strings.Replace(validManifest, "api.example.com", "xn--a.example.com", 1)
	_, err := loadManifestError(t, input)
	var validation *ValidationError
	if !errors.As(err, &validation) || !hasProblem(validation, "services.web.http.domains[0]") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestManifestCollectionsHaveExplicitBounds(t *testing.T) {
	manifest := loadManifest(t, validManifest)
	service := manifest.Services["web"]
	service.HTTP.Domains = make([]string, maxDomainsPerService+1)
	for index := range service.HTTP.Domains {
		service.HTTP.Domains[index] = "api" + strconv.Itoa(index) + ".example.com"
	}
	manifest.Services["web"] = service
	var validation *ValidationError
	if !errors.As(Validate(manifest), &validation) || !hasProblem(validation, "services.web.http.domains") {
		t.Fatalf("Validate() = %v", validation)
	}
}

func TestDuplicateDomainAcrossServicesIsRejected(t *testing.T) {
	input := `amends "https://raw.githubusercontent.com/TorstenDittmann/orch/main/pkl/v1alpha1/Application.pkl"

name = "duplicate-route"
services {
  ["api"] {
    image = "example/api:1"
    http = new Http {
      port = 8080
      domains { "shared.example.com" }
      healthPath = "/healthz"
    }
  }
  ["web"] {
    image = "example/web:1"
    http = new Http {
      port = 8080
      domains { "shared.example.com" }
      healthPath = "/healthz"
    }
  }
}
`
	_, err := loadManifestError(t, input)
	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("Load() error = %v, want *ValidationError", err)
	}
	if !hasProblem(validation, "services.web.http.domains[0]") {
		t.Fatalf("problems = %#v, want duplicate route problem", validation.Problems)
	}
}

func TestDuplicatePublicHTTPIngressAcrossServicesIsRejected(t *testing.T) {
	manifest := loadManifest(t, validManifest)
	service := manifest.Services["web"]
	service.HTTP.Domains = nil
	service.HTTP.Public = true
	manifest.Services = map[string]Service{"api": service, "web": service}
	var validation *ValidationError
	if !errors.As(Validate(manifest), &validation) || !hasProblem(validation, "services.web.http.public") {
		t.Fatalf("Validate() = %v, want duplicate public ingress problem", validation)
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
	input := `amends "https://raw.githubusercontent.com/TorstenDittmann/orch/main/pkl/v1alpha1/Application.pkl"

name = "database"
services {
  ["db"] {
    image = "postgres:17"
    replicas = 2
    mounts {
      new Mount {
        source = "data"
        target = "/var/lib/postgresql/data"
      }
    }
  }
}
volumes {
  ["data"] {}
}
`
	_, err := loadManifestError(t, input)
	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("Load() error = %v, want *ValidationError", err)
	}
	if !hasProblem(validation, "services.db.replicas") {
		t.Fatalf("problems = %#v, want local-volume replica problem", validation.Problems)
	}
}

func TestUnsafeMountAndSecretTargetsAreRejected(t *testing.T) {
	input := `amends "https://raw.githubusercontent.com/TorstenDittmann/orch/main/pkl/v1alpha1/Application.pkl"

name = "unsafe"
services {
  ["web"] {
    image = "example/web:1"
    mounts {
      new Mount { source = "data"; target = "/proc" }
      new Mount { source = "data"; target = "/etc/resolv.conf" }
    }
    secretFiles {
      new SecretFile { source = "api-key"; target = "/tmp/key" }
    }
  }
}
volumes {
  ["data"] {}
}
`
	_, err := loadManifestError(t, input)
	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("Load() error = %v, want *ValidationError", err)
	}
	for _, want := range []string{"services.web.mounts[0].target", "services.web.mounts[1].target", "services.web.secret_files[0].target"} {
		if !hasProblem(validation, want) {
			t.Errorf("problems = %#v, want path %q", validation.Problems, want)
		}
	}
}

func loadManifest(t *testing.T, source string) *Manifest {
	t.Helper()
	manifest, err := loadManifestError(t, source)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return manifest
}

func loadManifestError(t *testing.T, source string) (*Manifest, error) {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "orch.pkl")
	if err := os.WriteFile(filename, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(context.Background(), filename, LoadOptions{})
}

func hasProblem(validation *ValidationError, path string) bool {
	for _, problem := range validation.Problems {
		if problem.Path == path {
			return true
		}
	}
	return false
}
