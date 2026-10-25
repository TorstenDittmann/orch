package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

func TestRemoteNodeCannotClaimNodeLocalHTTPRoute(t *testing.T) {
	t.Parallel()
	state := executionStore(t)
	endpoint := model.ServiceEndpoint{
		AllocationID: "alloc-remote", Application: "remote", Service: "web", NodeID: "node-two",
		Address: "10.88.1.2", Port: 8080, Domains: []string{"remote.example.com"},
		HTTPS: true, HTTPPublic: true, HTTPNodeLocal: true,
	}
	network := model.NetworkConfig{Endpoints: []model.ServiceEndpoint{endpoint}}
	for _, domain := range []string{"remote.example.com", model.PublicHTTPRoute} {
		route := model.RouteStatus{Application: endpoint.Application, Service: endpoint.Service,
			Domain: domain, HTTPS: domain != model.PublicHTTPRoute, State: model.RoutePending, UpdatedAt: time.Now()}
		for _, node := range []string{"node-one", "node-two"} {
			tx, err := state.db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			err = validateRouteObservation(context.Background(), tx, node, network, route)
			_ = tx.Rollback()
			if node == endpoint.NodeID {
				if err != nil {
					t.Fatalf("owner's route rejected: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "not in the acknowledged assignment") {
				t.Fatalf("remote route authorized: %v", err)
			}
		}
	}
}

func TestActiveRouteCertificateStateMatchesDomain(t *testing.T) {
	t.Parallel()
	state := executionStore(t)
	endpoint := model.ServiceEndpoint{
		AllocationID: "alloc-tenants", Application: "site", Service: "tenants", NodeID: "node-one",
		Address: "10.88.1.2", Port: 8080, Domains: []string{"*.example.com", "example.com"}, HTTPS: true,
	}
	network := model.NetworkConfig{Endpoints: []model.ServiceEndpoint{endpoint}}
	for _, test := range []struct {
		domain, certificate string
		valid               bool
	}{
		{"*.example.com", "on_demand", true},
		{"*.example.com", "managed", false},
		{"example.com", "managed", true},
		{"example.com", "on_demand", false},
	} {
		route := model.RouteStatus{Application: endpoint.Application, Service: endpoint.Service, Domain: test.domain,
			HTTPS: true, State: model.RouteActive, CertificateState: test.certificate, UpstreamCount: 1, UpdatedAt: time.Now()}
		tx, err := state.db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		err = validateRouteObservation(context.Background(), tx, endpoint.NodeID, network, route)
		_ = tx.Rollback()
		rejected := err != nil && strings.Contains(err.Error(), "valid certificate state")
		if rejected == test.valid {
			t.Fatalf("%s with %s certificate: err = %v, want valid=%v", test.domain, test.certificate, err, test.valid)
		}
	}
}
