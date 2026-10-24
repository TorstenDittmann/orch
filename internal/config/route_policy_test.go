package config

import (
	"errors"
	"reflect"
	"testing"
)

const routePolicyManifest = `amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

name = "site"

services {
  ["web"] {
    image = "example.invalid/web:1"
    http = new Http {
      port = 8080
      domains { "example.com" }
      healthPath = "/healthz"
      basicAuth = new BasicAuth { user = "admin"; passwordSecret = "admin-password" }
      redirects { ["www.example.com"] = "example.com" }
    }
  }
}
`

func TestLoadBasicAuthAndRedirects(t *testing.T) {
	manifest := loadManifest(t, routePolicyManifest)
	web := manifest.Services["web"].HTTP
	if web.BasicAuth == nil || *web.BasicAuth != (BasicAuth{User: "admin", PasswordSecret: "admin-password"}) {
		t.Fatalf("basic auth = %#v", web.BasicAuth)
	}
	if !reflect.DeepEqual(web.Redirects, map[string]string{"www.example.com": "example.com"}) {
		t.Fatalf("redirects = %#v", web.Redirects)
	}
	canonical, err := CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseCanonicalJSON(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.Services["web"], manifest.Services["web"]) {
		t.Fatalf("canonical round trip = %#v", parsed.Services["web"])
	}
}

func TestValidateRejectsBadRoutePolicy(t *testing.T) {
	cases := map[string]struct {
		edit func(*Manifest)
		path string
	}{
		"redirect to unknown domain": {func(m *Manifest) {
			m.Services["web"].HTTP.Redirects["www.example.com"] = "other.example.com"
		}, `services.web.http.redirects["www.example.com"]`},
		"redirect a served domain": {func(m *Manifest) {
			m.Services["web"].HTTP.Redirects["example.com"] = "example.com"
		}, `services.web.http.redirects["example.com"]`},
		"redirect from wildcard": {func(m *Manifest) {
			m.Services["web"].HTTP.Redirects["*.example.com"] = "example.com"
		}, `services.web.http.redirects["*.example.com"]`},
		"redirect claimed by another service": {func(m *Manifest) {
			other := m.Services["web"]
			other.HTTP = &HTTP{Port: 8080, Domains: []string{"www.example.com"}, HealthPath: "/healthz"}
			m.Services["api"] = other
		}, `services.web.http.redirects["www.example.com"]`},
		"user with colon": {func(m *Manifest) {
			m.Services["web"].HTTP.BasicAuth.User = "ad:min"
		}, "services.web.http.basic_auth.user"},
		"auth without ingress": {func(m *Manifest) {
			m.Services["web"].HTTP.Domains = nil
			m.Services["web"].HTTP.Redirects = nil
		}, "services.web.http.basic_auth"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			manifest := loadManifest(t, routePolicyManifest)
			test.edit(manifest)
			var validation *ValidationError
			if !errors.As(Validate(manifest), &validation) || !hasProblem(validation, test.path) {
				t.Fatalf("Validate() = %v, want problem at %s", validation, test.path)
			}
		})
	}
}
