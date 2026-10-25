package config

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestLoadCapabilitiesNormalizesAndPreservesDefaults(t *testing.T) {
	source := strings.Replace(validManifest, "replicas = 2", `replicas = 2
    capabilities { "CAP_SETUID"; "CAP_CHOWN"; "CAP_SETGID"; "CAP_KILL" }`, 1)
	manifest := loadManifest(t, source)
	want := []string{"CAP_CHOWN", "CAP_KILL", "CAP_SETGID", "CAP_SETUID"}
	if !slices.Equal(manifest.Services["web"].Capabilities, want) {
		t.Fatalf("capabilities = %v", manifest.Services["web"].Capabilities)
	}
	if !slices.Equal(manifest.CapabilityServices(), []string{"web"}) {
		t.Fatalf("capability services = %v", manifest.CapabilityServices())
	}
	canonical, err := CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := ParseCanonicalJSON(canonical)
	if err != nil || !slices.Equal(decoded.Services["web"].Capabilities, want) {
		t.Fatalf("canonical round trip = %#v, %v", decoded, err)
	}
	baseline := loadManifest(t, validManifest)
	unset, err := CanonicalJSON(baseline)
	if err != nil || strings.Contains(string(unset), "capabilities") || len(baseline.CapabilityServices()) != 0 {
		t.Fatalf("unset capabilities changed existing manifests: %s, %v", unset, err)
	}
}

func TestValidateCapabilitiesRejectsInvalidGrants(t *testing.T) {
	for name, capabilities := range map[string][]string{
		"unknown":   {"CAP_NOT_REAL"},
		"oversized": {strings.Repeat("X", 1000)},
		"alias":     {"CHOWN"},
		"all":       {"ALL"},
		"empty":     {""},
		"duplicate": {"CAP_CHOWN", "CAP_CHOWN"},
		"too many":  slices.Repeat([]string{"CAP_CHOWN"}, 42),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCapabilities(capabilities); err == nil {
				t.Fatalf("accepted %v", capabilities)
			}
		})
	}
	if err := ValidateCapabilities(linuxCapabilities); err != nil || len(linuxCapabilities) != 41 {
		t.Fatalf("known Linux capabilities = %d, %v", len(linuxCapabilities), err)
	}
}

func TestCapabilitiesCannotBeCombinedWithPrivileged(t *testing.T) {
	manifest := loadManifest(t, validManifest)
	service := manifest.Services["web"]
	service.Capabilities = []string{"CAP_SETUID"}
	service.Privileged = true
	manifest.Services["web"] = service
	var validation *ValidationError
	if !errors.As(Validate(manifest), &validation) || !hasProblem(validation, "services.web.capabilities") {
		t.Fatalf("Validate = %v", validation)
	}
}
