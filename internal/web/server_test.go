package web

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	"change.codes/torsten/better-swarm/internal/tui"
)

type fakeProvider struct {
	tui.Provider
	updated    *orchv1alpha1.UpdateNodeRequest
	rolledBack *orchv1alpha1.RollbackApplicationRequest
	jobAction  string
}

func (f *fakeProvider) GetCluster(context.Context) (*orchv1alpha1.Cluster, error) {
	return &orchv1alpha1.Cluster{Name: "prod", Generation: 3}, nil
}
func (f *fakeProvider) ListApplications(context.Context) ([]*orchv1alpha1.ApplicationSummary, error) {
	return []*orchv1alpha1.ApplicationSummary{{Name: "web", ExecutionStatus: orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_RUNNING}}, nil
}
func (f *fakeProvider) ListRuntimes(context.Context, uint32) (*orchv1alpha1.ListRuntimesResponse, error) {
	return &orchv1alpha1.ListRuntimesResponse{}, nil
}
func (f *fakeProvider) ListNodes(context.Context) ([]*orchv1alpha1.Node, error) {
	return []*orchv1alpha1.Node{{Name: "node-1"}}, nil
}
func (f *fakeProvider) ListRecentEvents(context.Context, uint32) (*orchv1alpha1.ListRecentEventsResponse, error) {
	return &orchv1alpha1.ListRecentEventsResponse{}, nil
}
func (f *fakeProvider) LoadLogs(_ context.Context, app, service string, replica, _ uint32, cursor string) (tui.LogBatch, error) {
	return tui.LogBatch{Lines: []tui.LogLine{{Stream: "stdout", Text: app + "/" + service + " <b>"}}, NextCursor: cursor + "x"}, nil
}
func (f *fakeProvider) UpdateNode(_ context.Context, request *orchv1alpha1.UpdateNodeRequest) (*orchv1alpha1.UpdateNodeResponse, error) {
	f.updated = request
	return &orchv1alpha1.UpdateNodeResponse{}, nil
}
func (f *fakeProvider) RollbackApplication(_ context.Context, request *orchv1alpha1.RollbackApplicationRequest) (*orchv1alpha1.ApplyApplicationResponse, error) {
	f.rolledBack = request
	return &orchv1alpha1.ApplyApplicationResponse{}, nil
}
func (f *fakeProvider) ListJobs(context.Context, string, string, uint32) (*orchv1alpha1.ListJobsResponse, error) {
	return &orchv1alpha1.ListJobsResponse{}, nil
}
func (f *fakeProvider) CancelJob(_ context.Context, id string) (*orchv1alpha1.OneOffJob, error) {
	f.jobAction = "cancel " + id
	return &orchv1alpha1.OneOffJob{Id: id}, nil
}
func (f *fakeProvider) RetryJob(_ context.Context, id, key string) (*orchv1alpha1.OneOffJob, error) {
	f.jobAction = "retry " + id
	if key == "" {
		return nil, context.Canceled
	}
	return &orchv1alpha1.OneOffJob{Id: "retried"}, nil
}
func (f *fakeProvider) ListCronRuns(_ context.Context, app, job string, _ uint32) ([]*orchv1alpha1.JobRunStatus, error) {
	return []*orchv1alpha1.JobRunStatus{{Id: "run1", Application: app, Job: job}}, nil
}
func (f *fakeProvider) CompareDeployments(_ context.Context, app, from, to string) (*orchv1alpha1.CompareDeploymentsResponse, error) {
	return &orchv1alpha1.CompareDeploymentsResponse{Changes: []*orchv1alpha1.DeploymentChange{{Path: app + ":" + from + ".." + to}}}, nil
}

const (
	testToken   = "secret-token"
	testSession = "secret-session"
)

func newTestServer(t *testing.T, provider *fakeProvider) *Server {
	t.Helper()
	server, err := New(provider, "prod", "direct", testToken)
	if err != nil {
		t.Fatal(err)
	}
	server.session = testSession
	return server
}

func request(method, target, body string, mutate func(*http.Request)) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Host = "127.0.0.1:4000"
	r.AddCookie(&http.Cookie{Name: cookieName, Value: testSession})
	if method == http.MethodPost {
		r.Header.Set("Origin", "http://127.0.0.1:4000")
		r.Header.Set("Content-Type", "application/json")
	}
	if mutate != nil {
		mutate(r)
	}
	return r
}

func serve(t *testing.T, provider *fakeProvider, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	newTestServer(t, provider).ServeHTTP(recorder, r)
	return recorder
}

func TestTokenExchangeSetsCookieOnce(t *testing.T) {
	server := newTestServer(t, &fakeProvider{})
	exchange := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/?token="+token, nil)
		r.Host = "localhost:4000"
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, r)
		return recorder
	}
	if response := exchange("wrong"); response.Code != http.StatusForbidden {
		t.Fatalf("wrong token status = %d", response.Code)
	}
	response := exchange(testToken)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", response.Code)
	}
	cookie := response.Result().Cookies()
	if len(cookie) != 1 || cookie[0].Value != testSession || !cookie[0].HttpOnly || cookie[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie = %+v", cookie)
	}
	if response := exchange(testToken); response.Code != http.StatusForbidden {
		t.Fatalf("reused token status = %d", response.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
	r.Host = "localhost:4000"
	r.AddCookie(&http.Cookie{Name: cookieName, Value: testToken})
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, r)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("token as cookie status = %d", recorder.Code)
	}
}

func TestRejectsMissingCookieAndForeignHost(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
	r.Host = "127.0.0.1:4000"
	if response := serve(t, &fakeProvider{}, r); response.Code != http.StatusUnauthorized {
		t.Fatalf("no cookie status = %d", response.Code)
	}
	rebound := request(http.MethodGet, "/api/snapshot", "", func(r *http.Request) { r.Host = "evil.example:4000" })
	if response := serve(t, &fakeProvider{}, rebound); response.Code != http.StatusForbidden {
		t.Fatalf("foreign host status = %d", response.Code)
	}
}

func TestSnapshotAndIndex(t *testing.T) {
	response := serve(t, &fakeProvider{}, request(http.MethodGet, "/api/snapshot", "", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body)
	}
	var body struct {
		Cluster      map[string]any   `json:"cluster"`
		Applications []map[string]any `json:"applications"`
		Errors       []string         `json:"errors"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Cluster["name"] != "prod" || len(body.Applications) != 1 || body.Applications[0]["executionStatus"] != "EXECUTION_STATUS_RUNNING" || len(body.Errors) != 0 {
		t.Fatalf("body = %s", response.Body)
	}
}

func TestServesAppShellAndAssets(t *testing.T) {
	for _, path := range []string{"/", "/apps/web"} {
		index := serve(t, &fakeProvider{}, request(http.MethodGet, path, "", nil))
		if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), "<title>orch</title>") || !strings.Contains(index.Body.String(), `src="/assets/`) || index.Header().Get("Content-Security-Policy") == "" {
			t.Fatalf("%s status = %d", path, index.Code)
		}
	}
	entries, err := fs.ReadDir(assets, "ui/dist/assets")
	if err != nil || len(entries) == 0 {
		t.Fatalf("embedded assets missing: %v", err)
	}
	asset := serve(t, &fakeProvider{}, request(http.MethodGet, "/assets/"+entries[0].Name(), "", nil))
	if asset.Code != http.StatusOK || !strings.Contains(asset.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset status = %d, cache = %q", asset.Code, asset.Header().Get("Cache-Control"))
	}
	if missing := serve(t, &fakeProvider{}, request(http.MethodGet, "/api/nope", "", nil)); missing.Code != http.StatusNotFound {
		t.Fatalf("unknown API status = %d", missing.Code)
	}
}

func TestCompareRunsAndJobActions(t *testing.T) {
	provider := &fakeProvider{}
	compare := serve(t, provider, request(http.MethodGet, "/api/apps/web/compare?from=a&to=b", "", nil))
	if compare.Code != http.StatusOK || !strings.Contains(compare.Body.String(), "web:a..b") {
		t.Fatalf("compare = %d: %s", compare.Code, compare.Body)
	}
	runs := serve(t, provider, request(http.MethodGet, "/api/apps/web/cron/backup/runs", "", nil))
	if runs.Code != http.StatusOK || !strings.Contains(runs.Body.String(), `"job":"backup"`) {
		t.Fatalf("runs = %d: %s", runs.Code, runs.Body)
	}
	if response := serve(t, provider, request(http.MethodPost, "/api/jobs/j1/retry", `{}`, nil)); response.Code != http.StatusOK || provider.jobAction != "retry j1" {
		t.Fatalf("retry = %d %q", response.Code, provider.jobAction)
	}
	if response := serve(t, provider, request(http.MethodPost, "/api/jobs/j1/cancel", `{}`, nil)); response.Code != http.StatusOK || provider.jobAction != "cancel j1" {
		t.Fatalf("cancel = %d %q", response.Code, provider.jobAction)
	}
	if response := serve(t, provider, request(http.MethodPost, "/api/jobs/j1/delete", `{}`, nil)); response.Code != http.StatusNotFound {
		t.Fatalf("unknown job action = %d", response.Code)
	}
}

func TestLogs(t *testing.T) {
	response := serve(t, &fakeProvider{}, request(http.MethodGet, "/api/logs?app=web&service=api&replica=0&cursor=c", "", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"nextCursor":"cx"`) || !strings.Contains(response.Body.String(), "web/api \\u003cb\\u003e") {
		t.Fatalf("status = %d: %s", response.Code, response.Body)
	}
	if response := serve(t, &fakeProvider{}, request(http.MethodGet, "/api/logs?app=web", "", nil)); response.Code != http.StatusBadRequest {
		t.Fatalf("missing service status = %d", response.Code)
	}
}

func TestMutationsRequireSameOrigin(t *testing.T) {
	provider := &fakeProvider{}
	crossSite := request(http.MethodPost, "/api/nodes/node-1", `{"action":"drain"}`, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") })
	if response := serve(t, provider, crossSite); response.Code != http.StatusForbidden || provider.updated != nil {
		t.Fatalf("cross-site status = %d", response.Code)
	}
	if response := serve(t, provider, request(http.MethodPost, "/api/nodes/node-1", `{"action":"drain"}`, nil)); response.Code != http.StatusOK {
		t.Fatalf("drain status = %d: %s", response.Code, response.Body)
	}
	if provider.updated.GetName() != "node-1" || provider.updated.GetLifecycle() != orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_DRAINING || provider.updated.GetIdempotencyKey() == "" {
		t.Fatalf("request = %v", provider.updated)
	}
	if response := serve(t, provider, request(http.MethodPost, "/api/nodes/node-1", `{"action":"remove"}`, nil)); response.Code != http.StatusBadRequest {
		t.Fatalf("unknown action status = %d", response.Code)
	}
	if response := serve(t, provider, request(http.MethodPost, "/api/apps/web/rollback", `{"revisionId":"rev1","expectedGeneration":"7"}`, nil)); response.Code != http.StatusOK {
		t.Fatalf("rollback status = %d: %s", response.Code, response.Body)
	}
	if provider.rolledBack.GetApplication() != "web" || provider.rolledBack.GetRevisionId() != "rev1" || provider.rolledBack.GetExpectedGeneration() != 7 {
		t.Fatalf("rollback = %v", provider.rolledBack)
	}
}
