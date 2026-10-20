// Package panel serves orch panel: a self-hosted dashboard that manages any
// number of orch clusters.
//
// Clusters are connected with `orch panel connect`, which creates an
// administrator access token on the cluster and hands it to the panel with a
// one-time connect code. There are no passwords: like orch web, a browser
// signs in with a one-time link that `orch panel open` prints. The link
// carries a short-lived access token that only an administrator of a
// connected cluster can create; the panel checks it with that cluster,
// revokes it, and gives the browser an HttpOnly session cookie.
package panel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/security"
	"github.com/TorstenDittmann/orch/internal/web"
)

const (
	cookieName = "orch_panel"
	// clusterHeader selects the cluster a dashboard API request is for.
	clusterHeader = "X-Orch-Cluster"
	// Application is the orch application orch panel install deploys and the
	// application sign-in tokens are issued for.
	Application = "orch-panel"
	// TokenName is the administrator access token a cluster gives the panel.
	TokenName = "orch-panel"
	// LoginTokenPrefix starts the name of every sign-in token, so the CLI
	// can clean up links that were never used.
	LoginTokenPrefix = "orch-panel-login-"
	// LoginTokenLifetime is how long a sign-in link works.
	LoginTokenLifetime = 15 * time.Minute
	// sessionLifetime is how long a sign-in lasts.
	sessionLifetime = 7 * 24 * time.Hour
	// failedAttemptLimit failed sign-ins or connects per minute are allowed
	// before the panel refuses more for the rest of that minute.
	failedAttemptLimit = 10
	requestTimeout     = 15 * time.Second
	maxBodyBytes       = 16 << 10
)

// Controller is the part of a cluster's controller API the panel needs
// beyond the dashboard.
type Controller interface {
	CheckAccessToken(context.Context, string) (*orchv1alpha1.ExchangeAccessTokenResponse, error)
	DeleteAccessToken(context.Context, string) (bool, error)
}

// Connection is a signed-in connection to one cluster.
type Connection struct {
	// Name is the cluster's own name, used when the connect request names
	// none.
	Name       string
	Controller Controller
	// Dashboard serves the dashboard API for this cluster, normally a
	// hosted web.Server.
	Dashboard http.Handler
	Close     func()
}

// Options configure a panel server.
type Options struct {
	// DataDir keeps the connected clusters. Empty keeps them in memory.
	DataDir string
	// Connect signs in to a cluster with an administrator access token and
	// must fail for any other token.
	Connect func(ctx context.Context, token string) (*Connection, error)
	// SetupCode connects the first cluster. A random one is generated and
	// logged when it is empty.
	SetupCode string
	// URL is where the panel is reached, used in the setup command it logs.
	URL string
	Log io.Writer
	Now func() time.Time
}

// Server signs browsers in and routes their requests to the selected
// cluster's dashboard.
type Server struct {
	connect   func(context.Context, string) (*Connection, error)
	assets    http.Handler
	store     *store
	setupCode string
	now       func() time.Time

	mu       sync.Mutex
	clusters map[string]*cluster
	codes    map[string]time.Time
	sessions map[string]time.Time
	failures []time.Time
}

// New loads the connected clusters and returns a panel server. It signs in
// to each cluster when it is first used, so clusters that cannot be reached
// do not hold up the panel.
func New(options Options) (*Server, error) {
	if options.Connect == nil {
		return nil, errors.New("panel needs a way to connect to clusters")
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	assets, err := web.Assets()
	if err != nil {
		return nil, err
	}
	s := &Server{
		connect: options.Connect, assets: assets, store: &store{dir: options.DataDir}, setupCode: options.SetupCode, now: now,
		clusters: map[string]*cluster{}, codes: map[string]time.Time{}, sessions: map[string]time.Time{},
	}
	stored, err := s.store.load()
	if err != nil {
		return nil, err
	}
	for _, entry := range stored {
		s.clusters[entry.Name] = newCluster(entry)
	}
	if s.setupCode == "" {
		if s.setupCode, err = randomHex(10); err != nil {
			return nil, err
		}
	}
	if len(stored) == 0 && options.Log != nil {
		url := options.URL
		if url == "" {
			url = "https://YOUR-PANEL"
		}
		fmt.Fprintf(options.Log, "orch panel has no clusters yet. Connect the first one from a workstation with:\n\n  orch panel connect %s --code %s\n\n", url, s.setupCode)
	}
	return s, nil
}

// Close disconnects from every cluster.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cluster := range s.clusters {
		cluster.close()
	}
}

// LoginURL is the one-time link for a sign-in token.
func LoginURL(panelURL, name, value string) string {
	return strings.TrimRight(panelURL, "/") + "/login?name=" + name + "&token=" + value
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
		return
	case "/login":
		s.login(w, r)
		return
	case "/logout":
		s.logout(w, r)
		return
	case "/connect":
		s.connectCluster(w, r)
		return
	}
	if !s.signedIn(r) {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/panel/") {
			http.Error(w, "sign in with a link from orch panel open", http.StatusUnauthorized)
			return
		}
		if s.clusterCount() == 0 {
			page(w, http.StatusUnauthorized, "Connect a cluster", "No cluster is connected yet. The panel's logs show the orch panel connect command for the first one.")
			return
		}
		page(w, http.StatusUnauthorized, "Sign in with a link", "Run orch panel open on a workstation with administrator access to one of the connected clusters. It prints a sign-in link that works once.")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/panel/api/"):
		s.panelAPI(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/"):
		s.dashboardAPI(w, r)
	default:
		s.assets.ServeHTTP(w, r)
	}
}

// sameOrigin requires changes to come from the panel's own pages as JSON,
// which a cross-site form cannot send.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	return (origin == "https://"+r.Host || origin == "http://"+r.Host) && strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
}

// dashboardAPI hands a dashboard request to the selected cluster, or the
// first one when none is selected.
func (s *Server) dashboardAPI(w http.ResponseWriter, r *http.Request) {
	name := r.Header.Get(clusterHeader)
	s.mu.Lock()
	if name == "" {
		for _, candidate := range s.sortedNames() {
			name = candidate
			break
		}
	}
	cluster := s.clusters[name]
	s.mu.Unlock()
	if cluster == nil {
		writeError(w, http.StatusConflict, errors.New("no cluster is connected; connect one in Clusters"))
		return
	}
	connection, err := cluster.ready(r.Context(), s.connect, s.now())
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("cannot reach cluster %s: %w", name, err))
		return
	}
	connection.Dashboard.ServeHTTP(w, r)
}

// login redeems a one-time link and starts a session.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.signedIn(r) {
		// An old link from history in a signed-in browser just opens the panel.
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if s.limited() {
		page(w, http.StatusTooManyRequests, "Too many attempts", "Too many invalid sign-in links. Wait a minute and open a new one.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	if err := s.redeem(ctx, r.URL.Query().Get("name"), r.URL.Query().Get("token")); err != nil {
		s.recordFailure()
		page(w, http.StatusForbidden, "This link does not work", "It is invalid, expired, was already used, or comes from a cluster this panel is not connected to. Run orch panel open for a new one.")
		return
	}
	id, err := randomHex(32)
	if err != nil {
		http.Error(w, "start session", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.sessions[id] = s.now().Add(sessionLifetime)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: id, Path: "/", MaxAge: int(sessionLifetime / time.Second),
		HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode,
	})
	// Leave the link's URL so it is not kept in the address bar.
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// redeem accepts a sign-in token exactly once. The token must come from a
// connected cluster, which must accept the credential as a read-only token
// for the panel; the named token must then be deleted by this request, and
// afterwards the credential must no longer work, which proves it was the
// named token.
func (s *Server) redeem(ctx context.Context, name, value string) error {
	if !strings.HasPrefix(name, LoginTokenPrefix) {
		return errors.New("not a sign-in token")
	}
	address, credential, err := security.ParseAccessToken(value)
	if err != nil {
		return err
	}
	// The address only picks which connected cluster to ask; the panel never
	// contacts it.
	cluster := s.clusterFor(address)
	if cluster == nil {
		return errors.New("sign-in token is not from a connected cluster")
	}
	connection, err := cluster.ready(ctx, s.connect, s.now())
	if err != nil {
		return err
	}
	controller := connection.Controller
	checked, err := controller.CheckAccessToken(ctx, credential)
	if err != nil {
		return err
	}
	if checked.GetApplication() != Application || !checked.GetReadOnly() || checked.GetAdministrator() {
		return errors.New("not a sign-in token for this panel")
	}
	deleted, err := controller.DeleteAccessToken(ctx, name)
	if err != nil || !deleted {
		return errors.New("sign-in token was already used")
	}
	if _, err := controller.CheckAccessToken(ctx, credential); err == nil {
		return errors.New("sign-in token does not match its name")
	}
	return nil
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, cookie.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode})
	page(w, http.StatusOK, "Signed out", "Run orch panel open to sign in again.")
}

func (s *Server) signedIn(r *http.Request) bool {
	cookie, err := r.Cookie(cookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.sessions[cookie.Value]
	if ok && !s.now().Before(expires) {
		delete(s.sessions, cookie.Value)
		return false
	}
	return ok
}

// limited reports whether the failed attempts of the last minute reached
// the limit, so a flood of bad links or codes cannot keep clusters busy.
func (s *Server) limited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-time.Minute)
	kept := s.failures[:0]
	for _, at := range s.failures {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	s.failures = kept
	return len(s.failures) >= failedAttemptLimit
}

func (s *Server) recordFailure() {
	s.mu.Lock()
	s.failures = append(s.failures, s.now())
	s.mu.Unlock()
}

func randomHex(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return hex.EncodeToString(value), nil
}

// secure reports whether the browser reached the panel over HTTPS, directly
// or through a proxy such as orch's ingress.
func secure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func readBody(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode request: %w", err))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func page(w http.ResponseWriter, status int, title, message string) {
	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_ = messagePage.Execute(w, struct{ Title, Message string }{title, message})
}

var messagePage = template.Must(template.New("page").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>orch panel</title><style>
:root{color-scheme:light dark;--bg:#f6f6f4;--card:#fff;--text:#1b1b1a;--muted:#6b6b66;--line:#e2e2dd}
@media (prefers-color-scheme:dark){:root{--bg:#121211;--card:#1b1b1a;--text:#ededea;--muted:#9a9a94;--line:#2d2d2a}}
*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--text);font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;padding:16px}
main{width:100%;max-width:400px;background:var(--card);border:1px solid var(--line);border-radius:12px;padding:28px}
h1{font-size:18px;margin:0 0 8px}p{margin:0;color:var(--muted)}
</style></head><body><main><h1>{{.Title}}</h1><p>{{.Message}}</p></main></body></html>`))
