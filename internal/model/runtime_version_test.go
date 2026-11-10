package model

import "testing"

func TestSupportedRuntimeVersion(t *testing.T) {
	for version, want := range map[string]bool{
		"containerd 2.3.4":    true,
		"containerd 2.3.7":    true,
		"containerd 2.3.12":   true,
		"containerd 2.3.3":    false,
		"containerd 2.4.0":    false,
		"containerd 2.3.7-rc": false,
		"containerd 2.3.":     false,
		"containerd 2.3.07":   false,
		"2.3.7":               false,
		"":                    false,
	} {
		if got := SupportedRuntimeVersion(version); got != want {
			t.Errorf("SupportedRuntimeVersion(%q) = %v, want %v", version, got, want)
		}
	}
	if !SupportedContainerdVersion("v2.3.7") || SupportedContainerdVersion("v2.2.9") {
		t.Fatal("bare containerd versions are not normalized")
	}
}
