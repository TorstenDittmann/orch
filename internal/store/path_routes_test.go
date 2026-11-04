package store

import (
	"testing"

	"github.com/TorstenDittmann/orch/internal/model"
)

func TestRouteMatchesHTTPIntentByPath(t *testing.T) {
	intent := &model.HTTPDesired{Port: 8080, Domains: []string{"example.com"}, HTTPS: true, Paths: []string{"/api"}}
	for domain, want := range map[string]bool{"example.com/api": true, "example.com": false, "example.com/v2": false} {
		if got := routeMatchesHTTPIntent(model.RouteStatus{Domain: domain, HTTPS: true}, intent); got != want {
			t.Errorf("route %s matched = %v, want %v", domain, got, want)
		}
	}
	whole := &model.HTTPDesired{Port: 8080, Domains: []string{"example.com"}, HTTPS: true}
	if !routeMatchesHTTPIntent(model.RouteStatus{Domain: "example.com", HTTPS: true}, whole) {
		t.Error("a service without paths no longer matches its domain")
	}
}
