package ingress

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
		{Domain: "*.com", Upstreams: []string{"10.88.0.3:80"}},
		{Domain: "*.*.example.com", Upstreams: []string{"10.88.0.3:80"}},
		{Domain: "a.*.example.com", Upstreams: []string{"10.88.0.3:80"}},
		{Domain: "*.example.com", RedirectFrom: []string{"www.example.org"}, Upstreams: []string{"10.88.0.3:80"}},
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

func TestRenderWildcardUsesOnDemandCertificatesAfterExactHosts(t *testing.T) {
	configuration, _, err := Render([]Route{
		{Application: "app", Service: "tenants", Domain: "*.example.com", HTTPS: true, Upstreams: []string{"10.88.0.4:8080"}},
		{Application: "app", Service: "api", Domain: "api.example.com", HTTPS: true, Upstreams: []string{"10.88.0.5:8080"}},
		{Application: "app", Service: "public", Domain: model.PublicHTTPRoute, Upstreams: []string{"10.88.0.6:8080"}},
	}, "/run/orch/caddy.sock")
	if err != nil {
		t.Fatal(err)
	}
	var decoded caddyConfig
	if err := json.Unmarshal(configuration, &decoded); err != nil {
		t.Fatal(err)
	}
	hosts := func(routes []caddyRoute) []string {
		result := make([]string, 0, len(routes))
		for _, route := range routes {
			if len(route.Match) == 0 {
				result = append(result, "")
				continue
			}
			result = append(result, strings.Join(route.Match[0].Host, ","))
		}
		return result
	}
	if got := hosts(decoded.Apps.HTTP.Servers["https"].Routes); strings.Join(got, " ") != "api.example.com *.example.com" {
		t.Fatalf("HTTPS route order = %q, want exact host before wildcard", got)
	}
	httpRoutes := decoded.Apps.HTTP.Servers["http"].Routes
	if got := hosts(httpRoutes); strings.Join(got, " ") != "127.0.0.1 api.example.com *.example.com " {
		t.Fatalf("HTTP route order = %q", got)
	}
	permission := httpRoutes[0].Match[0]
	if permission.ClientIP == nil || len(permission.ClientIP.Ranges) != 1 || permission.ClientIP.Ranges[0] != "127.0.0.1/32" ||
		len(permission.Path) != 1 || permission.Path[0] != onDemandPermissionPath {
		t.Fatalf("permission route = %#v, want loopback-only path match", permission)
	}
	if !strings.Contains(string(configuration), `"Location":["https://{http.request.host}{http.request.uri}"]`) {
		t.Fatalf("wildcard redirect does not keep the matched host: %s", configuration)
	}
	tls := decoded.Apps.TLS
	if tls == nil || tls.Automation.OnDemand == nil ||
		tls.Automation.OnDemand.Permission != (caddyPermission{Module: "http", Endpoint: "http://127.0.0.1" + onDemandPermissionPath}) {
		t.Fatalf("TLS app = %#v, want on-demand permission", tls)
	}
	policies := tls.Automation.Policies
	if len(policies) != 2 || policies[0].OnDemand || strings.Join(policies[0].Subjects, ",") != "api.example.com" ||
		!policies[1].OnDemand || strings.Join(policies[1].Subjects, ",") != "*.example.com" ||
		len(policies[0].Issuers)+len(policies[1].Issuers) != 0 {
		t.Fatalf("automation policies = %#v, want the exact host first, then an on-demand wildcard, with default issuers", policies)
	}
	if tls.Certificates == nil || strings.Join(tls.Certificates.Automate, ",") != "api.example.com" {
		t.Fatalf("certificates = %#v, want the exact host automated despite the wildcard", tls.Certificates)
	}
}

func TestRenderPlainWildcardNeedsNoCertificates(t *testing.T) {
	configuration, _, err := Render([]Route{
		{Application: "app", Service: "tenants", Domain: "*.example.com", Upstreams: []string{"10.88.0.4:8080"}},
	}, "/run/orch/caddy.sock")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(configuration), `"tls"`) || strings.Contains(string(configuration), onDemandPermissionPath) {
		t.Fatalf("plain HTTP wildcard configured certificates: %s", configuration)
	}
}

func TestFakeReportsOnDemandCertificateForWildcard(t *testing.T) {
	statuses, err := (&Fake{}).Apply(t.Context(), []Route{
		{Domain: "*.example.com", HTTPS: true, Upstreams: []string{"10.88.0.4:8080"}},
		{Domain: "example.com", HTTPS: true, Upstreams: []string{"10.88.0.4:8080"}},
		{Domain: "*.example.org", Upstreams: []string{"10.88.0.4:8080"}},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for index, want := range []string{"on_demand", "managed", "disabled"} {
		if statuses[index].CertificateState != want {
			t.Fatalf("route %d certificate = %q, want %q", index, statuses[index].CertificateState, want)
		}
	}
}
