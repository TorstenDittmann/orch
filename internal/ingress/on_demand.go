package ingress

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/TorstenDittmann/orch/internal/config"
)

const (
	// onDemandSocketName is the unix socket, next to Caddy's admin socket,
	// where the agent answers Caddy's on-demand certificate question.
	onDemandSocketName = "on-demand.sock"
	// onDemandNamesFile remembers every name the agent allowed, so a
	// certificate Caddy already holds is never refused later.
	onDemandNamesFile = "on-demand-names.json"
	// OnDemandNewNamesPerDay is how many names below one wildcard may get
	// their first certificate within 24 hours. Let's Encrypt allows 50 new
	// certificates per registered domain a week, so without a limit anyone
	// could spend that on random subdomains and block every certificate
	// under the domain, renewals included.
	OnDemandNewNamesPerDay = 20
	onDemandWindow         = 24 * time.Hour
	maxOnDemandNamesBytes  = 4 << 20
)

func onDemandSocket(adminSocket string) string {
	return filepath.Join(filepath.Dir(adminSocket), onDemandSocketName)
}

type onDemandName struct {
	Wildcard  string    `json:"wildcard"`
	AllowedAt time.Time `json:"allowed_at"`
}

// onDemandPolicy decides which names may get an on-demand certificate: one
// label below a wildcard currently routed with HTTPS, and at most
// OnDemandNewNamesPerDay new ones per wildcard a day. Names allowed before
// stay allowed, so renewals never compete with new names.
type onDemandPolicy struct {
	mu        sync.Mutex
	path      string
	now       func() time.Time
	wildcards map[string]bool
	names     map[string]onDemandName
	refused   map[string]time.Time
}

func newOnDemandPolicy(stateDir string, now func() time.Time) *onDemandPolicy {
	policy := &onDemandPolicy{path: filepath.Join(stateDir, onDemandNamesFile), now: now,
		wildcards: make(map[string]bool), names: make(map[string]onDemandName), refused: make(map[string]time.Time)}
	if contents, err := readAgentFile(policy.path, maxOnDemandNamesBytes); err == nil {
		if err := json.Unmarshal(contents, &policy.names); err != nil {
			log.Printf("ingress: ignore unreadable %s: %v", onDemandNamesFile, err)
			policy.names = make(map[string]onDemandName)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("ingress: ignore %s: %v", onDemandNamesFile, err)
	}
	return policy
}

func (p *onDemandPolicy) setWildcards(routes []Route) {
	wildcards := make(map[string]bool)
	for _, route := range routes {
		if route.HTTPS && config.IsWildcardDomain(route.Domain) {
			wildcards[route.Domain] = true
		}
	}
	p.mu.Lock()
	p.wildcards = wildcards
	p.mu.Unlock()
}

// allow reports whether name may get a certificate, recording it when it
// is new.
func (p *onDemandPolicy) allow(name string) bool {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	if config.ValidateExactDomain(name) != nil {
		return false
	}
	_, base, _ := strings.Cut(name, ".")
	wildcard := "*." + base
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.wildcards[wildcard] {
		return false
	}
	if known, ok := p.names[name]; ok && known.Wildcard == wildcard {
		return true
	}
	now := p.now().UTC()
	recent := 0
	for _, known := range p.names {
		if known.Wildcard == wildcard && now.Sub(known.AllowedAt) < onDemandWindow {
			recent++
		}
	}
	if recent >= OnDemandNewNamesPerDay {
		// Log once an hour per wildcard, so a flood does not flood the log.
		if last, ok := p.refused[wildcard]; !ok || now.Sub(last) >= time.Hour {
			p.refused[wildcard] = now
			log.Printf("ingress: refused a certificate for %s: %d new names below %s in the last 24 hours; list names you need as exact domains",
				name, recent, wildcard)
		}
		return false
	}
	p.names[name] = onDemandName{Wildcard: wildcard, AllowedAt: now}
	contents, err := json.Marshal(p.names)
	if err == nil {
		err = publishAgentFile(p.path, contents, 0o600)
	}
	if err != nil {
		// Still allowed for now; it is only forgotten after a restart.
		log.Printf("ingress: remember on-demand certificate name: %v", err)
	}
	return true
}

// ServeHTTP answers Caddy's permission request, GET ?domain=NAME, with 200
// to allow a certificate and 403 to refuse it.
func (p *onDemandPolicy) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet || request.URL.Path != onDemandPermissionPath {
		http.NotFound(response, request)
		return
	}
	if !p.allow(request.URL.Query().Get("domain")) {
		http.Error(response, "not allowed", http.StatusForbidden)
		return
	}
	response.WriteHeader(http.StatusOK)
}

// onDemandServer serves the policy on a unix socket only Caddy's user can
// open.
type onDemandServer struct {
	server   *http.Server
	listener net.Listener
}

func startOnDemandServer(path string, policy http.Handler, identity processIdentity) (*onDemandServer, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err == nil && identity.uid != 0 {
		err = os.Chown(path, identity.uid, identity.gid)
	}
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	server := &http.Server{Handler: policy, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 5 * time.Second, MaxHeaderBytes: 8 << 10}
	go func() { _ = server.Serve(listener) }()
	return &onDemandServer{server: server, listener: listener}, nil
}

func (s *onDemandServer) Close() error {
	if s == nil {
		return nil
	}
	return s.server.Close()
}
