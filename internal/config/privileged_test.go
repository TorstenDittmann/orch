package config

import (
	"slices"
	"strings"
	"testing"
)

func TestLoadPrivileged(t *testing.T) {
	source := strings.Replace(nodePortManifest, `  udp = new Udp { port = 8790; nodePort = 8790 }
`, `  udp = new Udp { port = 8790; nodePort = 8790 }
  privileged = true
`, 1)
	manifest := loadManifest(t, source)
	if got := manifest.PrivilegedServices(); !slices.Equal(got, []string{"game-eu", "game-us"}) {
		t.Fatalf("privileged services = %v", got)
	}
	unset := loadManifest(t, nodePortManifest)
	if len(unset.PrivilegedServices()) != 0 {
		t.Fatalf("default privileged services = %v", unset.PrivilegedServices())
	}
	canonical, err := CanonicalJSON(unset)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), "privileged") {
		t.Fatalf("unset privileged changed the canonical manifest: %s", canonical)
	}
}
