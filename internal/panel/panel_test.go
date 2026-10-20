package panel

import (
	"bytes"
	"context"
	"encoding/json"
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

// fakeCluster keeps access tokens by name, like a controller.
type fakeCluster struct {
	name    string
	address string
	mu      sync.Mutex
	tokens  map[string]fakeToken
	next    int
}

type fakeToken struct {
	credential    string
	application   string
	readOnly      bool
	administrator bool
}

func newFakeCluster(name, pin string) *fakeCluster {
	return &fakeCluster{name: name, address: "https://" + name + ".example:9443#sha256:" + strings.Repeat(pin, 64), tokens: map[string]fakeToken{}}
}

func (f *fakeCluster) CheckAccessToken(_ context.Context, credential string) (*orchv1alpha1.ExchangeAccessTokenResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, token := range f.tokens {
		if token.credential == credential {
			return &orchv1alpha1.ExchangeAccessTokenResponse{Application: token.application, ReadOnly: token.readOnly, Administrator: token.administrator}, nil
		}
	}
	return nil, errors.New("access token is invalid")
}

func (f *fakeCluster) DeleteAccessToken(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.tokens[name]
	delete(f.tokens, name)
	return ok, nil
}

// add stores a token and returns its ORCH_TOKEN value.
func (f *fakeCluster) add(name string, token fakeToken) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := strings.Repeat("a", 14) + string(rune('a'+f.next/10)) + string(rune('0'+f.next%10))
	secret := strings.Repeat("B", 42) + string(rune('A'+f.next))
	token.credential = id + "." + secret
	f.tokens[name] = token
	return security.EncodeAccessToken(f.address, id, secret)
}

func (f *fakeCluster) admin() string {
	return f.add(TokenName, fakeToken{administrator: true})
}

func (f *fakeCluster) login(name string) string {
	return f.add(LoginTokenPrefix+name, fakeToken{application: Application, readOnly: true})
}

// fakeClusters signs in to fake clusters the way the CLI's connect does:
// only with an administrator token.
type fakeClusters map[string]*fakeCluster

func (f fakeClusters) connect(ctx context.Context, token string) (*Connection, error) {
	address, credential, err := security.ParseAccessToken(token)
	if err != nil {
		return nil, err
	}
	cluster := f[address]
	if cluster == nil {
		return nil, errors.New("cluster is unreachable")
	}
	checked, err := cluster.CheckAccessToken(ctx, credential)
	if err != nil {
		return nil, err
	}
	if !checked.GetAdministrator() {
		return nil, errors.New("not an administrator token")
	}
	dashboard := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("dashboard " + cluster.name)) })
	return &Connection{Name: cluster.name, Controller: cluster, Dashboard: dashboard}, nil
}

func (f fakeClusters) with(clusters ...*fakeCluster) fakeClusters {
	for _, cluster := range clusters {
		f[cluster.address] = cluster
	}
	return f
}

type browser struct {
	t      *testing.T
	server *Server
	cookie *http.Cookie
}

func (b *browser) do(method, path string, body any, header map[string]string) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, "https://panel.example.com"+path, reader)
	request.Header.Set("X-Forwarded-Proto", "https")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "https://panel.example.com")
	}
	for key, value := range header {
		request.Header.Set(key, value)
	}
	if b.cookie != nil {
		request.AddCookie(b.cookie)
	}
	recorder := httptest.NewRecorder()
	b.server.ServeHTTP(recorder, request)
	return recorder
}

// signIn follows a sign-in link and keeps the session cookie.
func (b *browser) signIn(name, value string) *httptest.ResponseRecorder {
	response := b.do(http.MethodGet, "/login?name="+url.QueryEscape(name)+"&token="+url.QueryEscape(value), nil, nil)
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == cookieName && cookie.Value != "" {
			if !cookie.HttpOnly || !cookie.Secure {
				b.t.Fatalf("session cookie is not HttpOnly and Secure: %+v", cookie)
			}
			b.cookie = cookie
		}
	}
	return response
}

// connect posts a connect request the way orch panel connect does: without
// a session or an Origin.
func connect(server *Server, code, name, token string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"code": code, "name": name, "token": token})
	request := httptest.NewRequest(http.MethodPost, "https://panel.example.com/connect", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	return recorder
}

func TestPanelConnectsClustersAndSignsInOnce(t *testing.T) {
	now := time.Now()
	dataDir := t.TempDir()
	prod, staging := newFakeCluster("prod", "1"), newFakeCluster("staging", "2")
	clusters := fakeClusters{}.with(prod, staging)
	var logs bytes.Buffer
	options := Options{DataDir: dataDir, Connect: clusters.connect, SetupCode: "setup", URL: "https://panel.example.com", Log: &logs, Now: func() time.Time { return now }}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "orch panel connect https://panel.example.com --code setup") {
		t.Fatalf("first start logged no setup command: %s", logs.String())
	}
	visitor := &browser{t: t, server: server}
	if response := visitor.do(http.MethodGet, "/", nil, nil); response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "logs") {
		t.Fatalf("visit before any cluster = %d %s", response.Code, response.Body.String())
	}

	// The setup code connects the first cluster, and only with an
	// administrator token.
	if response := connect(server, "wrong", "", prod.admin()); response.Code != http.StatusForbidden {
		t.Fatalf("connect with a wrong code = %d", response.Code)
	}
	if response := connect(server, "setup", "", prod.login("x")); response.Code != http.StatusBadRequest {
		t.Fatalf("connect with a sign-in token = %d %s", response.Code, response.Body.String())
	}
	if response := connect(server, "setup", "", prod.admin()); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"prod"`) {
		t.Fatalf("connect with the setup code = %d %s", response.Code, response.Body.String())
	}
	if response := connect(server, "setup", "", staging.admin()); response.Code != http.StatusForbidden {
		t.Fatalf("setup code after the first cluster = %d", response.Code)
	}

	if response := visitor.do(http.MethodGet, "/api/snapshot", nil, nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("API without session = %d", response.Code)
	}
	if response := visitor.signIn(LoginTokenPrefix+"s", staging.login("s")); response.Code != http.StatusForbidden {
		t.Fatalf("sign-in from a cluster that is not connected = %d", response.Code)
	}
	link := prod.login("one")
	if response := visitor.signIn(LoginTokenPrefix+"one", link); response.Code != http.StatusSeeOther || visitor.cookie == nil {
		t.Fatalf("sign-in = %d", response.Code)
	}
	if _, ok := prod.tokens[LoginTokenPrefix+"one"]; ok {
		t.Fatal("sign-in token was not revoked")
	}
	if response := (&browser{t: t, server: server}).signIn(LoginTokenPrefix+"one", link); response.Code != http.StatusForbidden {
		t.Fatalf("reused link = %d", response.Code)
	}
	if response := visitor.do(http.MethodGet, "/api/snapshot", nil, nil); response.Body.String() != "dashboard prod" {
		t.Fatalf("dashboard with session = %d %s", response.Code, response.Body.String())
	}

	// A signed-in browser creates a connect code for the next cluster; the
	// code works once.
	if response := visitor.do(http.MethodPost, "/panel/api/clusters/code", map[string]string{}, map[string]string{"Origin": "https://evil.example"}); response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin connect code = %d", response.Code)
	}
	response := visitor.do(http.MethodPost, "/panel/api/clusters/code", map[string]string{}, nil)
	var created struct{ Code, Command string }
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || created.Code == "" ||
		created.Command != "orch panel connect https://panel.example.com --code "+created.Code {
		t.Fatalf("connect code = %d %s", response.Code, response.Body.String())
	}
	if response := connect(server, created.Code, "prod", staging.admin()); response.Code != http.StatusConflict {
		t.Fatalf("connect under a taken name = %d %s", response.Code, response.Body.String())
	}
	if response := connect(server, created.Code, "", staging.admin()); response.Code != http.StatusOK {
		t.Fatalf("connect with a code = %d %s", response.Code, response.Body.String())
	}
	if response := connect(server, created.Code, "again", newFakeCluster("other", "3").admin()); response.Code != http.StatusForbidden {
		t.Fatalf("reused connect code = %d", response.Code)
	}
	if response := visitor.do(http.MethodGet, "/api/snapshot", nil, map[string]string{clusterHeader: "staging"}); response.Body.String() != "dashboard staging" {
		t.Fatalf("dashboard for staging = %s", response.Body.String())
	}
	if response := visitor.do(http.MethodGet, "/panel/api/clusters", nil, nil); !strings.Contains(response.Body.String(), `"name":"prod","endpoint":"https://prod.example:9443"`) ||
		!strings.Contains(response.Body.String(), `"name":"staging"`) {
		t.Fatalf("clusters = %s", response.Body.String())
	}

	// The panel remembers its clusters across restarts.
	restarted, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if response := connect(restarted, "setup", "", newFakeCluster("other", "3").admin()); response.Code != http.StatusForbidden {
		t.Fatalf("setup code after a restart with clusters = %d", response.Code)
	}
	again := &browser{t: t, server: restarted}
	again.signIn(LoginTokenPrefix+"two", staging.login("two"))
	if response := again.do(http.MethodGet, "/api/snapshot", nil, map[string]string{clusterHeader: "prod"}); response.Body.String() != "dashboard prod" {
		t.Fatalf("dashboard after restart = %s", response.Body.String())
	}

	// Disconnecting forgets the cluster and revokes the panel's token on it.
	if response := again.do(http.MethodPost, "/panel/api/clusters/prod/disconnect", map[string]string{}, nil); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"revoked":true`) {
		t.Fatalf("disconnect = %d %s", response.Code, response.Body.String())
	}
	if _, ok := prod.tokens[TokenName]; ok {
		t.Fatal("disconnect left the panel's token on the cluster")
	}
	if response := again.do(http.MethodGet, "/api/snapshot", nil, map[string]string{clusterHeader: "prod"}); response.Code != http.StatusConflict {
		t.Fatalf("dashboard for a disconnected cluster = %d", response.Code)
	}

	again.do(http.MethodGet, "/logout", nil, nil)
	if response := again.do(http.MethodGet, "/api/snapshot", nil, nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("API after logout = %d", response.Code)
	}
	expiring := &browser{t: t, server: restarted}
	expiring.signIn(LoginTokenPrefix+"three", staging.login("three"))
	now = now.Add(sessionLifetime)
	if response := expiring.do(http.MethodGet, "/api/snapshot", nil, nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("API with an expired session = %d", response.Code)
	}
}

func TestPanelRejectsOtherTokens(t *testing.T) {
	prod := newFakeCluster("prod", "1")
	server, err := New(Options{Connect: fakeClusters{}.with(prod).connect, SetupCode: "setup"})
	if err != nil {
		t.Fatal(err)
	}
	if response := connect(server, "setup", "", prod.admin()); response.Code != http.StatusOK {
		t.Fatalf("connect = %d %s", response.Code, response.Body.String())
	}
	deploy := prod.add(LoginTokenPrefix+"deploy", fakeToken{application: Application})
	other := prod.add(LoginTokenPrefix+"other", fakeToken{application: "shop", readOnly: true})
	misnamed := prod.add("shop-read-only", fakeToken{application: Application, readOnly: true})
	prod.login("victim")
	for name, link := range map[string][2]string{
		"deploy token":          {LoginTokenPrefix + "deploy", deploy},
		"another app's token":   {LoginTokenPrefix + "other", other},
		"token without prefix":  {"shop-read-only", misnamed},
		"credential of another": {LoginTokenPrefix + "victim", misnamed},
		"administrator token":   {LoginTokenPrefix + "admin", prod.admin()},
		"garbage":               {LoginTokenPrefix + "x", "orch_nope"},
		"missing token":         {"", ""},
	} {
		if response := (&browser{t: t, server: server}).signIn(link[0], link[1]); response.Code != http.StatusForbidden {
			t.Fatalf("%s = %d", name, response.Code)
		}
	}
	// A credential that does not belong to the name never opens a session;
	// the named link is used up.
	if _, ok := prod.tokens[LoginTokenPrefix+"victim"]; ok {
		t.Fatal("named token survived a mismatched credential")
	}
}

func TestPanelLimitsInvalidAttempts(t *testing.T) {
	now := time.Now()
	prod := newFakeCluster("prod", "1")
	server, err := New(Options{Connect: fakeClusters{}.with(prod).connect, SetupCode: "setup", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	for range failedAttemptLimit {
		connect(server, "guess", "", prod.admin())
	}
	if response := connect(server, "setup", "", prod.admin()); response.Code != http.StatusTooManyRequests {
		t.Fatalf("connect after too many failures = %d", response.Code)
	}
	now = now.Add(time.Minute + time.Second)
	if response := connect(server, "setup", "", prod.admin()); response.Code != http.StatusOK {
		t.Fatalf("connect after a minute = %d %s", response.Code, response.Body.String())
	}
	for range failedAttemptLimit {
		(&browser{t: t, server: server}).signIn(LoginTokenPrefix+"x", "orch_nope")
	}
	link := prod.login("ok")
	if response := (&browser{t: t, server: server}).signIn(LoginTokenPrefix+"ok", link); response.Code != http.StatusTooManyRequests {
		t.Fatalf("sign-in after too many failures = %d", response.Code)
	}
}
