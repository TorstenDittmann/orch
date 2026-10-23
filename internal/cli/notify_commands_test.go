package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/control"
)

func TestNotifyAddsTestsListsAndRemovesClusterNotifications(t *testing.T) {
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
	var mu sync.Mutex
	var messages []string
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Text string }
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		messages = append(messages, body.Text)
	}))
	defer slack.Close()

	var stdout, stderr bytes.Buffer
	ctx := withCommandInput(context.Background(), strings.NewReader(slack.URL+"\n"))
	if code := RunContext(ctx, []string{"notify", "add", "slack", "--on", "node.lost,deploy.failed", "--context", contextPath}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("notify add code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Added notification slack for deploy.failed, node.lost.") || !strings.Contains(stdout.String(), "Sent a test message.") {
		t.Fatalf("notify add stdout=%q", stdout.String())
	}
	mu.Lock()
	if len(messages) != 1 || messages[0] != "[test] test notification from orch; slack is set up" {
		t.Fatalf("messages = %q", messages)
	}
	mu.Unlock()

	stdout.Reset()
	if code := Run([]string{"notify", "add", "webhook", "http://127.0.0.1:1/hook", "--name", "broken", "--context", contextPath}, &stdout, &stderr); code != ExitRuntime ||
		!strings.Contains(stdout.String(), "The test message failed") {
		t.Fatalf("broken notify add code=%d stdout=%q", code, stdout.String())
	}
	stdout.Reset()
	if code := Run([]string{"notify", "list", "--context", contextPath}, &stdout, &stderr); code != ExitOK ||
		stdout.String() != "broken  webhook  backup.stale,certificate.expiring,node.disk_low,node.lost\nslack  slack  deploy.failed,node.lost\n" {
		t.Fatalf("notify list code=%d stdout=%q", code, stdout.String())
	}
	stdout.Reset()
	if code := Run([]string{"notify", "test", "--context", contextPath}, &stdout, &stderr); code != ExitRuntime ||
		!strings.Contains(stdout.String(), "broken: failed:") || !strings.Contains(stdout.String(), "slack: sent") {
		t.Fatalf("notify test code=%d stdout=%q", code, stdout.String())
	}
	stdout.Reset()
	if code := Run([]string{"notify", "remove", "broken", "--context", contextPath}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("notify remove code=%d stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	if code := Run([]string{"notify", "test", "--context", contextPath}, &stdout, &stderr); code != ExitOK || stdout.String() != "slack: sent\n" {
		t.Fatalf("notify test after remove code=%d stdout=%q", code, stdout.String())
	}
}
