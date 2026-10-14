package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const nodePortManifest = `amends "orch:/v1alpha1/Application.pkl"

name = "nmv"

local game = new Service {
  image = "example.invalid/game:1"
  tcp = new Tcp { port = 8787; nodePort = 8787 }
  udp = new Udp { port = 8790; nodePort = 8790 }
}

services {
  ["game-eu"] = (game) { constraints { ["region"] = "eu" } }
  ["game-us"] = (game) { constraints { ["region"] = "us" } }
}

deploy { maxSurge = 0; maxUnavailable = 1 }
`

func TestLoadNodeLocalTCPAndUDPPorts(t *testing.T) {
	manifest := loadManifest(t, nodePortManifest)
	game := manifest.Services["game-eu"]
	if game.TCP == nil || game.TCP.NodePort == nil || *game.TCP.NodePort != 8787 || game.TCP.PublicPort != nil {
		t.Fatalf("tcp = %#v", game.TCP)
	}
	if game.UDP == nil || *game.UDP != (UDP{Port: 8790, NodePort: 8790}) {
		t.Fatalf("udp = %#v", game.UDP)
	}
	if got := game.NodePorts(); !reflect.DeepEqual(got, []string{"tcp/8787", "udp/8790"}) {
		t.Fatalf("node ports = %#v", got)
	}
	canonical, err := CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseCanonicalJSON(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.Services["game-us"], manifest.Services["game-us"]) {
		t.Fatalf("canonical round trip = %#v", parsed.Services["game-us"])
	}
}

func TestValidateRejectsUnsafeNodePorts(t *testing.T) {
	cases := map[string]struct {
		edit func(*Manifest)
		path string
	}{
		"surge": {func(m *Manifest) { m.Deploy.MaxSurge = 1 }, "deploy.max_surge"},
		"reserved tcp": {func(m *Manifest) {
			port := 443
			m.Services["game-eu"].TCP.NodePort = &port
		}, "services.game-eu.tcp.node_port"},
		"reserved udp": {func(m *Manifest) { m.Services["game-eu"].UDP.NodePort = 51820 }, "services.game-eu.udp.node_port"},
		"dns udp":      {func(m *Manifest) { m.Services["game-eu"].UDP.NodePort = 53 }, "services.game-eu.udp.node_port"},
		"udp without readiness": {func(m *Manifest) {
			service := m.Services["game-eu"]
			service.TCP = nil
			m.Services["game-eu"] = service
		}, "services.game-eu.udp"},
		"ingress port reused": {func(m *Manifest) {
			port := 8787
			m.Services["game-us"].TCP.NodePort = nil
			m.Services["game-us"].TCP.PublicPort = &port
		}, "services.game-eu.tcp.node_port"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			manifest := loadManifest(t, nodePortManifest)
			test.edit(manifest)
			var validation *ValidationError
			if !errors.As(Validate(manifest), &validation) || !hasProblem(validation, test.path) {
				t.Fatalf("Validate() = %v, want problem at %s", validation, test.path)
			}
		})
	}
}

func TestPklDefaultSurgeRejectsNodePorts(t *testing.T) {
	source := strings.Replace(nodePortManifest, "deploy { maxSurge = 0; maxUnavailable = 1 }\n", "", 1)
	_, err := loadManifestError(t, source)
	if err == nil || !strings.Contains(err.Error(), "max_surge") {
		t.Fatalf("Load() error = %v, want max_surge problem", err)
	}
}
