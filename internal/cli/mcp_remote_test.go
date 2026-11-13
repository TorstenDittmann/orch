package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/control"
)

func TestRemoteMCPServesToolsWithTheApprovedAccess(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	t.Setenv("ORCH_TARGET", "")
	t.Setenv("ORCH_TOKEN", "")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	dataDir := filepath.Join(root, "state")
	contextPath := filepath.Join(root, "config", "context.json")
	if _, err := clusterctl.Bootstrap(context.Background(), clusterctl.BootstrapOptions{
		Name: "test", Server: "https://" + address, DataDir: dataDir, ContextPath: contextPath, Now: time.Now, Random: rand.Reader,
	}); err != nil {
		t.Fatal(err)
	}
	controller, err := control.Start(context.Background(), control.Options{DataDir: dataDir, Listen: address, MCPListen: "127.0.0.1:0", MCP: NewMCPRunner()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Shutdown(context.Background()) })

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"mcp", "expose", "mcp.example.test", "--context", contextPath}, &stdout, &stderr); code != ExitOK ||
		!strings.Contains(stdout.String(), "https://mcp.example.test/mcp") {
		t.Fatalf("mcp expose code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	base := "http://" + controller.MCPAddr().String()
	client := &http.Client{Timeout: 30 * time.Second}
	send := func(method, path, contentType, body string, header http.Header) (*http.Response, []byte) {
		t.Helper()
		request, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Host = "mcp.example.test"
		if contentType != "" {
			request.Header.Set("Content-Type", contentType)
		}
		for key, values := range header {
			request.Header[key] = values
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		return response, data
	}

	_, body := send(http.MethodPost, "/register", "application/json", `{"client_name":"Claude","redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}`, nil)
	var registered struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("register: %s", body)
	}
	verifier := strings.Repeat("x", 50)
	digest := sha256.Sum256([]byte(verifier))
	query := url.Values{"response_type": {"code"}, "client_id": {registered.ClientID}, "redirect_uri": {"https://claude.ai/api/mcp/auth_callback"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])}, "code_challenge_method": {"S256"}}
	_, body = send(http.MethodGet, "/authorize?"+query.Encode(), "", "", nil)
	code := regexp.MustCompile(`orch mcp approve ([A-Z0-9]{4}-[A-Z0-9]{4})`).FindStringSubmatch(string(body))
	poll := regexp.MustCompile(`var poll="([^"]+)"`).FindStringSubmatch(string(body))
	if code == nil || poll == nil {
		t.Fatalf("authorize page: %s", body)
	}
	stdout.Reset()
	if code := Run([]string{"mcp", "approve", code[1], "--context", contextPath}, &stdout, &stderr); code != ExitOK ||
		!strings.Contains(stdout.String(), "Approved Claude (returning to claude.ai)") || !strings.Contains(stdout.String(), "orch token revoke mcp-claude-") {
		t.Fatalf("mcp approve code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	_, body = send(http.MethodGet, "/authorize/status?poll="+url.QueryEscape(poll[1]), "", "", nil)
	var status struct {
		Redirect string `json:"redirect"`
	}
	_ = json.Unmarshal(body, &status)
	redirect, err := url.Parse(status.Redirect)
	if err != nil || redirect.Query().Get("code") == "" {
		t.Fatalf("status: %s", body)
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {redirect.Query().Get("code")}, "client_id": {registered.ClientID},
		"redirect_uri": {"https://claude.ai/api/mcp/auth_callback"}, "code_verifier": {verifier}}
	_, body = send(http.MethodPost, "/token", "application/x-www-form-urlencoded", form.Encode(), nil)
	var issued struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(body, &issued) != nil || issued.AccessToken == "" {
		t.Fatalf("token: %s", body)
	}
	bearer := http.Header{"Authorization": {"Bearer " + issued.AccessToken}, "Accept": {"application/json, text/event-stream"}}

	response, body := send(http.MethodPost, "/mcp", "application/json",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`, bearer)
	if response.StatusCode != http.StatusOK || response.Header.Get("Mcp-Session-Id") == "" || !strings.Contains(string(body), "remote MCP server") {
		t.Fatalf("initialize = %d %s", response.StatusCode, body)
	}
	if response, _ := send(http.MethodPost, "/mcp", "application/json", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, bearer); response.StatusCode != http.StatusAccepted {
		t.Fatalf("notification = %d", response.StatusCode)
	}
	if response, _ := send(http.MethodGet, "/mcp", "", "", bearer); response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp = %d", response.StatusCode)
	}
	_, body = send(http.MethodPost, "/mcp", "application/json", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, bearer)
	var listed struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties map[string]any `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("tools/list: %s", body)
	}
	names := map[string]bool{}
	for _, tool := range listed.Result.Tools {
		names[tool.Name] = true
		if _, ok := tool.InputSchema.Properties["target"]; ok {
			t.Fatalf("remote tool %s takes a target", tool.Name)
		}
	}
	if !names["status"] || !names["logs"] || !names["schema"] || names["deploy"] || names["rollback"] || names["exec"] || names["targets"] || names["validate"] {
		t.Fatalf("read-only remote tools = %v", names)
	}
	_, body = send(http.MethodPost, "/mcp", "application/json", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"status","arguments":{}}}`, bearer)
	var called struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &called); err != nil || called.Result.IsError || len(called.Result.Content) == 0 || !strings.Contains(called.Result.Content[0].Text, `"test"`) {
		t.Fatalf("status call: %s", body)
	}
	_, body = send(http.MethodPost, "/mcp", "application/json", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"rollback","arguments":{"app":"hello","revision":"x"}}}`, bearer)
	if !strings.Contains(string(body), "unknown tool") {
		t.Fatalf("rollback over a read-only connection: %s", body)
	}

	stdout.Reset()
	if code := Run([]string{"token", "list", "--context", contextPath}, &stdout, &stderr); code != ExitOK || !strings.Contains(stdout.String(), "MCP read (Claude)") {
		t.Fatalf("token list code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	name := regexp.MustCompile(`mcp-claude-[0-9a-f]+`).FindString(stdout.String())
	if code := Run([]string{"token", "revoke", name, "--context", contextPath}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("token revoke code=%d stderr=%s", code, stderr.String())
	}
	if response, _ := send(http.MethodPost, "/mcp", "application/json", `{"jsonrpc":"2.0","id":5,"method":"ping"}`, bearer); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("MCP after revoke = %d", response.StatusCode)
	}
	stdout.Reset()
	if code := Run([]string{"mcp", "expose", "--off", "--context", contextPath}, &stdout, &stderr); code != ExitOK || !strings.Contains(stdout.String(), "Remote MCP is off") {
		t.Fatalf("mcp expose --off code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}
