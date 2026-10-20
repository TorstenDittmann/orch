package config

import (
	"errors"
	"strings"
	"testing"
)

func autoscaleManifest(block string) string {
	return strings.Replace(validManifest, "    replicas = 2\n", block, 1)
}

func TestLoadAutoscale(t *testing.T) {
	manifest := loadManifest(t, autoscaleManifest("    autoscale = new Autoscale { min = 2; max = 10 }\n"))
	web := manifest.Services["web"]
	if web.Autoscale == nil || *web.Autoscale != (Autoscale{Min: 2, Max: 10, CPUPercent: DefaultAutoscaleCPU}) {
		t.Fatalf("autoscale = %#v", web.Autoscale)
	}
	if web.Replicas != 2 || web.MaxReplicas() != 10 {
		t.Fatalf("replicas = %d, max = %d", web.Replicas, web.MaxReplicas())
	}
	canonical, err := CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCanonicalJSON(canonical); err != nil {
		t.Fatalf("canonical autoscale manifest does not parse: %v", err)
	}
	unset := loadManifest(t, validManifest)
	if canonical, _ := CanonicalJSON(unset); strings.Contains(string(canonical), "autoscale") {
		t.Fatalf("unset autoscale changed the canonical manifest: %s", canonical)
	}
}

func TestLoadAutoscaleRejectsReplicas(t *testing.T) {
	_, err := loadManifestError(t, autoscaleManifest("    replicas = 3\n    autoscale = new Autoscale { min = 2; max = 10 }\n"))
	var validation *ValidationError
	if !errors.As(err, &validation) || !hasProblem(validation, "services.web.replicas") {
		t.Fatalf("Load() = %v", err)
	}
}

func TestValidateAutoscale(t *testing.T) {
	cases := map[string]struct {
		edit func(*Service)
		path string
	}{
		"max below min":  {func(s *Service) { s.Autoscale.Max = 1 }, "services.web.autoscale.max"},
		"max too large":  {func(s *Service) { s.Autoscale.Max = 101 }, "services.web.autoscale.max"},
		"cpu too low":    {func(s *Service) { s.Autoscale.CPUPercent = 5 }, "services.web.autoscale.cpu_percent"},
		"replicas drift": {func(s *Service) { s.Replicas = 4 }, "services.web.replicas"},
		"volume": {func(s *Service) {
			s.Mounts = []Mount{{Source: "data", Target: "/data"}}
		}, "services.web.autoscale"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			manifest := loadManifest(t, autoscaleManifest("    autoscale = new Autoscale { min = 2; max = 10 }\n"))
			manifest.Volumes = map[string]Volume{"data": {Driver: "local"}}
			web := manifest.Services["web"]
			test.edit(&web)
			manifest.Services["web"] = web
			var validation *ValidationError
			if !errors.As(Validate(manifest), &validation) || !hasProblem(validation, test.path) {
				t.Fatalf("Validate() = %v", validation)
			}
		})
	}
}

func TestValidateAutoscaleCountsMaxTowardTotal(t *testing.T) {
	manifest := loadManifest(t, autoscaleManifest("    autoscale = new Autoscale { min = 1; max = 100 }\n"))
	for _, name := range []string{"a", "b"} {
		service := manifest.Services["web"]
		service.HTTP = nil
		manifest.Services[name] = service
	}
	var validation *ValidationError
	if !errors.As(Validate(manifest), &validation) || !hasProblem(validation, "services") {
		t.Fatalf("Validate() = %v", validation)
	}
}
