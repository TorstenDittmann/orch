package config

import (
	"errors"
	"reflect"
	"strings"
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
		"auth over plain HTTP": {func(m *Manifest) {
			m.Services["web"].HTTP.HTTPS = false
		}, "services.web.http.basic_auth"},
		"auth on the public listener": {func(m *Manifest) {
			m.Services["web"].HTTP.Public = true
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

const httpListingManifest = `amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

name = "storage"

services {
  ["rustfs"] {
    image = "example.invalid/rustfs:1"
    http = new Listing {
      new Http {
        port = 9000
        domains { "s3.example.com" }
        healthPath = "/health"
      }
      new Http {
        port = 9001
        domains { "console.example.com" }
      }
    }
  }
}
`

func TestLoadHTTPListing(t *testing.T) {
	manifest := loadManifest(t, httpListingManifest)
	http := manifest.Services["rustfs"].HTTP
	if !reflect.DeepEqual(http.Domains, []string{"console.example.com", "s3.example.com"}) {
		t.Fatalf("domains = %#v", http.Domains)
	}
	if !reflect.DeepEqual(http.DomainPorts, map[string]int{"console.example.com": 9001}) {
		t.Fatalf("domain ports = %#v", http.DomainPorts)
	}
	canonical, err := CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseCanonicalJSON(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.Services["rustfs"], manifest.Services["rustfs"]) {
		t.Fatalf("canonical round trip = %#v", parsed.Services["rustfs"])
	}
}

func TestSinglePortManifestHasNoDomainPorts(t *testing.T) {
	canonical, err := CanonicalJSON(loadManifest(t, routePolicyManifest))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), "domain_ports") {
		t.Fatalf("canonical JSON changed for a single-port service: %s", canonical)
	}
}

func TestHTTPListingRejectedByPkl(t *testing.T) {
	cases := map[string]struct{ entry, message string }{
		"repeats port":    {`port = 9000; domains { "console.example.com" }`, "every entry needs its own port"},
		"no domains":      {`port = 9001`, "http[1].domains"},
		"public":          {`port = 9001; public = true; domains { "console.example.com" }`, "http[1].public"},
		"health path":     {`port = 9001; healthPath = "/"; domains { "console.example.com" }`, "http[1].healthPath"},
		"other https":     {`port = 9001; https = false; domains { "console.example.com" }`, "http[1].https"},
		"other nodeLocal": {`port = 9001; nodeLocal = true; domains { "console.example.com" }`, "http[1].nodeLocal"},
		"other basicAuth": {`port = 9001; basicAuth = new BasicAuth { user = "a"; passwordSecret = "p" }; domains { "console.example.com" }`, "http[1].basicAuth"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			source := strings.Replace(httpListingManifest, `port = 9001
        domains { "console.example.com" }`, test.entry, 1)
			if _, err := loadManifestError(t, source); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("Load() error = %v, want %q", err, test.message)
			}
		})
	}
}

func TestValidateRejectsBadDomainPorts(t *testing.T) {
	cases := map[string]struct {
		edit func(*Manifest)
		path string
	}{
		"domain also on the main port": {func(m *Manifest) {
			http := m.Services["rustfs"].HTTP
			http.Domains = append(http.Domains, "console.example.com")
		}, `services.rustfs.http.domain_ports["console.example.com"]`},
		"invalid domain": {func(m *Manifest) {
			http := m.Services["rustfs"].HTTP
			http.Domains = []string{"console", "s3.example.com"}
			http.DomainPorts = map[string]int{"console": 9001}
		}, `services.rustfs.http.domain_ports["console"]`},
		"repeats port": {func(m *Manifest) {
			m.Services["rustfs"].HTTP.DomainPorts["console.example.com"] = 9000
		}, `services.rustfs.http.domain_ports["console.example.com"]`},
		"port out of range": {func(m *Manifest) {
			m.Services["rustfs"].HTTP.DomainPorts["console.example.com"] = 70000
		}, `services.rustfs.http.domain_ports["console.example.com"]`},
		"unknown domain": {func(m *Manifest) {
			m.Services["rustfs"].HTTP.DomainPorts["other.example.com"] = 9002
		}, `services.rustfs.http.domain_ports["other.example.com"]`},
		"claimed by another service": {func(m *Manifest) {
			other := m.Services["rustfs"]
			other.HTTP = &HTTP{Port: 8080, Domains: []string{"console.example.com"}, HealthPath: "/healthz", HTTPS: true}
			m.Services["admin"] = other
		}, `services.rustfs.http.domain_ports["console.example.com"]`},
		"without health path": {func(m *Manifest) {
			http := m.Services["rustfs"].HTTP
			http.Domains = []string{"console.example.com"}
			http.HealthPath = ""
		}, "services.rustfs.http.health_path"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			manifest := loadManifest(t, httpListingManifest)
			test.edit(manifest)
			var validation *ValidationError
			if !errors.As(Validate(manifest), &validation) || !hasProblem(validation, test.path) {
				t.Fatalf("Validate() = %v, want problem at %s", validation, test.path)
			}
		})
	}
}

func TestAllowPrivateNetworkReachesTheCanonicalManifest(t *testing.T) {
	manifest := loadManifest(t, routePolicyManifest+"\nallowPrivateNetwork = true\n")
	if !manifest.AllowPrivateNetwork {
		t.Fatal("allowPrivateNetwork was not evaluated")
	}
	canonical, err := CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseCanonicalJSON(canonical)
	if err != nil || !parsed.AllowPrivateNetwork {
		t.Fatalf("canonical round trip = %v, %v", parsed, err)
	}
	if plain := loadManifest(t, routePolicyManifest); plain.AllowPrivateNetwork {
		t.Fatal("allowPrivateNetwork is on by default")
	}
}
