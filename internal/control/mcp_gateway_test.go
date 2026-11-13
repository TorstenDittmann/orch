package control

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
	"github.com/TorstenDittmann/orch/internal/model"
)

// recordingRunner answers each MCP request by calling the in-process API,
// once with a procedure every level may use and once with one none may.
type recordingRunner struct{}

func (recordingRunner) ServeMCP(response http.ResponseWriter, request *http.Request, session MCPSession) {
	reference, release := controlclient.RegisterInProcess("test", session.Dial)
	defer release()
	remote, err := controlclient.New(reference)
	if err != nil {
		http.Error(response, err.Error(), http.StatusInternalServerError)
		return
	}
	defer remote.CloseIdleConnections()
	result := map[string]string{"access": session.Access, "name": session.Name}
	if cluster, _, err := remote.GetClusterStatus(request.Context()); err != nil {
		result["read"] = err.Error()
	} else {
		result["read"] = "ok " + cluster.GetName()
	}
	_, err = remote.DeleteAccessToken(request.Context(), "anything")
	result["admin"] = connect.CodeOf(err).String()
	_, err = remote.SetSecret(request.Context(), "hello", "KEY", []byte("v"), "")
	result["secret"] = connect.CodeOf(err).String()
	writeJSON(response, http.StatusOK, result)
}

type mcpTestClient struct {
	t      *testing.T
	base   string
	host   string
	client *http.Client
}

func (c mcpTestClient) do(method, path, contentType string, body io.Reader, header http.Header) (*http.Response, []byte) {
	c.t.Helper()
	request, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	request.Host = c.host
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	for key, values := range header {
		request.Header[key] = values
	}
	response, err := c.client.Do(request)
	if err != nil {
		c.t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response, data
}

func TestRemoteMCPConnectionIsApprovedScopedAndRevocable(t *testing.T) {
	_, dataDir, contextPath, _ := bootstrapController(t)
	server, err := Start(context.Background(), Options{DataDir: dataDir, Listen: "127.0.0.1:0", MCPListen: "127.0.0.1:0", MCP: recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	rewriteContextServer(t, contextPath, "https://"+server.Addr().String())
	admin, err := controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.CloseIdleConnections)
	ctx := context.Background()
	gateway := mcpTestClient{t: t, base: "http://" + server.MCPAddr().String(), host: "mcp.example.test", client: &http.Client{Timeout: 10 * time.Second}}

	// Off by default: nothing answers.
	if response, _ := gateway.do(http.MethodGet, "/.well-known/oauth-protected-resource", "", nil, nil); response.StatusCode != http.StatusNotFound {
		t.Fatalf("metadata while off = %d", response.StatusCode)
	}
	if _, err := admin.SetMCPDomain(ctx, "*.example.test"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("wildcard domain = %v", err)
	}
	if changed, err := admin.SetMCPDomain(ctx, "MCP.example.test"); err != nil || !changed {
		t.Fatalf("SetMCPDomain() = %v, %v", changed, err)
	}
	if domain, err := admin.MCPDomain(ctx); err != nil || domain != "mcp.example.test" {
		t.Fatalf("MCPDomain() = %q, %v", domain, err)
	}
	other := gateway
	other.host = "other.example.test"
	if response, _ := other.do(http.MethodGet, "/.well-known/oauth-protected-resource", "", nil, nil); response.StatusCode != http.StatusNotFound {
		t.Fatalf("metadata for another host = %d", response.StatusCode)
	}

	response, body := gateway.do(http.MethodPost, "/mcp", "application/json", strings.NewReader(`{}`), nil)
	if response.StatusCode != http.StatusUnauthorized || !strings.Contains(response.Header.Get("WWW-Authenticate"), `resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource"`) {
		t.Fatalf("unauthenticated MCP = %d %q", response.StatusCode, response.Header.Get("WWW-Authenticate"))
	}
	response, body = gateway.do(http.MethodGet, "/.well-known/oauth-authorization-server", "", nil, nil)
	var metadata map[string]any
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &metadata) != nil || metadata["token_endpoint"] != "https://mcp.example.test/token" {
		t.Fatalf("authorization server metadata = %d %s", response.StatusCode, body)
	}

	if response, _ := gateway.do(http.MethodPost, "/register", "application/json", strings.NewReader(`{"client_name":"Claude","redirect_uris":["http://evil.example/cb"]}`), nil); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("plain HTTP redirect registered: %d", response.StatusCode)
	}
	response, body = gateway.do(http.MethodPost, "/register", "application/json", strings.NewReader(`{"client_name":"Claude","redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}`), nil)
	var registered struct {
		ClientID string `json:"client_id"`
	}
	if response.StatusCode != http.StatusCreated || json.Unmarshal(body, &registered) != nil || registered.ClientID == "" {
		t.Fatalf("register = %d %s", response.StatusCode, body)
	}

	verifier := strings.Repeat("v", 64)
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	authorize := func(redirect string) (*http.Response, []byte) {
		query := url.Values{"response_type": {"code"}, "client_id": {registered.ClientID}, "redirect_uri": {redirect},
			"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"xyz"}, "resource": {"https://mcp.example.test/mcp"}}
		return gateway.do(http.MethodGet, "/authorize?"+query.Encode(), "", nil, nil)
	}
	if response, _ := authorize("https://evil.example/cb"); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unregistered redirect = %d", response.StatusCode)
	}
	response, body = authorize("https://claude.ai/api/mcp/auth_callback")
	page := string(body)
	code := regexp.MustCompile(`orch mcp approve ([A-Z0-9]{4}-[A-Z0-9]{4})`).FindStringSubmatch(page)
	poll := regexp.MustCompile(`var poll="([^"]+)"`).FindStringSubmatch(page)
	if response.StatusCode != http.StatusOK || code == nil || poll == nil || !strings.Contains(page, "claude.ai") ||
		!strings.Contains(response.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("authorize page = %d %s", response.StatusCode, page)
	}
	status := func() map[string]string {
		_, body := gateway.do(http.MethodGet, "/authorize/status?poll="+url.QueryEscape(poll[1]), "", nil, nil)
		var value map[string]string
		if err := json.Unmarshal(body, &value); err != nil {
			t.Fatalf("status %s: %v", body, err)
		}
		return value
	}
	if value := status(); value["status"] != "pending" {
		t.Fatalf("status before approval = %v", value)
	}
	if _, err := admin.ApproveMCPConnection(ctx, "AAAA-AAAA", "", "", 0); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown code = %v", err)
	}
	approved, err := admin.ApproveMCPConnection(ctx, strings.ToLower(strings.ReplaceAll(code[1], "-", "")), "", "", 0)
	if err != nil || approved.GetConnection().GetMcpAccess() != model.MCPAccessRead || approved.GetRedirectHost() != "claude.ai" ||
		!strings.HasPrefix(approved.GetConnection().GetName(), "mcp-claude-") {
		t.Fatalf("ApproveMCPConnection() = %+v, %v", approved, err)
	}
	if _, err := admin.ApproveMCPConnection(ctx, code[1], "", "", 0); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("second approval = %v", err)
	}
	value := status()
	redirect, err := url.Parse(value["redirect"])
	if value["status"] != "approved" || err != nil || redirect.Host != "claude.ai" || redirect.Query().Get("state") != "xyz" ||
		redirect.Query().Get("iss") != "https://mcp.example.test" {
		t.Fatalf("status after approval = %v", value)
	}
	authCode := redirect.Query().Get("code")

	token := func(form url.Values) (int, map[string]any) {
		response, body := gateway.do(http.MethodPost, "/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), nil)
		var value map[string]any
		_ = json.Unmarshal(body, &value)
		return response.StatusCode, value
	}
	exchange := url.Values{"grant_type": {"authorization_code"}, "code": {authCode}, "client_id": {registered.ClientID},
		"redirect_uri": {"https://claude.ai/api/mcp/auth_callback"}, "code_verifier": {strings.Repeat("w", 64)}}
	if status, _ := token(exchange); status != http.StatusBadRequest {
		t.Fatalf("wrong PKCE verifier = %d", status)
	}
	// A failed attempt burns the code, as with a stolen one.
	exchange.Set("code_verifier", verifier)
	if status, _ := token(exchange); status != http.StatusBadRequest {
		t.Fatalf("reused code = %d", status)
	}

	// Start over and redeem properly.
	_, body = authorize("https://claude.ai/api/mcp/auth_callback")
	code = regexp.MustCompile(`orch mcp approve ([A-Z0-9]{4}-[A-Z0-9]{4})`).FindStringSubmatch(string(body))
	poll = regexp.MustCompile(`var poll="([^"]+)"`).FindStringSubmatch(string(body))
	if _, err := admin.ApproveMCPConnection(ctx, code[1], model.MCPAccessDeploy, "", 0); err != nil {
		t.Fatal(err)
	}
	redirect, _ = url.Parse(status()["redirect"])
	exchange.Set("code", redirect.Query().Get("code"))
	statusCode, issued := token(exchange)
	access, _ := issued["access_token"].(string)
	refresh, _ := issued["refresh_token"].(string)
	if statusCode != http.StatusOK || access == "" || refresh == "" || issued["scope"] != model.MCPAccessDeploy {
		t.Fatalf("token = %d %v", statusCode, issued)
	}

	call := func(bearer string) (int, map[string]string) {
		response, body := gateway.do(http.MethodPost, "/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`),
			http.Header{"Authorization": {"Bearer " + bearer}})
		var value map[string]string
		_ = json.Unmarshal(body, &value)
		return response.StatusCode, value
	}
	statusCode, result := call(access)
	if statusCode != http.StatusOK || result["access"] != model.MCPAccessDeploy || !strings.HasPrefix(result["read"], "ok ") ||
		result["admin"] != connect.CodePermissionDenied.String() || result["secret"] != connect.CodePermissionDenied.String() {
		t.Fatalf("MCP call = %d %v", statusCode, result)
	}
	if response, _ := gateway.do(http.MethodPost, "/mcp", "application/json", strings.NewReader(`{}`),
		http.Header{"Authorization": {"Bearer " + access}, "Origin": {"https://evil.example"}}); response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin MCP = %d", response.StatusCode)
	}

	refreshForm := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {registered.ClientID}}
	statusCode, refreshed := token(refreshForm)
	if statusCode != http.StatusOK || refreshed["refresh_token"] == refresh {
		t.Fatalf("refresh = %d %v", statusCode, refreshed)
	}
	if statusCode, _ := token(refreshForm); statusCode != http.StatusBadRequest {
		t.Fatalf("reused refresh token = %d", statusCode)
	}

	listed, err := admin.ListAccessTokens(ctx)
	var names []string
	for _, item := range listed {
		if item.GetMcpAccess() != "" {
			names = append(names, item.GetName()+"="+item.GetMcpAccess())
		}
	}
	if err != nil || len(names) != 2 {
		t.Fatalf("listed connections = %v, %v", names, err)
	}
	var deployName string
	for _, item := range listed {
		if item.GetMcpAccess() == model.MCPAccessDeploy {
			deployName = item.GetName()
		}
	}
	if changed, err := admin.DeleteAccessToken(ctx, deployName); err != nil || !changed {
		t.Fatalf("revoke = %v, %v", changed, err)
	}
	if statusCode, _ := call(refreshed["access_token"].(string)); statusCode != http.StatusUnauthorized {
		t.Fatalf("MCP call after revoke = %d", statusCode)
	}

	// An ORCH_TOKEN works as a bearer token too, with its own limits.
	created, err := admin.CreateAccessToken(ctx, "hello-read-only", "hello", true, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	statusCode, result = call(created.GetValue())
	if statusCode != http.StatusOK || result["access"] != model.MCPAccessRead || result["read"] == "" || strings.HasPrefix(result["read"], "ok ") {
		t.Fatalf("ORCH_TOKEN MCP call = %d %v", statusCode, result)
	}

	if _, err := admin.SetMCPDomain(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if response, _ := gateway.do(http.MethodGet, "/.well-known/oauth-protected-resource", "", nil, nil); response.StatusCode != http.StatusNotFound {
		t.Fatalf("metadata after turning off = %d", response.StatusCode)
	}
}

func TestMCPProceduresGrowWithAccess(t *testing.T) {
	read, deploy, destructive := mcpProcedures(model.MCPAccessRead), mcpProcedures(model.MCPAccessDeploy), mcpProcedures(model.MCPAccessDestructive)
	if len(mcpProcedures("admin")) != 0 {
		t.Fatal("unknown access level allows procedures")
	}
	for procedure := range read {
		if !deploy[procedure] || !destructive[procedure] {
			t.Fatalf("%s is readable but not at higher levels", procedure)
		}
	}
	for procedure := range deploy {
		if !destructive[procedure] {
			t.Fatalf("%s is deployable but not destructive", procedure)
		}
	}
	for _, procedure := range []string{"/orch.v1alpha1.ControlService/CreateAccessToken", "/orch.v1alpha1.ControlService/ApproveMCPConnection",
		"/orch.v1alpha1.ControlService/SetMCPDomain", "/orch.v1alpha1.ControlService/RemoveNode", "/orch.v1alpha1.ControlService/ApplyApplication"} {
		if destructive[procedure] {
			t.Fatalf("%s is reachable over MCP", procedure)
		}
	}
}

func TestMCPGrantNames(t *testing.T) {
	for client, want := range map[string]string{"Claude": "mcp-claude-ab12", "Claude Desktop (beta)": "mcp-claude-desktop-beta-ab12", "!!!": "mcp-ab12"} {
		if got := mcpGrantName(client, "ab12"); got != want {
			t.Errorf("mcpGrantName(%q) = %q, want %q", client, got, want)
		}
	}
}

func TestMCPDomainNeedsAServingGateway(t *testing.T) {
	_, dataDir, contextPath, _ := bootstrapController(t)
	server, err := Start(context.Background(), Options{DataDir: dataDir, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	rewriteContextServer(t, contextPath, "https://"+server.Addr().String())
	admin, err := controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.CloseIdleConnections)
	if _, err := admin.SetMCPDomain(context.Background(), "mcp.example.test"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expose without a gateway = %v", err)
	}
	if _, err := admin.SetMCPDomain(context.Background(), ""); err != nil {
		t.Fatalf("turning off without a gateway = %v", err)
	}
}
