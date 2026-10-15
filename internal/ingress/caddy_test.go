package ingress

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

func TestNewCaddySeparatesRootStateFromIngressData(t *testing.T) {
	directory := t.TempDir()
	dataDir := filepath.Join(directory, "data")
	stateDir := filepath.Join(directory, "state")
	caddy, err := NewCaddy(CaddyOptions{
		Binary: "/usr/bin/caddy", AdminSocket: filepath.Join(dataDir, "admin.sock"),
		DataDir: dataDir, StateDir: stateDir, User: "orch-ingress",
	})
	if err != nil {
		t.Fatal(err)
	}
	if caddy.options.StateDir != stateDir {
		t.Fatalf("state dir = %q", caddy.options.StateDir)
	}
	info, err := os.Lstat(stateDir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("state directory = %#v, %v", info, err)
	}
	if _, err := NewCaddy(CaddyOptions{
		Binary: "/usr/bin/caddy", AdminSocket: filepath.Join(dataDir, "admin.sock"),
		DataDir: dataDir, StateDir: filepath.Join(dataDir, "agent"), User: "orch-ingress",
	}); err == nil {
		t.Fatal("NewCaddy accepted agent state beneath the Caddy-writable directory")
	}
}

func TestAgentStateFilesAreAtomicPrivateAndBounded(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "state.json")
	if err := publishAgentFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	contents, err := readAgentFile(path, 16)
	if err != nil || string(contents) != "{}\n" {
		t.Fatalf("readAgentFile() = %q, %v", contents, err)
	}
	if _, err := readAgentFile(path, 1); err == nil {
		t.Fatal("readAgentFile accepted an oversized file")
	}
	unsafe := filepath.Join(directory, "unsafe")
	if err := os.Symlink(path, unsafe); err != nil {
		t.Fatal(err)
	}
	if _, err := readAgentFile(unsafe, 16); err == nil {
		t.Fatal("readAgentFile followed a symlink")
	}
}

func TestEmptyRouteApplyFailureIsNotMasked(t *testing.T) {
	statuses, err := applyFailure(nil, time.Now(), "caddy_unavailable", errors.New("offline"))
	if err == nil || len(statuses) != 0 {
		t.Fatalf("applyFailure() = %#v, %v", statuses, err)
	}
}

func TestProbeRoutesReportsEachRouteIndependently(t *testing.T) {
	routes := []Route{
		{Application: "app", Service: "web", Domain: "ok.example.com", HTTPS: true, Upstreams: []string{"10.0.0.2:80"}},
		{Application: "app", Service: "api", Domain: "broken.example.com", Upstreams: []string{"10.0.0.3:80"}},
		{Application: "app", Service: "new", Domain: "issuing.example.com", HTTPS: true, Upstreams: []string{"10.0.0.4:80"}},
		{Application: "app", Service: "err", Domain: "erroring.example.com", HTTPS: true, Upstreams: []string{"10.0.0.5:80"}},
	}
	attempts := map[string]int{}
	caddy := &Caddy{options: CaddyOptions{Probe: func(_ context.Context, route Route) error {
		attempts[route.Domain]++
		switch route.Domain {
		case "broken.example.com":
			return &routeStatusError{domain: route.Domain, status: 502}
		case "erroring.example.com":
			return &routeStatusError{domain: route.Domain, status: 500}
		case "issuing.example.com":
			return errors.New("remote error: tls: internal error")
		}
		return nil
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	statuses := caddy.probeRoutes(ctx, routes, time.Now())
	if len(statuses) != 4 {
		t.Fatalf("statuses = %#v", statuses)
	}
	if statuses[0].State != model.RouteActive || statuses[0].CertificateState != "managed" || statuses[0].Reason != "" {
		t.Fatalf("healthy route = %#v", statuses[0])
	}
	if statuses[1].State != model.RouteFailed || statuses[1].Reason != "route_probe_failed" {
		t.Fatalf("5xx route = %#v", statuses[1])
	}
	if statuses[2].State != model.RoutePending || statuses[2].Reason != "certificate_pending" || statuses[2].CertificateState != "pending" {
		t.Fatalf("issuing route = %#v", statuses[2])
	}
	if statuses[3].State != model.RouteFailed || statuses[3].Reason != "route_probe_failed" {
		t.Fatalf("HTTPS 5xx route = %#v", statuses[3])
	}
	if attempts["ok.example.com"] != 1 || attempts["broken.example.com"] < 2 {
		t.Fatalf("probe attempts = %#v; healthy routes must not be re-probed, failing ones retried", attempts)
	}
}
