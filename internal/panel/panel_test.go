package panel

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, now *time.Time) *Server {
	t.Helper()
	dashboard := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("dashboard")) })
	server, err := New(Options{Dashboard: dashboard, DataDir: t.TempDir(), SetupToken: "setup-secret", Now: func() time.Time { return *now }})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func post(server *Server, path string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "https://panel.example.com"+path, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://panel.example.com")
	if cookie != nil {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	return recorder
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

func TestPanelSetupSignInAndSessions(t *testing.T) {
	now := time.Now()
	server := newTestServer(t, &now)

	if response := get(server, "/", nil); response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/setup" {
		t.Fatalf("first visit = %d %s", response.Code, response.Header().Get("Location"))
	}
	if response := get(server, "/api/snapshot", nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("API without session = %d", response.Code)
	}
	if response := get(server, "/healthz", nil); response.Code != http.StatusOK {
		t.Fatalf("healthz = %d", response.Code)
	}
	if response := post(server, "/setup", url.Values{"token": {"wrong"}, "password": {"correct horse battery"}, "confirm": {"correct horse battery"}}, nil); response.Code != http.StatusForbidden {
		t.Fatalf("setup with a wrong token = %d", response.Code)
	}
	if response := post(server, "/setup", url.Values{"token": {"setup-secret"}, "password": {"short"}, "confirm": {"short"}}, nil); response.Code != http.StatusBadRequest {
		t.Fatalf("setup with a short password = %d", response.Code)
	}
	setup := post(server, "/setup", url.Values{"token": {"setup-secret"}, "password": {"correct horse battery"}, "confirm": {"correct horse battery"}}, nil)
	first := sessionCookie(t, setup)
	if response := get(server, "/", first); response.Code != http.StatusOK || response.Body.String() != "dashboard" {
		t.Fatalf("dashboard after setup = %d %s", response.Code, response.Body.String())
	}
	if response := post(server, "/setup", url.Values{"token": {"setup-secret"}, "password": {"another password!"}, "confirm": {"another password!"}}, nil); response.Code != http.StatusForbidden {
		t.Fatalf("setup link reused = %d", response.Code)
	}

	if response := get(server, "/", nil); response.Header().Get("Location") != "/login" {
		t.Fatalf("visit without session after setup = %s", response.Header().Get("Location"))
	}
	if response := post(server, "/login", url.Values{"password": {"not the password"}}, nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", response.Code)
	}
	second := sessionCookie(t, post(server, "/login", url.Values{"password": {"correct horse battery"}}, nil))
	if response := get(server, "/api/snapshot", second); response.Code != http.StatusOK {
		t.Fatalf("API with session = %d", response.Code)
	}

	get(server, "/logout", second)
	if response := get(server, "/api/snapshot", second); response.Code != http.StatusUnauthorized {
		t.Fatalf("API after logout = %d", response.Code)
	}
	now = now.Add(sessionLifetime)
	if response := get(server, "/api/snapshot", first); response.Code != http.StatusUnauthorized {
		t.Fatalf("API with an expired session = %d", response.Code)
	}
}

func TestPanelRejectsCrossOriginAndLimitsGuessing(t *testing.T) {
	now := time.Now()
	server := newTestServer(t, &now)
	sessionCookie(t, post(server, "/setup", url.Values{"token": {"setup-secret"}, "password": {"correct horse battery"}, "confirm": {"correct horse battery"}}, nil))

	request := httptest.NewRequest(http.MethodPost, "https://panel.example.com/login", strings.NewReader("password=correct+horse+battery"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://evil.example")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin sign-in = %d", recorder.Code)
	}

	for range failedSignInLimit {
		post(server, "/login", url.Values{"password": {"guess"}}, nil)
	}
	if response := post(server, "/login", url.Values{"password": {"correct horse battery"}}, nil); response.Code != http.StatusTooManyRequests {
		t.Fatalf("sign-in after too many failures = %d", response.Code)
	}
	now = now.Add(time.Minute + time.Second)
	sessionCookie(t, post(server, "/login", url.Values{"password": {"correct horse battery"}}, nil))
}

func TestPanelAcceptsBrowserFetchMetadata(t *testing.T) {
	now := time.Now()
	server := newTestServer(t, &now)
	form := url.Values{"token": {"setup-secret"}, "password": {"correct horse battery"}, "confirm": {"correct horse battery"}}
	send := func(site, origin string) int {
		request := httptest.NewRequest(http.MethodPost, "https://panel.example.com/setup", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Sec-Fetch-Site", site)
		request.Header.Set("Origin", origin)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		return recorder.Code
	}
	// A cross-site form cannot borrow a matching Origin.
	if code := send("cross-site", "https://panel.example.com"); code != http.StatusForbidden {
		t.Fatalf("cross-site post = %d", code)
	}
	// Browsers send Origin: null for posts from pages without a referrer.
	if code := send("same-origin", "null"); code != http.StatusSeeOther {
		t.Fatalf("same-origin post with a null Origin = %d", code)
	}
}
