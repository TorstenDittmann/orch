package ingress

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
)

type caddyConfig struct {
	Admin   caddyAdmin   `json:"admin"`
	Logging caddyLogging `json:"logging"`
	Apps    caddyApps    `json:"apps"`
}

type caddyAdmin struct {
	Listen string `json:"listen"`
}

type caddyApps struct {
	HTTP caddyHTTPApp `json:"http"`
	TLS  *caddyTLSApp `json:"tls,omitempty"`
}

type caddyTLSApp struct {
	Automation caddyTLSAutomation `json:"automation"`
}

type caddyTLSAutomation struct {
	Policies []caddyTLSAutomationPolicy `json:"policies"`
}

type caddyTLSAutomationPolicy struct {
	Subjects []string          `json:"subjects"`
	Issuers  []caddyACMEIssuer `json:"issuers"`
}

type caddyACMEIssuer struct {
	Module               string   `json:"module"`
	CA                   string   `json:"ca"`
	TrustedRootsPEMFiles []string `json:"trusted_roots_pem_files"`
}

// testACMEIssuer is deliberately private. It lets the privileged integration
// test exercise Caddy's real automatic-certificate path against an isolated CA
// without adding an operator-controlled issuer or arbitrary Caddy JSON surface.
type testACMEIssuer struct {
	directoryURL   string
	trustedRootPEM string
}

type caddyLogging struct {
	Sink *caddyLog           `json:"sink"`
	Logs map[string]caddyLog `json:"logs"`
}

type caddyLog struct {
	Writer  caddyFileLogWriter `json:"writer"`
	Level   string             `json:"level,omitempty"`
	Exclude []string           `json:"exclude,omitempty"`
}

type caddyFileLogWriter struct {
	Output       string `json:"output"`
	Filename     string `json:"filename"`
	Mode         string `json:"mode"`
	Roll         bool   `json:"roll"`
	RollSizeMB   int    `json:"roll_size_mb"`
	RollGzip     bool   `json:"roll_gzip"`
	RollKeep     int    `json:"roll_keep"`
	RollKeepDays int    `json:"roll_keep_days"`
}

type caddyHTTPApp struct {
	Servers map[string]caddyServer `json:"servers"`
}

type caddyServer struct {
	Listen                []string             `json:"listen"`
	Routes                []caddyRoute         `json:"routes,omitempty"`
	TLSConnectionPolicies []map[string]any     `json:"tls_connection_policies,omitempty"`
	AutomaticHTTPS        *caddyAutomaticHTTPS `json:"automatic_https,omitempty"`
}

type caddyAutomaticHTTPS struct {
	Disable bool `json:"disable"`
}

type caddyRoute struct {
	Match    []caddyMatcher `json:"match,omitempty"`
	Handle   []any          `json:"handle"`
	Terminal bool           `json:"terminal"`
}

type caddyMatcher struct {
	Host []string `json:"host"`
}

type reverseProxyHandler struct {
	Handler   string          `json:"handler"`
	Upstreams []caddyUpstream `json:"upstreams"`
}

type caddyUpstream struct {
	Dial string `json:"dial"`
}

type authenticationHandler struct {
	Handler   string                  `json:"handler"`
	Providers authenticationProviders `json:"providers"`
}

type authenticationProviders struct {
	HTTPBasic httpBasicProvider `json:"http_basic"`
}

type httpBasicProvider struct {
	Hash      map[string]string `json:"hash"`
	Accounts  []basicAccount    `json:"accounts"`
	HashCache struct{}          `json:"hash_cache"`
}

type basicAccount struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type staticResponseHandler struct {
	Handler    string              `json:"handler"`
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers"`
}

func Render(routes []Route, adminSocket string) ([]byte, string, error) {
	return render(routes, adminSocket, nil)
}

func render(routes []Route, adminSocket string, testIssuer *testACMEIssuer) ([]byte, string, error) {
	if strings.TrimSpace(adminSocket) == "" || strings.ContainsAny(adminSocket, "\r\n") {
		return nil, "", fmt.Errorf("Caddy admin socket is required")
	}
	if err := validateTestACMEIssuer(testIssuer); err != nil {
		return nil, "", err
	}
	routes = cloneRoutes(routes)
	sort.Slice(routes, func(i, j int) bool {
		leftPublic := routes[i].Domain == model.PublicHTTPRoute
		rightPublic := routes[j].Domain == model.PublicHTTPRoute
		if leftPublic != rightPublic {
			return !leftPublic
		}
		return routes[i].Domain < routes[j].Domain
	})
	seen := make(map[string]struct{}, len(routes))
	httpRoutes := make([]caddyRoute, 0, len(routes))
	httpsRoutes := make([]caddyRoute, 0, len(routes))
	httpsSubjects := make([]string, 0, len(routes))
	for _, route := range routes {
		if route.Domain == model.PublicHTTPRoute {
			if route.HTTPS {
				return nil, "", fmt.Errorf("public IP ingress cannot enable HTTPS")
			}
		} else if err := config.ValidateDomain(route.Domain); err != nil {
			return nil, "", fmt.Errorf("invalid ingress domain %q: %w", route.Domain, err)
		}
		if _, exists := seen[route.Domain]; exists {
			return nil, "", fmt.Errorf("ingress domains must be unique")
		}
		seen[route.Domain] = struct{}{}
		sort.Strings(route.Upstreams)
		upstreams := make([]caddyUpstream, 0, len(route.Upstreams))
		lastUpstream := ""
		for _, upstream := range route.Upstreams {
			host, port, err := net.SplitHostPort(upstream)
			address, addressErr := netip.ParseAddr(host)
			portNumber, portErr := strconv.ParseUint(port, 10, 16)
			if err != nil || addressErr != nil || !address.Is4() || !address.IsPrivate() ||
				portErr != nil || portNumber == 0 || upstream == lastUpstream {
				return nil, "", fmt.Errorf("invalid or duplicate ingress upstream %q", upstream)
			}
			lastUpstream = upstream
			upstreams = append(upstreams, caddyUpstream{Dial: upstream})
		}
		if len(upstreams) == 0 {
			return nil, "", fmt.Errorf("ingress route %q has no ready upstream", route.Domain)
		}
		handlers := make([]any, 0, 2)
		if route.BasicAuth != nil {
			handler, err := basicAuthHandler(*route.BasicAuth)
			if err != nil {
				return nil, "", fmt.Errorf("ingress route %q: %w", route.Domain, err)
			}
			handlers = append(handlers, handler)
		}
		handlers = append(handlers, reverseProxyHandler{Handler: "reverse_proxy", Upstreams: upstreams})
		reverse := caddyRoute{Handle: handlers, Terminal: true}
		if route.Domain != model.PublicHTTPRoute {
			reverse.Match = []caddyMatcher{{Host: []string{route.Domain}}}
		} else if len(route.RedirectFrom) != 0 {
			return nil, "", fmt.Errorf("public IP ingress cannot have redirects")
		}
		scheme := "http://"
		if route.HTTPS {
			scheme = "https://"
			httpsRoutes = append(httpsRoutes, reverse)
			httpsSubjects = append(httpsSubjects, route.Domain)
			httpRoutes = append(httpRoutes, redirectRoute(route.Domain, "https://"+route.Domain))
		} else {
			httpRoutes = append(httpRoutes, reverse)
		}
		redirectFrom := append([]string(nil), route.RedirectFrom...)
		sort.Strings(redirectFrom)
		for _, from := range redirectFrom {
			if err := config.ValidateDomain(from); err != nil {
				return nil, "", fmt.Errorf("invalid ingress redirect domain %q: %w", from, err)
			}
			if _, exists := seen[from]; exists {
				return nil, "", fmt.Errorf("ingress domains must be unique")
			}
			seen[from] = struct{}{}
			// Plain HTTP goes straight to the target so a redirect never
			// takes two hops.
			httpRoutes = append(httpRoutes, redirectRoute(from, scheme+route.Domain))
			if route.HTTPS {
				httpsRoutes = append(httpsRoutes, redirectRoute(from, scheme+route.Domain))
				httpsSubjects = append(httpsSubjects, from)
			}
		}
	}
	servers := map[string]caddyServer{
		"http": {
			Listen: []string{":80"}, Routes: httpRoutes,
			AutomaticHTTPS: &caddyAutomaticHTTPS{Disable: true},
		},
	}
	if len(httpsRoutes) != 0 {
		servers["https"] = caddyServer{
			Listen: []string{":443"}, Routes: httpsRoutes,
			TLSConnectionPolicies: []map[string]any{{}},
		}
	}
	apps := caddyApps{HTTP: caddyHTTPApp{Servers: servers}}
	if testIssuer != nil && len(httpsSubjects) != 0 {
		apps.TLS = &caddyTLSApp{Automation: caddyTLSAutomation{Policies: []caddyTLSAutomationPolicy{{
			Subjects: httpsSubjects,
			Issuers: []caddyACMEIssuer{{Module: "acme", CA: testIssuer.directoryURL,
				TrustedRootsPEMFiles: []string{testIssuer.trustedRootPEM}}},
		}}}}
	}
	value := caddyConfig{
		Admin: caddyAdmin{Listen: "unix//" + adminSocket},
		Logging: caddyLogging{
			Sink: &caddyLog{Writer: managedLogWriter(adminSocket), Level: "INFO"},
			Logs: map[string]caddyLog{"default": {
				Writer: managedLogWriter(adminSocket), Level: "INFO", Exclude: []string{"http.log.access"},
			}},
		},
		Apps: apps,
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, "", fmt.Errorf("encode Caddy configuration: %w", err)
	}
	configuration := append(encoded, '\n')
	digest := sha256.Sum256(configuration)
	return configuration, "sha256:" + hex.EncodeToString(digest[:]), nil
}

var (
	basicAuthUser = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	argon2idHash  = regexp.MustCompile(`^\$argon2id\$v=19\$m=[0-9]+,t=[0-9]+,p=[0-9]+\$[A-Za-z0-9+/]+\$[A-Za-z0-9+/]+$`)
)

// basicAuthHandler checks the account before the reverse proxy runs. The hash
// cache keeps argon2id from running on every request with the same password.
func basicAuthHandler(auth model.HTTPBasicAuth) (authenticationHandler, error) {
	if !basicAuthUser.MatchString(auth.User) || !argon2idHash.MatchString(auth.PasswordHash) {
		return authenticationHandler{}, fmt.Errorf("basic auth account is invalid")
	}
	return authenticationHandler{Handler: "authentication", Providers: authenticationProviders{HTTPBasic: httpBasicProvider{
		Hash:     map[string]string{"algorithm": "argon2id"},
		Accounts: []basicAccount{{Username: auth.User, Password: auth.PasswordHash}},
	}}}, nil
}

func redirectRoute(from, target string) caddyRoute {
	return caddyRoute{
		Match: []caddyMatcher{{Host: []string{from}}},
		Handle: []any{staticResponseHandler{
			Handler: "static_response", StatusCode: 308,
			Headers: map[string][]string{"Location": {target + "{http.request.uri}"}},
		}},
		Terminal: true,
	}
}

func validateTestACMEIssuer(issuer *testACMEIssuer) error {
	if issuer == nil {
		return nil
	}
	directory, err := url.Parse(issuer.directoryURL)
	if err != nil || directory.Scheme != "https" || directory.User != nil || directory.Host == "" ||
		directory.RawQuery != "" || directory.Fragment != "" {
		return fmt.Errorf("test ACME directory must be an isolated HTTPS endpoint")
	}
	host := directory.Hostname()
	address, addressErr := netip.ParseAddr(host)
	if host != "localhost" && (addressErr != nil || !address.IsLoopback()) {
		return fmt.Errorf("test ACME directory must use a loopback host")
	}
	if !filepath.IsAbs(issuer.trustedRootPEM) || strings.ContainsAny(issuer.trustedRootPEM, "\r\n") {
		return fmt.Errorf("test ACME trusted root must be an absolute path")
	}
	return nil
}

func managedLogWriter(adminSocket string) caddyFileLogWriter {
	return caddyFileLogWriter{
		Output: "file", Filename: filepath.Join(filepath.Dir(adminSocket), "caddy.log"),
		Mode: "0600", Roll: true, RollSizeMB: 10, RollGzip: true,
		RollKeep: 3, RollKeepDays: 7,
	}
}
