package ingress

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

type renderedRoute struct {
	Match  []caddyMatcher    `json:"match"`
	Handle []json.RawMessage `json:"handle"`
}

func renderedServers(t *testing.T, configuration []byte) map[string][]renderedRoute {
	t.Helper()
	var decoded struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Routes []renderedRoute `json:"routes"`
				} `json:"servers"`
			} `json:"http"`
			TLS *struct {
				Automation struct {
					Policies []struct {
						Subjects []string `json:"subjects"`
					} `json:"policies"`
				} `json:"automation"`
			} `json:"tls"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(configuration, &decoded); err != nil {
		t.Fatal(err)
	}
	result := make(map[string][]renderedRoute)
	for name, server := range decoded.Apps.HTTP.Servers {
		result[name] = server.Routes
	}
	return result
}

func TestRenderSharesADomainByPath(t *testing.T) {
	configuration, _, err := Render([]Route{
		{Application: "shop", Service: "web", Domain: "example.com", HTTPS: true,
			Upstreams: []string{"10.88.0.4:3000"}, RedirectFrom: []string{"www.example.com"}},
		{Application: "shop", Service: "api", Domain: "example.com", HTTPS: true,
			Upstreams: []string{"10.88.0.5:8080"}, Path: "/api", StripPrefix: true},
		{Application: "shop", Service: "admin", Domain: "example.com", HTTPS: true,
			Upstreams: []string{"10.88.0.6:8080"}, Path: "/api/admin"},
	}, "/run/orch/caddy.sock")
	if err != nil {
		t.Fatal(err)
	}
	servers := renderedServers(t, configuration)

	var served []string
	for _, route := range servers["https"] {
		if len(route.Match) != 1 || !slices.Equal(route.Match[0].Host, []string{"example.com"}) {
			continue
		}
		upstream := string(route.Handle[len(route.Handle)-1])
		served = append(served, strings.Join(route.Match[0].Path, ",")+" "+upstream)
	}
	if len(served) != 3 ||
		!strings.HasPrefix(served[0], "/api/admin,/api/admin/* ") || !strings.Contains(served[0], "10.88.0.6:8080") ||
		!strings.HasPrefix(served[1], "/api,/api/* ") || !strings.Contains(served[1], "10.88.0.5:8080") ||
		!strings.HasPrefix(served[2], " ") || !strings.Contains(served[2], "10.88.0.4:3000") {
		t.Fatalf("example.com routes are not longest prefix first: %q", served)
	}
	for _, route := range servers["https"] {
		if len(route.Match) == 1 && slices.Equal(route.Match[0].Path, []string{"/api", "/api/*"}) {
			if len(route.Handle) != 2 || string(route.Handle[0]) != `{"handler":"rewrite","strip_path_prefix":"/api"}` {
				t.Fatalf("/api route does not strip its prefix: %s", route.Handle)
			}
		}
		if len(route.Match) == 1 && slices.Equal(route.Match[0].Path, []string{"/api/admin", "/api/admin/*"}) &&
			len(route.Handle) != 1 {
			t.Fatalf("/api/admin route should only proxy: %s", route.Handle)
		}
	}

	// The shared domain gets one plain HTTP redirect and one certificate.
	redirects := 0
	for _, route := range servers["http"] {
		if len(route.Match) == 1 && slices.Equal(route.Match[0].Host, []string{"example.com"}) {
			redirects++
			if len(route.Match[0].Path) != 0 || !strings.Contains(string(route.Handle[0]), `"status_code":308`) {
				t.Fatalf("plain HTTP route for example.com is not a whole-domain redirect: %s", route.Handle)
			}
		}
	}
	if redirects != 1 {
		t.Fatalf("example.com has %d plain HTTP redirects, want 1", redirects)
	}
}

func TestRenderPathRoutesKeepCertificatesUnique(t *testing.T) {
	configuration, _, err := Render([]Route{
		{Application: "a", Service: "web", Domain: "*.example.com", HTTPS: true, Upstreams: []string{"10.88.0.4:3000"}},
		{Application: "a", Service: "api", Domain: "*.example.com", HTTPS: true, Upstreams: []string{"10.88.0.5:8080"}, Path: "/api"},
		{Application: "a", Service: "api", Domain: "example.com", HTTPS: true, Upstreams: []string{"10.88.0.5:8080"}, Path: "/api"},
	}, "/run/orch/caddy.sock")
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Apps struct {
			TLS struct {
				Certificates struct {
					Automate []string `json:"automate"`
				} `json:"certificates"`
				Automation struct {
					Policies []struct {
						Subjects []string `json:"subjects"`
					} `json:"policies"`
				} `json:"automation"`
			} `json:"tls"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(configuration, &decoded); err != nil {
		t.Fatal(err)
	}
	policies := decoded.Apps.TLS.Automation.Policies
	if len(policies) != 2 || !slices.Equal(policies[0].Subjects, []string{"example.com"}) ||
		!slices.Equal(policies[1].Subjects, []string{"*.example.com"}) ||
		!slices.Equal(decoded.Apps.TLS.Certificates.Automate, []string{"example.com"}) {
		t.Fatalf("certificate subjects repeat a shared domain: %s", configuration)
	}
}

func TestRenderRejectsInvalidPathRoutes(t *testing.T) {
	upstream := []string{"10.88.0.4:8080"}
	for name, routes := range map[string][]Route{
		"duplicate path": {
			{Application: "a", Service: "one", Domain: "example.com", Upstreams: upstream, Path: "/api"},
			{Application: "a", Service: "two", Domain: "example.com", Upstreams: upstream, Path: "/api"},
		},
		"trailing slash":     {{Application: "a", Service: "one", Domain: "example.com", Upstreams: upstream, Path: "/api/"}},
		"placeholder":        {{Application: "a", Service: "one", Domain: "example.com", Upstreams: upstream, Path: "/{http.request.host}"}},
		"dot segment":        {{Application: "a", Service: "one", Domain: "example.com", Upstreams: upstream, Path: "/api/.."}},
		"strip without path": {{Application: "a", Service: "one", Domain: "example.com", Upstreams: upstream, StripPrefix: true}},
		"public with a path": {{Application: "a", Service: "one", Domain: model.PublicHTTPRoute, Upstreams: upstream, Path: "/api"}},
	} {
		if _, _, err := Render(routes, "/run/orch/caddy.sock"); err == nil {
			t.Errorf("%s: render accepted invalid routes", name)
		}
	}
}

func TestFakeReportsPathRoutesByKey(t *testing.T) {
	fake := &Fake{}
	statuses, err := fake.Apply(context.Background(), []Route{
		{Application: "a", Service: "web", Domain: "example.com", Upstreams: []string{"10.88.0.4:3000"}},
		{Application: "a", Service: "api", Domain: "example.com", Upstreams: []string{"10.88.0.5:8080"}, Path: "/api"},
	}, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || statuses[0].Domain != "example.com" || statuses[1].Domain != "example.com/api" {
		t.Fatalf("statuses = %+v", statuses)
	}
}
