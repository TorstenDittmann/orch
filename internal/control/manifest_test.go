package control

import (
	"testing"

	"github.com/TorstenDittmann/orch/internal/config"
)

func newTestManifest(name string, services map[string]config.Service) *config.Manifest {
	for serviceName, service := range services {
		if service.Replicas == 0 {
			service.Replicas = 1
		}
		if service.Restart == "" {
			service.Restart = config.DefaultRestart
		}
		if service.Resources.CPU == "" {
			service.Resources.CPU = config.DefaultCPU
		}
		if service.Resources.Memory == "" {
			service.Resources.Memory = config.DefaultMemory
		}
		services[serviceName] = service
	}
	return &config.Manifest{
		Schema:   config.SchemaVersion,
		Name:     name,
		Services: services,
		Deploy: config.Deployment{
			MaxSurge:       1,
			MaxUnavailable: 0,
			HealthDeadline: config.DefaultHealthDeadline,
			AutoRollback:   true,
		},
	}
}

func mustCanonicalConfig(t *testing.T, manifest *config.Manifest) []byte {
	t.Helper()
	if err := config.Validate(manifest); err != nil {
		t.Fatal(err)
	}
	canonical, err := config.CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}
