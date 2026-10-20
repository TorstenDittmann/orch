// Package panel serves orch panel: the web dashboard as an always-on
// application on the cluster it manages.
//
// The panel runs as an ordinary orch application and reaches the controller
// with an administrator access token (orch token create --admin). There are
// no passwords: like orch web, a browser signs in with a one-time link that
// `orch panel open` prints. The link carries a short-lived access token that
// only an administrator can create; the panel checks it with the controller,
// revokes it, and gives the browser an HttpOnly session cookie.
package panel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/security"
)

const (
	cookieName = "orch_panel"
	// Application is the orch application the panel runs as.
	Application = "orch-panel"
	// LoginTokenPrefix starts the name of every sign-in token, so the CLI
	// can clean up links that were never used.
	LoginTokenPrefix = "orch-panel-login-"
	// LoginTokenLifetime is how long a sign-in link works.
	LoginTokenLifetime = 15 * time.Minute
	// sessionLifetime is how long a sign-in lasts.
	sessionLifetime = 7 * 24 * time.Hour
	// failedSignInLimit failed sign-ins per minute are allowed before the
	// panel refuses more for the rest of that minute.
	failedSignInLimit = 10
	requestTimeout    = 15 * time.Second
)

// Controller is the part of the controller API sign-in needs.
type Controller interface {
	CheckAccessToken(context.Context, string) (*orchv1alpha1.ExchangeAccessTokenResponse, error)
	DeleteAccessToken(context.Context, string) (bool, error)
}

// Options configure a panel server.
type Options struct {
	// Dashboard serves everything behind the sign-in, normally a hosted
	// web.Server.
	Dashboard  http.Handler
	Controller Controller
	Now        func() time.Time
}

// Server signs browsers in and hands their requests to the dashboard.
type Server struct {
	dashboard  http.Handler
	controller Controller
	now        func() time.Time

	mu       sync.Mutex
	sessions map[string]time.Time
	failures []time.Time
}

// New returns a panel server.
func New(options Options) (*Server, error) {
	if options.Dashboard == nil || options.Controller == nil {
		return nil, errors.New("panel needs a dashboard and a controller")
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Server{dashboard: options.Dashboard, controller: options.Controller, now: now, sessions: map[string]time.Time{}}, nil
}

// LoginURL is the one-time link for a sign-in token.
func LoginURL(domain, name, value string) string {
	return "https://" + domain + "/login?name=" + name + "&token=" + value
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
	}
	if !s.signedIn(r) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "sign in with a link from orch panel open", http.StatusUnauthorized)
			return
		}
		page(w, http.StatusUnauthorized, "Sign in with a link", "Run orch panel open on your workstation. It prints a sign-in link that works once.")
		return
	}
	s.dashboard.ServeHTTP(w, r)
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
		page(w, http.StatusForbidden, "This link does not work", "It is invalid, expired or was already used. Run orch panel open for a new one.")
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

// redeem accepts a sign-in token exactly once. The controller must accept the
// credential as a read-only token for the panel, the named token must then be
// deleted by this request, and afterwards the credential must no longer
// work, which proves it was the named token.
func (s *Server) redeem(ctx context.Context, name, value string) error {
	if !strings.HasPrefix(name, LoginTokenPrefix) {
		return errors.New("not a sign-in token")
	}
	// The token names a cluster address, but only this panel's own
	// controller is asked about it.
	_, credential, err := security.ParseAccessToken(value)
	if err != nil {
		return err
	}
	checked, err := s.controller.CheckAccessToken(ctx, credential)
	if err != nil {
		return err
	}
	if checked.GetApplication() != Application || !checked.GetReadOnly() || checked.GetAdministrator() {
		return errors.New("not a sign-in token for this panel")
	}
	deleted, err := s.controller.DeleteAccessToken(ctx, name)
	if err != nil || !deleted {
		return errors.New("sign-in token was already used")
	}
	if _, err := s.controller.CheckAccessToken(ctx, credential); err == nil {
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

// limited reports whether the failed sign-ins of the last minute reached the
// limit, so a flood of bad links cannot keep the controller busy.
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
	return len(s.failures) >= failedSignInLimit
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
// or through orch's ingress.
func secure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
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
