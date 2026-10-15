package ingress

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/TorstenDittmann/orch/internal/model"
)

func TestRenderIsDeterministicAndRoutesOnlyPrivateUpstreams(t *testing.T) {
	routes := []Route{
		{Application: "app", Service: "web", Domain: "b.example.com", HTTPS: false, Upstreams: []string{"10.88.0.9:8080"}},
		{Application: "app", Service: "web", Domain: "a.example.com", HTTPS: true, Upstreams: []string{"10.88.0.4:8080", "10.88.0.3:8080"}},
	}
	first, firstDigest, err := Render(routes, "/run/orch/caddy.sock")
	if err != nil {
		t.Fatal(err)
	}
	second, secondDigest, err := Render([]Route{routes[1], routes[0]}, "/run/orch/caddy.sock")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || firstDigest != secondDigest {
		t.Fatalf("render was not deterministic\n%s\n%s", first, second)
	}
	sum := sha256.Sum256(first)
	if want := "sha256:" + hex.EncodeToString(sum[:]); firstDigest != want {
		t.Fatalf("render digest = %q, want exact-byte digest %q", firstDigest, want)
	}
	if strings.Index(string(first), "10.88.0.3:8080") > strings.Index(string(first), "10.88.0.4:8080") {
		t.Fatalf("upstreams are not sorted: %s", first)
	}
	for _, required := range []string{`"output":"file"`, `"roll_size_mb":10`, `"roll_gzip":true`, `"roll_keep":3`,
		`"mode":"0600"`, `https://a.example.com{http.request.uri}`} {
		if !strings.Contains(string(first), required) {
			t.Fatalf("rendered config is missing %s: %s", required, first)
		}
	}
	if strings.Contains(string(first), "{http.request.host}") {
		t.Fatalf("HTTPS redirect trusts the request Host header: %s", first)
	}
}

func TestRenderPlacesNamedHostsBeforePublicIPCatchAll(t *testing.T) {
	configuration, _, err := Render([]Route{
		{Application: "app", Service: "public", Domain: model.PublicHTTPRoute, Upstreams: []string{"10.88.0.4:8080"}},
		{Application: "app", Service: "named", Domain: "app.example.com", Upstreams: []string{"10.88.0.5:8080"}},
	}, "/run/orch/caddy.sock")
	if err != nil {
		t.Fatal(err)
	}
	var decoded caddyConfig
	if err := json.Unmarshal(configuration, &decoded); err != nil {
		t.Fatal(err)
	}
	routes := decoded.Apps.HTTP.Servers["http"].Routes
	if len(routes) != 2 || len(routes[0].Match) != 1 || !bytes.Equal([]byte(routes[0].Match[0].Host[0]), []byte("app.example.com")) || len(routes[1].Match) != 0 {
		t.Fatalf("HTTP route order = %#v, want exact host followed by catch-all", routes)
	}
}

func TestRenderRejectsUnsafeDomainsAndDuplicateUpstreams(t *testing.T) {
	for _, test := range []Route{
		{Domain: "svc.internal", Upstreams: []string{"10.88.0.3:80"}},
		{Domain: "*.example.com", Upstreams: []string{"10.88.0.3:80"}},
		{Domain: "example.com", Upstreams: []string{"10.88.0.3:80", "10.88.0.3:80"}},
		{Domain: "example.com", Upstreams: []string{"203.0.113.3:80"}},
		{Domain: "example.com", Upstreams: []string{"10.88.0.3:not-a-port"}},
		{Domain: "example.com", Upstreams: []string{"10.88.0.3:0"}},
	} {
		if _, _, err := Render([]Route{test}, "/run/orch/caddy.sock"); err == nil {
			t.Fatalf("Render(%#v) succeeded", test)
		}
	}
}

func TestRenderRejectsInvalidPublicIPRoutes(t *testing.T) {
	for _, routes := range [][]Route{
		{{Domain: model.PublicHTTPRoute, HTTPS: true, Upstreams: []string{"10.88.0.3:80"}}},
		{{Domain: model.PublicHTTPRoute, Upstreams: []string{"10.88.0.3:80"}}, {Domain: model.PublicHTTPRoute, Upstreams: []string{"10.88.0.4:80"}}},
	} {
		if _, _, err := Render(routes, "/run/orch/caddy.sock"); err == nil {
			t.Fatalf("Render(%#v) accepted an invalid public route", routes)
		}
	}
}

func TestRenderIsolatedTestACMEIssuer(t *testing.T) {
	issuer := &testACMEIssuer{
		directoryURL:   "https://127.0.0.1:14000/dir",
		trustedRootPEM: "/tmp/orch-pebble-root.pem",
	}
	configuration, _, err := render([]Route{{
		Domain: "acme.example.com", HTTPS: true, Upstreams: []string{"10.88.0.3:8080"},
	}}, "/run/orch/caddy.sock", issuer)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`"tls":{"automation":{"policies":[{"subjects":["acme.example.com"]`,
		`"module":"acme"`, `"ca":"https://127.0.0.1:14000/dir"`,
		`"trusted_roots_pem_files":["/tmp/orch-pebble-root.pem"]`,
	} {
		if !strings.Contains(string(configuration), required) {
			t.Fatalf("isolated issuer configuration is missing %s: %s", required, configuration)
		}
	}
	production, _, err := Render([]Route{{
		Domain: "acme.example.com", HTTPS: true, Upstreams: []string{"10.88.0.3:8080"},
	}}, "/run/orch/caddy.sock")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(production), "orch-pebble") || strings.Contains(string(production), `"ca":`) {
		t.Fatalf("test issuer escaped into production rendering: %s", production)
	}
	for _, invalid := range []*testACMEIssuer{
		{directoryURL: "https://acme-v02.api.letsencrypt.org/directory", trustedRootPEM: "/tmp/root.pem"},
		{directoryURL: "http://127.0.0.1:14000/dir", trustedRootPEM: "/tmp/root.pem"},
		{directoryURL: "https://127.0.0.1:14000/dir", trustedRootPEM: "relative.pem"},
	} {
		if _, _, err := render(nil, "/run/orch/caddy.sock", invalid); err == nil {
			t.Fatalf("render accepted unsafe isolated issuer %#v", invalid)
		}
	}
}
