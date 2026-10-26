package ingress

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"regexp"
	"strings"
	"testing"

	"github.com/TorstenDittmann/orch/internal/model"
)

const testArgon2idHash = "$argon2id$v=19$m=47104,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXphYmNkZWY"

func TestRenderBasicAuthAndRedirects(t *testing.T) {
	configuration, _, err := Render([]Route{{
		Application: "site", Service: "web", Domain: "example.com", HTTPS: true,
		Upstreams:    []string{"10.88.0.4:8080"},
		BasicAuth:    &model.HTTPBasicAuth{User: "admin", PasswordHash: testArgon2idHash},
		RedirectFrom: []string{"www.example.com"},
	}}, "/run/orch/caddy.sock")
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Routes []struct {
						Match  []caddyMatcher    `json:"match"`
						Handle []json.RawMessage `json:"handle"`
					} `json:"routes"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(configuration, &decoded); err != nil {
		t.Fatal(err)
	}
	handlers := func(server, host string) []string {
		for _, route := range decoded.Apps.HTTP.Servers[server].Routes {
			if len(route.Match) == 1 && route.Match[0].Host[0] == host {
				result := make([]string, len(route.Handle))
				for index, handle := range route.Handle {
					result[index] = string(handle)
				}
				return result
			}
		}
		t.Fatalf("no %s route for %s in %s", server, host, configuration)
		return nil
	}
	served := handlers("https", "example.com")
	if len(served) != 3 || !strings.Contains(served[0], `"handler":"subroute"`) || !strings.Contains(served[0], `"status_code":401`) ||
		!strings.Contains(served[1], `"handler":"authentication"`) ||
		!strings.Contains(served[1], `"algorithm":"argon2id"`) || !strings.Contains(served[1], `"username":"admin"`) ||
		!strings.Contains(served[1], `"hash_cache":{}`) || !strings.Contains(served[2], `"handler":"reverse_proxy"`) {
		t.Fatalf("protected route handlers = %v", served)
	}
	for _, server := range []string{"http", "https"} {
		redirect := handlers(server, "www.example.com")
		if len(redirect) != 1 || !strings.Contains(redirect[0], `"status_code":308`) ||
			!strings.Contains(redirect[0], `"https://example.com{http.request.uri}"`) {
			t.Fatalf("%s redirect handlers = %v", server, redirect)
		}
	}
}

func TestRenderPlainHTTPRedirectKeepsScheme(t *testing.T) {
	configuration, _, err := Render([]Route{{
		Application: "site", Service: "web", Domain: "example.com",
		Upstreams: []string{"10.88.0.4:8080"}, RedirectFrom: []string{"www.example.com"},
	}}, "/run/orch/caddy.sock")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configuration), `"http://example.com{http.request.uri}"`) ||
		strings.Contains(string(configuration), `":443"`) {
		t.Fatalf("plain HTTP redirect config = %s", configuration)
	}
}

func TestRenderRejectsUnsafeRoutePolicy(t *testing.T) {
	base := Route{Application: "site", Service: "web", Domain: "example.com", Upstreams: []string{"10.88.0.4:8080"}}
	cases := map[string]func(*Route){
		"placeholder user": func(route *Route) {
			route.BasicAuth = &model.HTTPBasicAuth{User: "{env.HOME}", PasswordHash: testArgon2idHash}
		},
		"bcrypt hash": func(route *Route) {
			route.BasicAuth = &model.HTTPBasicAuth{User: "admin", PasswordHash: "$2a$14$abcdefghijklmnopqrstuv"}
		},
		"redirect collides with domain": func(route *Route) { route.RedirectFrom = []string{"example.com"} },
		"wildcard redirect":             func(route *Route) { route.RedirectFrom = []string{"*.example.com"} },
		"public redirect": func(route *Route) {
			route.Domain = model.PublicHTTPRoute
			route.RedirectFrom = []string{"www.example.com"}
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			route := base
			edit(&route)
			if _, _, err := Render([]Route{route}, "/run/orch/caddy.sock"); err == nil {
				t.Fatal("Render() accepted an unsafe route")
			}
		})
	}
}

func TestProbeUpstreamsSeesPastBasicAuth(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	live := listener.Addr().String()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := closed.Addr().String()
	_ = closed.Close()
	defer listener.Close()

	route := Route{Domain: "example.com", Upstreams: []string{dead, live}}
	if err := probeUpstreams(context.Background(), route); err != nil {
		t.Fatalf("probeUpstreams() with a live upstream = %v", err)
	}
	route.Upstreams = []string{dead}
	var status *routeStatusError
	if err := probeUpstreams(context.Background(), route); !errors.As(err, &status) || status.status != 502 {
		t.Fatalf("probeUpstreams() with no live upstream = %v, want 502", err)
	}
}

func TestBasicAuthUserPatternMatchesOnlyTheUser(t *testing.T) {
	users := []string{"admin", "ab", "abc", "a.b-c_d", "x"}
	for _, user := range users {
		pattern := regexp.MustCompile(basicAuthUserPattern(user))
		for _, password := range []string{"", "p", "pa", "pas", "s3cr3t+/=", strings.Repeat("z", 40)} {
			for _, scheme := range []string{"Basic ", "basic "} {
				header := scheme + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
				if !pattern.MatchString(header) {
					t.Errorf("%s: %q not matched", user, header)
				}
			}
			for _, other := range append([]string{user + "x", user[:len(user)-1], "Admin", "root"}, users...) {
				if other == user {
					continue
				}
				header := "Basic " + base64.StdEncoding.EncodeToString([]byte(other+":"+password))
				if pattern.MatchString(header) {
					t.Errorf("%s: other user %q matched", user, other)
				}
			}
		}
	}
}
