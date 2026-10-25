package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/control"
)

func TestTokenLetsAnAgentUseOneApplicationWithoutATarget(t *testing.T) {
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
	controller, err := control.Start(context.Background(), control.Options{DataDir: dataDir, Listen: address})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Shutdown(context.Background()) })

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"token", "create", "--app", "hello", "--context", contextPath}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("token create code=%d stderr=%s", code, stderr.String())
	}
	deployToken := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(deployToken, "orch_") || !strings.Contains(stderr.String(), "orch token revoke hello") {
		t.Fatalf("token create stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"token", "create", "--app", "hello", "--read-only", "--ttl", "1h", "--context", contextPath}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("read-only token create code=%d stderr=%s", code, stderr.String())
	}
	viewToken := strings.TrimSpace(stdout.String())

	manifest := writeManifest(t, cliManifest)
	t.Setenv("ORCH_TOKEN", deployToken)
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"deploy", "--file", manifest, "--json"}, &stdout, &stderr); code != ExitOK || !strings.Contains(stdout.String(), `"application": "hello"`) {
		t.Fatalf("deploy with ORCH_TOKEN code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stderr.Reset()
	if code := Run([]string{"nodes"}, &stdout, &stderr); code != ExitRuntime || !strings.Contains(stderr.String(), "deployer credentials") {
		t.Fatalf("nodes with ORCH_TOKEN code=%d stderr=%s", code, stderr.String())
	}

	// orch mcp picks up the same token: reading works, deploying another app does not.
	t.Setenv("ORCH_TOKEN", viewToken)
	session := startMCPTestSession(t, newMCPServer("", mcpChanges))
	session.request("initialize", map[string]any{"protocolVersion": mcpLatestProtocol})
	if text, failed := session.call("app_status", map[string]any{"app": "hello"}); failed || !strings.Contains(text, `"name": "hello"`) {
		t.Fatalf("app_status with read-only token failed=%t: %s", failed, text)
	}
	if text, failed := session.call("deploy", map[string]any{"file": manifest}); !failed || !strings.Contains(text, "read-only") {
		t.Fatalf("deploy with read-only token failed=%t: %s", failed, text)
	}

	t.Setenv("ORCH_TOKEN", "")
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"token", "create", "--admin", "--app", "hello", "--context", contextPath}, &stdout, &stderr); code != ExitInvalid {
		t.Fatalf("token create --admin --app code=%d stderr=%s", code, stderr.String())
	}
	stderr.Reset()
	if code := Run([]string{"token", "create", "--admin", "--context", contextPath}, &stdout, &stderr); code != ExitOK ||
		!strings.Contains(stderr.String(), "administrator") || !strings.Contains(stderr.String(), "orch token revoke admin") {
		t.Fatalf("token create --admin code=%d stderr=%s", code, stderr.String())
	}
	t.Setenv("ORCH_TOKEN", strings.TrimSpace(stdout.String()))
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"nodes"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("nodes with an administrator ORCH_TOKEN code=%d stderr=%s", code, stderr.String())
	}

	t.Setenv("ORCH_TOKEN", "")
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"token", "list", "--context", contextPath}, &stdout, &stderr); code != ExitOK ||
		!strings.Contains(stdout.String(), "admin  all apps  administrator") ||
		!strings.Contains(stdout.String(), "hello  app hello  deploy") || !strings.Contains(stdout.String(), "hello-read-only  app hello  read-only") {
		t.Fatalf("token list code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := Run([]string{"token", "revoke", "hello-read-only", "--context", contextPath}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("token revoke code=%d stderr=%s", code, stderr.String())
	}
	t.Setenv("ORCH_TOKEN", viewToken)
	stderr.Reset()
	if code := Run([]string{"app", "status", "hello"}, &stdout, &stderr); code != ExitRuntime || !strings.Contains(stderr.String(), "orch token create") {
		t.Fatalf("revoked token code=%d stderr=%s", code, stderr.String())
	}
	t.Setenv("ORCH_TOKEN", "not-a-token")
	stderr.Reset()
	if code := Run([]string{"app", "status", "hello"}, &stdout, &stderr); code != ExitRuntime || !strings.Contains(stderr.String(), "not an orch access token") {
		t.Fatalf("malformed token code=%d stderr=%s", code, stderr.String())
	}
}
