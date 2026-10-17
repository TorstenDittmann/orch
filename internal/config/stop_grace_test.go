package config

import (
	"errors"
	"strings"
	"testing"
)

func TestLoadStopGracePeriod(t *testing.T) {
	source := strings.Replace(nodePortManifest, `  udp = new Udp { port = 8790; nodePort = 8790 }
`, `  udp = new Udp { port = 8790; nodePort = 8790 }
  stopGracePeriod = "2m"
`, 1)
	manifest := loadManifest(t, source)
	if got := manifest.Services["game-eu"].StopGrace; got != "2m" {
		t.Fatalf("stop grace = %q", got)
	}
	unset := loadManifest(t, nodePortManifest)
	if got := unset.Services["game-eu"].StopGrace; got != "" {
		t.Fatalf("default stop grace = %q, want empty", got)
	}
	canonical, err := CanonicalJSON(unset)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), "stop_grace") {
		t.Fatalf("unset stop grace changed the canonical manifest: %s", canonical)
	}
}

func TestValidateRejectsInvalidStopGracePeriod(t *testing.T) {
	for _, value := range []string{"0s", "500ms", "1500ms", "11m", "soon"} {
		t.Run(value, func(t *testing.T) {
			manifest := loadManifest(t, nodePortManifest)
			service := manifest.Services["game-eu"]
			service.StopGrace = value
			manifest.Services["game-eu"] = service
			var validation *ValidationError
			if !errors.As(Validate(manifest), &validation) || !hasProblem(validation, "services.game-eu.stop_grace_period") {
				t.Fatalf("Validate() = %v", validation)
			}
		})
	}
}
