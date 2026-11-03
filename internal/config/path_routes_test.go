package config

import (
	"errors"
	"reflect"
	"testing"
)

const pathRoutesManifest = `amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

name = "shop"

services {
  ["web"] {
    image = "example.invalid/web:1"
    http = new Http {
      port = 3000
      domains { "example.com" }
      healthPath = "/"
    }
  }
  ["api"] {
    image = "example.invalid/api:1"
    http = new Http {
      port = 8080
      domains { "example.com" }
      paths { "/api" }
      stripPrefix = true
      healthPath = "/healthz"
    }
  }
}
`

func TestLoadPathRoutes(t *testing.T) {
	manifest := loadManifest(t, pathRoutesManifest)
	api := manifest.Services["api"].HTTP
	if !reflect.DeepEqual(api.Paths, []string{"/api"}) || !api.StripPrefix {
		t.Fatalf("api http = %#v", api)
	}
	if web := manifest.Services["web"].HTTP; web.Paths != nil || web.StripPrefix {
		t.Fatalf("web http = %#v", web)
	}
	if !reflect.DeepEqual(api.RouteKeys(), []string{"example.com/api"}) {
		t.Fatalf("route keys = %#v", api.RouteKeys())
	}
}

// pathRoutesCanonical shares hello.example.com between web on / and an api
// service on /api.
func pathRoutesCanonical(t *testing.T) *Manifest {
	t.Helper()
	manifest := loadHelloCanonical(t)
	api := manifest.Services["web"]
	api.HTTP = &HTTP{Port: 8080, Domains: []string{"hello.example.com"}, HealthPath: "/healthz", HTTPS: true,
		Paths: []string{"/api"}, StripPrefix: true}
	manifest.Services["api"] = api
	return manifest
}

func TestValidateAcceptsPathRoutesOnASharedDomain(t *testing.T) {
	manifest := pathRoutesCanonical(t)
	// A longer prefix may belong to yet another service.
	admin := manifest.Services["web"]
	admin.HTTP = &HTTP{Port: 8080, Domains: []string{"hello.example.com"}, HealthPath: "/healthz", HTTPS: true,
		Paths: []string{"/api/admin", "/admin"}}
	manifest.Services["admin"] = admin
	if err := Validate(manifest); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestValidateRejectsBadPathRoutes(t *testing.T) {
	cases := map[string]struct {
		edit func(*Manifest)
		path string
	}{
		"same domain without paths": {func(m *Manifest) {
			m.Services["api"].HTTP.Paths = nil
			m.Services["api"].HTTP.StripPrefix = false
		}, "services.web.http.domains[0]"},
		"same path twice": {func(m *Manifest) {
			m.Services["web"].HTTP.Paths = []string{"/api"}
		}, "services.web.http.domains[0]"},
		"trailing slash": {func(m *Manifest) {
			m.Services["api"].HTTP.Paths = []string{"/api/"}
		}, "services.api.http.paths[0]"},
		"no leading slash": {func(m *Manifest) {
			m.Services["api"].HTTP.Paths = []string{"api"}
		}, "services.api.http.paths[0]"},
		"wildcard": {func(m *Manifest) {
			m.Services["api"].HTTP.Paths = []string{"/api/*"}
		}, "services.api.http.paths[0]"},
		"uppercase": {func(m *Manifest) {
			m.Services["api"].HTTP.Paths = []string{"/API"}
		}, "services.api.http.paths[0]"},
		"dot segment": {func(m *Manifest) {
			m.Services["api"].HTTP.Paths = []string{"/api/.."}
		}, "services.api.http.paths[0]"},
		"duplicate path": {func(m *Manifest) {
			m.Services["api"].HTTP.Paths = []string{"/api", "/api"}
		}, "services.api.http.paths[1]"},
		"strip without paths": {func(m *Manifest) {
			m.Services["web"].HTTP.StripPrefix = true
		}, "services.web.http.strip_prefix"},
		"paths without domains": {func(m *Manifest) {
			m.Services["api"].HTTP.Domains = nil
		}, "services.api.http.paths"},
		"paths on the public listener": {func(m *Manifest) {
			m.Services["api"].HTTP.Public = true
		}, "services.api.http.paths"},
		"mixed https": {func(m *Manifest) {
			m.Services["web"].HTTP.HTTPS = false
		}, "services.web.http.domains[0]"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			manifest := pathRoutesCanonical(t)
			test.edit(manifest)
			var validation *ValidationError
			if !errors.As(Validate(manifest), &validation) || !hasProblem(validation, test.path) {
				t.Fatalf("Validate() = %v, want problem at %s", validation, test.path)
			}
		})
	}
}
