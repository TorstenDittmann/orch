package panel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/security"
)

// fakeController keeps access tokens by name, like the controller.
type fakeController struct {
	mu     sync.Mutex
	tokens map[string]fakeToken
}

type fakeToken struct {
	credential  string
	application string
	readOnly    bool
}

func (f *fakeController) CheckAccessToken(_ context.Context, credential string) (*orchv1alpha1.ExchangeAccessTokenResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, token := range f.tokens {
		if token.credential == credential {
			return &orchv1alpha1.ExchangeAccessTokenResponse{Application: token.application, ReadOnly: token.readOnly}, nil
		}
	}
	return nil, errors.New("access token is invalid")
}

func (f *fakeController) DeleteAccessToken(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.tokens[name]
	delete(f.tokens, name)
	return ok, nil
}

// add stores a token and returns its ORCH_TOKEN value.
func (f *fakeController) add(name, application string, readOnly bool) string {
	id := strings.Repeat("a", 15) + string(rune('0'+len(f.tokens)))
	secret := strings.Repeat("B", 42) + string(rune('A'+len(f.tokens)))
	f.tokens[name] = fakeToken{credential: id + "." + secret, application: application, readOnly: readOnly}
	return security.EncodeAccessToken("https://attacker.example:9443#sha256:00", id, secret)
}

func newTestServer(t *testing.T, now *time.Time) (*Server, *fakeController) {
	t.Helper()
	controller := &fakeController{tokens: map[string]fakeToken{}}
	dashboard := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("dashboard")) })
	server, err := New(Options{Dashboard: dashboard, Controller: controller, Now: func() time.Time { return *now }})
	if err != nil {
		t.Fatal(err)
	}
	return server, controller
}

func get(server *Server, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "https://panel.example.com"+path, nil)
	if cookie != nil {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	return recorder
}

func loginPath(name, value string) string {
	return "/login?name=" + url.QueryEscape(name) + "&token=" + url.QueryEscape(value)
}

func sessionCookie(t *testing.T, recorder *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == cookieName && cookie.Value != "" {
			if !cookie.HttpOnly || !cookie.Secure {
				t.Fatalf("session cookie is not HttpOnly and Secure: %+v", cookie)
			}
			return cookie
		}
	}
	t.Fatalf("no session cookie; status %d body %s", recorder.Code, recorder.Body.String())
	return nil
}

func TestPanelSignsInOnceWithALink(t *testing.T) {
	now := time.Now()
	server, controller := newTestServer(t, &now)

	if response := get(server, "/", nil); response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "orch panel open") {
		t.Fatalf("visit without session = %d %s", response.Code, response.Body.String())
	}
	if response := get(server, "/api/snapshot", nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("API without session = %d", response.Code)
	}
	if response := get(server, "/healthz", nil); response.Code != http.StatusOK {
		t.Fatalf("healthz = %d", response.Code)
	}

	link := controller.add(LoginTokenPrefix+"one", Application, true)
	signIn := get(server, loginPath(LoginTokenPrefix+"one", link), nil)
	if signIn.Code != http.StatusSeeOther || signIn.Header().Get("Location") != "/" {
		t.Fatalf("sign-in = %d %s", signIn.Code, signIn.Header().Get("Location"))
	}
	cookie := sessionCookie(t, signIn)
	if response := get(server, "/", cookie); response.Code != http.StatusOK || response.Body.String() != "dashboard" {
		t.Fatalf("dashboard with session = %d %s", response.Code, response.Body.String())
	}
	if _, ok := controller.tokens[LoginTokenPrefix+"one"]; ok {
		t.Fatal("sign-in token was not revoked")
	}
	if response := get(server, loginPath(LoginTokenPrefix+"one", link), nil); response.Code != http.StatusForbidden {
		t.Fatalf("reused link = %d", response.Code)
	}

	get(server, "/logout", cookie)
	if response := get(server, "/api/snapshot", cookie); response.Code != http.StatusUnauthorized {
		t.Fatalf("API after logout = %d", response.Code)
	}
	second := sessionCookie(t, get(server, loginPath(LoginTokenPrefix+"two", controller.add(LoginTokenPrefix+"two", Application, true)), nil))
	now = now.Add(sessionLifetime)
	if response := get(server, "/api/snapshot", second); response.Code != http.StatusUnauthorized {
		t.Fatalf("API with an expired session = %d", response.Code)
	}
}

func TestPanelRejectsOtherTokens(t *testing.T) {
	now := time.Now()
	server, controller := newTestServer(t, &now)
	deploy := controller.add(LoginTokenPrefix+"deploy", Application, false)
	other := controller.add(LoginTokenPrefix+"other", "shop", true)
	misnamed := controller.add("shop-read-only", Application, true)
	controller.add(LoginTokenPrefix+"victim", Application, true)
	for name, path := range map[string]string{
		"deploy token":           loginPath(LoginTokenPrefix+"deploy", deploy),
		"another app's token":    loginPath(LoginTokenPrefix+"other", other),
		"token without prefix":   loginPath("shop-read-only", misnamed),
		"credential of another":  loginPath(LoginTokenPrefix+"victim", misnamed),
		"garbage":                loginPath(LoginTokenPrefix+"x", "orch_nope"),
		"missing token and name": "/login",
	} {
		if response := get(server, path, nil); response.Code != http.StatusForbidden {
			t.Fatalf("%s = %d", name, response.Code)
		}
	}
	// A credential that does not belong to the name never opens a session;
	// the named link is used up.
	if _, ok := controller.tokens[LoginTokenPrefix+"victim"]; ok {
		t.Fatal("named token survived a mismatched credential")
	}
}

func TestPanelLimitsInvalidLinks(t *testing.T) {
	now := time.Now()
	server, controller := newTestServer(t, &now)
	for range failedSignInLimit {
		get(server, loginPath(LoginTokenPrefix+"x", "orch_nope"), nil)
	}
	link := controller.add(LoginTokenPrefix+"ok", Application, true)
	if response := get(server, loginPath(LoginTokenPrefix+"ok", link), nil); response.Code != http.StatusTooManyRequests {
		t.Fatalf("sign-in after too many failures = %d", response.Code)
	}
	now = now.Add(time.Minute + time.Second)
	sessionCookie(t, get(server, loginPath(LoginTokenPrefix+"ok", link), nil))
}
