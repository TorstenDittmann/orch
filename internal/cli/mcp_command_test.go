package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/control"
)

type mcpTestSession struct {
	t      *testing.T
	input  *io.PipeWriter
	output *bufio.Scanner
	done   chan error
	nextID int
}

func startMCPTestSession(t *testing.T, server *mcpServer) *mcpTestSession {
	t.Helper()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	session := &mcpTestSession{t: t, input: inputWriter, output: bufio.NewScanner(outputReader), done: make(chan error, 1)}
	session.output.Buffer(make([]byte, 0, 64<<10), 4<<20)
	go func() {
		session.done <- server.serve(context.Background(), inputReader, outputWriter)
		_ = outputWriter.Close()
	}()
	t.Cleanup(func() {
		_ = inputWriter.Close()
		select {
		case <-session.done:
		case <-time.After(10 * time.Second):
			t.Error("mcp server did not stop at end of input")
		}
	})
	return session
}

// request sends one request and returns its response.
func (s *mcpTestSession) request(method string, params any) map[string]any {
	s.t.Helper()
	s.nextID++
	message := map[string]any{"jsonrpc": "2.0", "id": s.nextID, "method": method}
	if params != nil {
		message["params"] = params
	}
	encoded, _ := json.Marshal(message)
	if _, err := s.input.Write(append(encoded, '\n')); err != nil {
		s.t.Fatal(err)
	}
	if !s.output.Scan() {
		s.t.Fatalf("no response to %s: %v", method, s.output.Err())
	}
	var response map[string]any
	if err := json.Unmarshal(s.output.Bytes(), &response); err != nil {
		s.t.Fatalf("response %q: %v", s.output.Text(), err)
	}
	if response["id"] != float64(s.nextID) || response["jsonrpc"] != "2.0" {
		s.t.Fatalf("response = %v", response)
	}
	return response
}

// call runs a tool and returns its text and whether it failed.
func (s *mcpTestSession) call(name string, arguments map[string]any) (string, bool) {
	s.t.Helper()
	response := s.request("tools/call", map[string]any{"name": name, "arguments": arguments})
	result, ok := response["result"].(map[string]any)
	if !ok {
		s.t.Fatalf("%s response = %v", name, response)
	}
	content := result["content"].([]any)[0].(map[string]any)
	return content["text"].(string), result["isError"].(bool)
}

func TestMCPHandshakeAndToolList(t *testing.T) {
	session := startMCPTestSession(t, newMCPServer("", false))
	initialized := session.request("initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}})
	result := initialized["result"].(map[string]any)
	if result["protocolVersion"] != "2025-03-26" || result["serverInfo"].(map[string]any)["name"] != "orch" || result["instructions"] == "" {
		t.Fatalf("initialize = %v", result)
	}
	if _, err := session.input.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if response := session.request("ping", nil); response["error"] != nil {
		t.Fatalf("ping = %v", response)
	}
	if response := session.request("resources/list", nil); response["error"].(map[string]any)["code"] != float64(-32601) {
		t.Fatalf("unknown method = %v", response)
	}

	tools := session.request("tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	byName := map[string]map[string]any{}
	for _, tool := range tools {
		definition := tool.(map[string]any)
		byName[definition["name"].(string)] = definition
	}
	for _, name := range []string{"status", "list_apps", "app_status", "logs", "deploy", "rollback", "restart", "set_secret", "list_volumes", "restore_volume", "run_cron", "schema", "validate"} {
		if byName[name] == nil {
			t.Fatalf("tool %s missing from %d tools", name, len(tools))
		}
	}
	annotations := func(name string) map[string]any { return byName[name]["annotations"].(map[string]any) }
	if annotations("app_status")["readOnlyHint"] != true || annotations("app_status")["destructiveHint"] != false {
		t.Fatalf("app_status annotations = %v", annotations("app_status"))
	}
	for _, name := range []string{"remove_app", "restore_volume", "delete_secret", "exec"} {
		if annotations(name)["destructiveHint"] != true || annotations(name)["readOnlyHint"] != false {
			t.Fatalf("%s annotations = %v", name, annotations(name))
		}
	}
	if annotations("deploy")["readOnlyHint"] != false || annotations("deploy")["destructiveHint"] != false {
		t.Fatalf("deploy annotations = %v", annotations("deploy"))
	}
	schema := byName["app_status"]["inputSchema"].(map[string]any)
	if !reflect.DeepEqual(schema["required"], []any{"app"}) || schema["properties"].(map[string]any)["target"] == nil {
		t.Fatalf("app_status schema = %v", schema)
	}
	if byName["schema"]["inputSchema"].(map[string]any)["properties"].(map[string]any)["target"] != nil {
		t.Fatal("local tool offers a target")
	}

	readOnly := newMCPServer("", true)
	if readOnly.byName["status"].name == "" || readOnly.byName["deploy"].name != "" || readOnly.byName["remove_app"].name != "" {
		t.Fatalf("read-only tools = %d", len(readOnly.tools))
	}
	for _, tool := range readOnly.tools {
		if !tool.readOnly || tool.destructive {
			t.Fatalf("read-only server offers %s", tool.name)
		}
	}
}

func TestMCPToolsBuildCommandLines(t *testing.T) {
	server := newMCPServer("production", false)
	var got []string
	var input string
	server.run = func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
		got = args
		value, _ := io.ReadAll(secretInput(ctx))
		input = string(value)
		_, _ = io.WriteString(stdout, "ok\n")
		return ExitOK
	}
	cases := []struct {
		tool      string
		arguments map[string]any
		want      []string
		input     string
	}{
		{"logs", map[string]any{"app": "shop", "service": "web", "tail": float64(20)},
			[]string{"logs", "--tail=20", "shop", "web", "--target", "production"}, ""},
		{"exec", map[string]any{"app": "shop", "service": "web", "command": []any{"ls", "-la"}, "target": "staging"},
			[]string{"exec", "shop", "web", "--target", "staging", "--", "ls", "-la"}, ""},
		{"deploy", map[string]any{"file": "app/orch.pkl", "wait": true},
			[]string{"deploy", "--file=app/orch.pkl", "--wait", "--json", "--target", "production"}, ""},
		{"set_secret", map[string]any{"app": "shop", "name": "api-token", "value": "s3cret"},
			[]string{"secret", "set", "--from-stdin", "shop", "api-token", "--json", "--target", "production"}, "s3cret"},
		{"update_node", map[string]any{"node": "worker-1", "action": "drain"},
			[]string{"node", "drain", "worker-1", "--json", "--target", "production"}, ""},
		{"schema", map[string]any{}, []string{"schema"}, ""},
	}
	for _, test := range cases {
		got, input = nil, ""
		text, failed := server.invoke(context.Background(), server.byName[test.tool], mcpArgs(test.arguments))
		if failed || text != "ok" || !reflect.DeepEqual(got, test.want) || input != test.input {
			t.Fatalf("%s: args=%q input=%q text=%q failed=%t", test.tool, got, input, text, failed)
		}
		if strings.Contains(strings.Join(got, " "), "s3cret") {
			t.Fatalf("%s put the secret on the command line", test.tool)
		}
	}
	for _, arguments := range []map[string]any{{}, {"app": "--context=/tmp/x"}, {"app": 7}} {
		got = nil
		if text, failed := server.invoke(context.Background(), server.byName["app_status"], mcpArgs(arguments)); !failed || got != nil {
			t.Fatalf("app_status(%v) ran %q: %s", arguments, got, text)
		}
	}
	if text, failed := server.invoke(context.Background(), server.byName["update_node"], mcpArgs{"node": "a", "action": "remove"}); !failed || got != nil {
		t.Fatalf("update_node accepted remove: %s", text)
	}
}

func TestMCPDrivesARealCluster(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	t.Setenv("ORCH_TARGET", "")
	dataDir := filepath.Join(root, "state")
	contextPath := filepath.Join(root, "config", "context.json")
	if _, err := clusterctl.Bootstrap(context.Background(), clusterctl.BootstrapOptions{
		Name: "test", Server: "https://127.0.0.1", DataDir: dataDir,
		ContextPath: contextPath, Now: time.Now, Random: rand.Reader,
	}); err != nil {
		t.Fatal(err)
	}
	controller, err := control.Start(context.Background(), control.Options{DataDir: dataDir, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Shutdown(context.Background()) })
	rewriteCLIContextServer(t, contextPath, "https://"+controller.Addr().String())
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"target", "import", "test", contextPath, "--use"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("target import code=%d stderr=%s", code, stderr.String())
	}
	manifest := writeManifest(t, cliManifest)

	session := startMCPTestSession(t, newMCPServer("", false))
	session.request("initialize", map[string]any{"protocolVersion": mcpLatestProtocol})
	if text, failed := session.call("status", nil); failed || !strings.Contains(text, `"name": "test"`) {
		t.Fatalf("status failed=%t: %s", failed, text)
	}
	if text, failed := session.call("validate", map[string]any{"file": manifest}); failed || !strings.Contains(text, "hello") {
		t.Fatalf("validate failed=%t: %s", failed, text)
	}
	if text, failed := session.call("list_apps", nil); failed || !strings.Contains(text, `"applications": []`) {
		t.Fatalf("list_apps before deploy failed=%t: %s", failed, text)
	}
	if text, failed := session.call("deploy", map[string]any{"file": manifest}); failed || !strings.Contains(text, `"application": "hello"`) {
		t.Fatalf("deploy failed=%t: %s", failed, text)
	}
	if text, failed := session.call("list_apps", nil); failed || !strings.Contains(text, `"name": "hello"`) {
		t.Fatalf("list_apps failed=%t: %s", failed, text)
	}
	if text, failed := session.call("app_status", map[string]any{"app": "hello"}); failed || !strings.Contains(text, `"name": "hello"`) {
		t.Fatalf("app_status failed=%t: %s", failed, text)
	}
	if text, failed := session.call("set_secret", map[string]any{"app": "hello", "name": "api-token", "value": "do-not-echo"}); failed || strings.Contains(text, "do-not-echo") {
		t.Fatalf("set_secret failed=%t: %s", failed, text)
	}
	if text, failed := session.call("list_secrets", map[string]any{"app": "hello"}); failed || !strings.Contains(text, "api-token") || strings.Contains(text, "do-not-echo") {
		t.Fatalf("list_secrets failed=%t: %s", failed, text)
	}
	if text, failed := session.call("app_status", map[string]any{"app": "missing"}); !failed || text == "" {
		t.Fatalf("app_status of a missing app failed=%t: %s", failed, text)
	}
	if text, failed := session.call("status", map[string]any{"target": "nope"}); !failed || !strings.Contains(text, "nope") {
		t.Fatalf("status with an unknown target failed=%t: %s", failed, text)
	}
}

func TestMCPCommandRejectsArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"mcp", "extra"}, &stdout, &stderr); code != ExitInvalid {
		t.Fatalf("code = %d", code)
	}
}
