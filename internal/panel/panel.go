// Package panel serves orch panel: the web dashboard as an always-on
// application on the cluster it manages, behind a password sign-in.
//
// The panel runs as an ordinary orch application. It reaches the controller
// with an administrator access token (orch token create --admin), so the
// browser never holds cluster credentials: it signs in to the panel with a
// password and gets an HttpOnly session cookie.
package panel

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	cookieName = "orch_panel"
	// sessionLifetime is how long a sign-in lasts.
	sessionLifetime = 7 * 24 * time.Hour
	// minPasswordLength keeps the one administrator password guessable only
	// with effort; there is no second factor yet.
	minPasswordLength = 12
	// failedSignInLimit failed sign-ins per minute are allowed before the
	// panel refuses more for the rest of that minute.
	failedSignInLimit  = 10
	passwordIterations = 600_000
	adminFile          = "admin.json"
)

// Options configure a panel server.
type Options struct {
	// Dashboard serves everything behind the sign-in, normally a hosted
	// web.Server.
	Dashboard http.Handler
	// DataDir holds the administrator password hash. It is the panel's
	// volume, so the password survives restarts and upgrades.
	DataDir string
	// SetupToken, from orch panel install, lets its holder set the
	// administrator password once.
	SetupToken string
	Now        func() time.Time
}

// Server signs people in and hands their requests to the dashboard.
type Server struct {
	dashboard  http.Handler
	dataDir    string
	setupToken string
	now        func() time.Time

	mu       sync.Mutex
	sessions map[string]time.Time
	failures []time.Time
}

// admin is the stored administrator account.
type admin struct {
	Salt       string `json:"salt"`
	Hash       string `json:"hash"`
	Iterations int    `json:"iterations"`
	// UsedSetupToken is the SHA-256 of the setup token that set this
	// password, so the same link cannot set it again.
	UsedSetupToken string `json:"used_setup_token,omitempty"`
}

// New returns a panel server. The data directory must already exist.
func New(options Options) (*Server, error) {
	if options.Dashboard == nil || options.DataDir == "" {
		return nil, errors.New("panel needs a dashboard and a data directory")
	}
	if info, err := os.Stat(options.DataDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("panel data directory %s is missing", options.DataDir)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Server{
		dashboard: options.Dashboard, dataDir: options.DataDir, setupToken: options.SetupToken,
		now: now, sessions: map[string]time.Time{},
	}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
		return
	case "/setup":
		s.setup(w, r)
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
			http.Error(w, "sign in to orch panel first", http.StatusUnauthorized)
			return
		}
		target := "/login"
		if _, err := s.readAdmin(); errors.Is(err, os.ErrNotExist) {
			target = "/setup"
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	s.dashboard.ServeHTTP(w, r)
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	token := r.FormValue("token")
	if r.Method != http.MethodPost {
		page(w, http.StatusOK, setupPage, pageData{Token: token, Message: setupHint(token)})
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	stored, err := s.readAdmin()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		http.Error(w, "read administrator account", http.StatusInternalServerError)
		return
	}
	if s.setupToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.setupToken)) != 1 || stored.UsedSetupToken == digest(token) {
		page(w, http.StatusForbidden, setupPage, pageData{Token: token, Message: "This setup link is invalid or was already used. Run orch panel install again for a new one."})
		return
	}
	password := r.FormValue("password")
	if len(password) < minPasswordLength {
		page(w, http.StatusBadRequest, setupPage, pageData{Token: token, Message: fmt.Sprintf("Use at least %d characters.", minPasswordLength)})
		return
	}
	if password != r.FormValue("confirm") {
		page(w, http.StatusBadRequest, setupPage, pageData{Token: token, Message: "The passwords do not match."})
		return
	}
	account, err := newAdmin(password, digest(token))
	if err == nil {
		err = s.writeAdmin(account)
	}
	if err != nil {
		http.Error(w, "save administrator account", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	// A new password signs everyone else out.
	s.sessions = map[string]time.Time{}
	s.mu.Unlock()
	s.startSession(w, r)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		page(w, http.StatusOK, loginPage, pageData{})
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	if s.limited() {
		page(w, http.StatusTooManyRequests, loginPage, pageData{Message: "Too many failed sign-ins. Wait a minute and try again."})
		return
	}
	stored, err := s.readAdmin()
	if errors.Is(err, os.ErrNotExist) {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if err != nil {
		http.Error(w, "read administrator account", http.StatusInternalServerError)
		return
	}
	if !stored.matches(r.FormValue("password")) {
		s.recordFailure()
		page(w, http.StatusUnauthorized, loginPage, pageData{Message: "Wrong password."})
		return
	}
	s.startSession(w, r)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, cookie.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) {
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
	http.Redirect(w, r, "/", http.StatusSeeOther)
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
// limit.
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

func (s *Server) readAdmin() (admin, error) {
	var account admin
	contents, err := os.ReadFile(filepath.Join(s.dataDir, adminFile))
	if err != nil {
		return account, err
	}
	if err := json.Unmarshal(contents, &account); err != nil {
		return account, fmt.Errorf("parse administrator account: %w", err)
	}
	return account, nil
}

func (s *Server) writeAdmin(account admin) error {
	contents, err := json.Marshal(account)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(s.dataDir, adminFile+".*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), filepath.Join(s.dataDir, adminFile))
}

func newAdmin(password, usedSetupToken string) (admin, error) {
	salt, err := randomHex(16)
	if err != nil {
		return admin{}, err
	}
	hash, err := pbkdf2.Key(sha256.New, password, []byte(salt), passwordIterations, 32)
	if err != nil {
		return admin{}, err
	}
	return admin{Salt: salt, Hash: hex.EncodeToString(hash), Iterations: passwordIterations, UsedSetupToken: usedSetupToken}, nil
}

func (a admin) matches(password string) bool {
	if a.Iterations <= 0 || password == "" {
		return false
	}
	hash, err := pbkdf2.Key(sha256.New, password, []byte(a.Salt), a.Iterations, 32)
	return err == nil && subtle.ConstantTimeCompare([]byte(hex.EncodeToString(hash)), []byte(a.Hash)) == 1
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
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

// sameOrigin rejects form posts from other sites. Browsers mark every POST
// with Sec-Fetch-Site or, for older ones, an Origin header.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin"
	}
	origin := r.Header.Get("Origin")
	return origin == "https://"+r.Host || origin == "http://"+r.Host
}

func setupHint(token string) string {
	if token == "" {
		return "Open the setup link that orch panel install printed."
	}
	return ""
}

type pageData struct {
	Token   string
	Message string
}

func page(w http.ResponseWriter, status int, body *template.Template, data pageData) {
	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	// same-origin keeps the setup token out of other sites' logs while still
	// letting browsers name this origin on the form post.
	header.Set("Referrer-Policy", "same-origin")
	header.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_ = body.Execute(w, data)
}

const pageStyle = `<style>
:root{color-scheme:light dark;--bg:#f6f6f4;--card:#fff;--text:#1b1b1a;--muted:#6b6b66;--line:#e2e2dd;--accent:#1b1b1a;--accent-text:#fff;--warn:#b42318}
@media (prefers-color-scheme:dark){:root{--bg:#121211;--card:#1b1b1a;--text:#ededea;--muted:#9a9a94;--line:#2d2d2a;--accent:#ededea;--accent-text:#121211;--warn:#f97066}}
*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--text);font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;padding:16px}
main{width:100%;max-width:360px;background:var(--card);border:1px solid var(--line);border-radius:12px;padding:28px}
h1{font-size:18px;margin:0 0 4px}p{margin:0 0 20px;color:var(--muted)}label{display:block;font-size:13px;margin:14px 0 6px}
input{width:100%;padding:9px 11px;border:1px solid var(--line);border-radius:8px;background:transparent;color:inherit;font:inherit}
button{margin-top:20px;width:100%;padding:10px;border:0;border-radius:8px;background:var(--accent);color:var(--accent-text);font:inherit;font-weight:600;cursor:pointer}
.message{color:var(--warn);margin:16px 0 0}
</style>`

var loginPage = template.Must(template.New("login").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>orch panel</title>` + pageStyle + `</head><body><main>
<h1>orch panel</h1><p>Sign in to manage this cluster.</p>
<form method="post" action="/login"><label for="password">Password</label><input id="password" name="password" type="password" autocomplete="current-password" required autofocus>
<button type="submit">Sign in</button></form>
{{if .Message}}<p class="message">{{.Message}}</p>{{end}}
</main></body></html>`))

var setupPage = template.Must(template.New("setup").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Set up orch panel</title>` + pageStyle + `</head><body><main>
<h1>Set up orch panel</h1><p>Choose the administrator password.</p>
{{if .Token}}<form method="post" action="/setup"><input type="hidden" name="token" value="{{.Token}}">
<label for="password">Password</label><input id="password" name="password" type="password" autocomplete="new-password" minlength="12" required autofocus>
<label for="confirm">Repeat password</label><input id="confirm" name="confirm" type="password" autocomplete="new-password" minlength="12" required>
<button type="submit">Save and sign in</button></form>{{end}}
{{if .Message}}<p class="message">{{.Message}}</p>{{end}}
</main></body></html>`))
