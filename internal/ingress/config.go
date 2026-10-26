package ingress

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
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
	Certificates *caddyCertificates `json:"certificates,omitempty"`
	Automation   caddyTLSAutomation `json:"automation"`
}

type caddyCertificates struct {
	Automate []string `json:"automate"`
}

type caddyTLSAutomation struct {
	OnDemand *caddyOnDemand             `json:"on_demand,omitempty"`
	Policies []caddyTLSAutomationPolicy `json:"policies"`
}

type caddyOnDemand struct {
	Permission caddyPermission `json:"permission"`
}

type caddyPermission struct {
	Module   string `json:"module"`
	Endpoint string `json:"endpoint"`
}

type caddyTLSAutomationPolicy struct {
	Subjects []string          `json:"subjects"`
	Issuers  []caddyACMEIssuer `json:"issuers,omitempty"`
	OnDemand bool              `json:"on_demand,omitempty"`
}

// onDemandPermissionPath answers Caddy's on-demand certificate question on the
// plain HTTP listener, reachable only from the node itself. Caddy asks only
// for names that match a wildcard policy's subjects, so those subjects are
// what bound issuance; the endpoint just has to say yes.
const onDemandPermissionPath = "/.well-known/orch/on-demand-tls"

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
	Host         []string               `json:"host,omitempty"`
	Path         []string               `json:"path,omitempty"`
	ClientIP     *caddyIPMatcher        `json:"client_ip,omitempty"`
	HeaderRegexp map[string]caddyRegexp `json:"header_regexp,omitempty"`
	Not          []caddyMatcher         `json:"not,omitempty"`
}

type caddyRegexp struct {
	Pattern string `json:"pattern"`
}

type subrouteHandler struct {
	Handler string       `json:"handler"`
	Routes  []caddyRoute `json:"routes"`
}

type caddyIPMatcher struct {
	Ranges []string `json:"ranges"`
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
	Headers    map[string][]string `json:"headers,omitempty"`
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
		if left, right := routeRank(routes[i].Domain), routeRank(routes[j].Domain); left != right {
			return left < right
		}
		return routes[i].Domain < routes[j].Domain
	})
	seen := make(map[string]struct{}, len(routes))
	httpRoutes := make([]caddyRoute, 0, len(routes))
	httpsRoutes := make([]caddyRoute, 0, len(routes))
	httpsSubjects := make([]string, 0, len(routes))
	wildcardSubjects := make([]string, 0)
	for _, route := range routes {
		wildcard := config.IsWildcardDomain(route.Domain)
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
			handlers = append(handlers, basicAuthUserGate(route.BasicAuth.User), handler)
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
			if wildcard {
				// Each subdomain gets its own certificate on its first TLS
				// handshake, so no DNS provider credentials are needed.
				wildcardSubjects = append(wildcardSubjects, route.Domain)
				httpRoutes = append(httpRoutes, redirectRoute(route.Domain, "https://{http.request.host}"))
			} else {
				httpsSubjects = append(httpsSubjects, route.Domain)
				httpRoutes = append(httpRoutes, redirectRoute(route.Domain, "https://"+route.Domain))
			}
		} else {
			httpRoutes = append(httpRoutes, reverse)
		}
		redirectFrom := append([]string(nil), route.RedirectFrom...)
		sort.Strings(redirectFrom)
		if wildcard && len(redirectFrom) != 0 {
			return nil, "", fmt.Errorf("ingress route %q cannot be a redirect target", route.Domain)
		}
		for _, from := range redirectFrom {
			if err := config.ValidateExactDomain(from); err != nil {
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
	if len(wildcardSubjects) != 0 {
		httpRoutes = append([]caddyRoute{onDemandPermissionRoute()}, httpRoutes...)
	}
	// Caddy tries routes in order, so an exact host such as api.example.com
	// wins over *.example.com, and both win over the public catch-all.
	for _, list := range [][]caddyRoute{httpRoutes, httpsRoutes} {
		sort.SliceStable(list, func(i, j int) bool { return matchRank(list[i]) < matchRank(list[j]) })
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
	var issuers []caddyACMEIssuer
	if testIssuer != nil {
		issuers = []caddyACMEIssuer{{Module: "acme", CA: testIssuer.directoryURL,
			TrustedRootsPEMFiles: []string{testIssuer.trustedRootPEM}}}
	}
	var automation caddyTLSAutomation
	var certificates *caddyCertificates
	if len(httpsSubjects) != 0 && (testIssuer != nil || len(wildcardSubjects) != 0) {
		// Caddy picks the first policy whose subjects match, so exact hosts
		// come before wildcards and keep certificates obtained up front.
		automation.Policies = append(automation.Policies, caddyTLSAutomationPolicy{Subjects: httpsSubjects, Issuers: issuers})
	}
	if len(wildcardSubjects) != 0 {
		// Caddy otherwise skips exact hosts under a managed wildcard, expecting
		// one wildcard certificate to cover them; on-demand never issues one.
		if len(httpsSubjects) != 0 {
			certificates = &caddyCertificates{Automate: httpsSubjects}
		}
		automation.OnDemand = &caddyOnDemand{Permission: caddyPermission{
			Module: "http", Endpoint: "http://127.0.0.1" + onDemandPermissionPath,
		}}
		automation.Policies = append(automation.Policies, caddyTLSAutomationPolicy{
			Subjects: wildcardSubjects, Issuers: issuers, OnDemand: true,
		})
	}
	if len(automation.Policies) != 0 {
		apps.TLS = &caddyTLSApp{Certificates: certificates, Automation: automation}
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

// basicAuthUserGate answers 401 to requests that do not name user before
// the authentication handler runs. Caddy checks an unknown username against
// a fixed argon2id hash that takes 46 MiB, so without the gate any client
// could make ingress allocate that much per request.
func basicAuthUserGate(user string) subrouteHandler {
	return subrouteHandler{Handler: "subroute", Routes: []caddyRoute{{
		Match: []caddyMatcher{{Not: []caddyMatcher{{HeaderRegexp: map[string]caddyRegexp{
			"Authorization": {Pattern: basicAuthUserPattern(user)},
		}}}}},
		Handle: []any{staticResponseHandler{Handler: "static_response", StatusCode: http.StatusUnauthorized,
			Headers: map[string][]string{"Www-Authenticate": {`Basic realm="restricted"`}}}},
		Terminal: true,
	}}}
}

// basicAuthUserPattern matches a Basic Authorization header whose
// credentials start with "user:", whatever the password. Base64 encodes
// whole groups of three bytes, so the prefix's full groups are fixed text
// and its last one or two bytes fix only some bits of the next character.
func basicAuthUserPattern(user string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	prefix := []byte(user + ":")
	full := len(prefix) / 3 * 3
	pattern := "^(?i:basic) " + regexp.QuoteMeta(base64.StdEncoding.EncodeToString(prefix[:full]))
	choices := func(base byte, count int) string {
		set := ""
		for offset := range count {
			set += string(alphabet[int(base)+offset])
		}
		return "[" + regexp.QuoteMeta(set) + "]"
	}
	switch rest := prefix[full:]; len(rest) {
	case 1:
		pattern += regexp.QuoteMeta(string(alphabet[rest[0]>>2])) + choices((rest[0]&3)<<4, 16)
	case 2:
		pattern += regexp.QuoteMeta(string(alphabet[rest[0]>>2])) +
			regexp.QuoteMeta(string(alphabet[(rest[0]&3)<<4|rest[1]>>4])) + choices((rest[1]&15)<<2, 4)
	}
	return pattern
}

// routeRank orders exact domains before wildcards and the public catch-all last.
func routeRank(domain string) int {
	switch {
	case domain == model.PublicHTTPRoute:
		return 2
	case config.IsWildcardDomain(domain):
		return 1
	default:
		return 0
	}
}

func matchRank(route caddyRoute) int {
	if len(route.Match) == 0 {
		return 2
	}
	for _, host := range route.Match[0].Host {
		if config.IsWildcardDomain(host) {
			return 1
		}
	}
	return 0
}

func onDemandPermissionRoute() caddyRoute {
	return caddyRoute{
		Match: []caddyMatcher{{
			Host: []string{"127.0.0.1"}, Path: []string{onDemandPermissionPath},
			ClientIP: &caddyIPMatcher{Ranges: []string{"127.0.0.1/32"}},
		}},
		Handle:   []any{staticResponseHandler{Handler: "static_response", StatusCode: 200}},
		Terminal: true,
	}
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
