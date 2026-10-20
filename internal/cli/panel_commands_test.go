package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	controlclient "github.com/TorstenDittmann/orch/internal/client"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/control"
	"github.com/TorstenDittmann/orch/internal/panel"
)

func TestPanelManifestIsValid(t *testing.T) {
	if err := config.Validate(panelManifest("panel.example.com", "ghcr.io/torstendittmann/orch-panel:1.0.0")); err != nil {
		t.Fatal(err)
	}
}

func TestPanelConnectsAClusterAndSignsIn(t *testing.T) {
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

	server, err := panel.New(panel.Options{DataDir: filepath.Join(root, "panel"), Connect: connectPanelCluster, SetupCode: "setup"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	hosted := httptest.NewServer(server)
	defer hosted.Close()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"panel", "connect", hosted.URL, "--code", "wrong", "--no-open", "--context", contextPath}, &stdout, &stderr); code != ExitRuntime || !strings.Contains(stderr.String(), "connect code") {
		t.Fatalf("connect with a wrong code=%d stderr=%s", code, stderr.String())
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.CloseIdleConnections()
	ctx := context.Background()
	if tokens, err := remote.ListAccessTokens(ctx); err != nil || len(tokens) != 0 {
		t.Fatalf("a failed connect left tokens: %+v, %v", tokens, err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"panel", "connect", hosted.URL, "--code", "setup", "--no-open", "--context", contextPath}, &stdout, &stderr); code != ExitOK ||
		!strings.Contains(stdout.String(), "as test") {
		t.Fatalf("panel connect code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	signIn := func(output string) int {
		_, rest, _ := strings.Cut(output, "Open "+hosted.URL)
		link := strings.Fields(rest)[0]
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, hosted.URL+link, nil))
		return recorder.Code
	}
	connectOutput := stdout.String()
	// A rejected code leaves the panel's working token alone.
	if code := Run([]string{"panel", "connect", hosted.URL, "--code", "setup", "--no-open", "--context", contextPath}, &stdout, &stderr); code != ExitRuntime {
		t.Fatalf("reusing the setup code = %d", code)
	}
	tokens, err := remote.ListAccessTokens(ctx)
	kept := false
	for _, token := range tokens {
		kept = kept || token.GetName() == panel.TokenName
	}
	if err != nil || !kept {
		t.Fatalf("tokens after a rejected code: %+v, %v", tokens, err)
	}
	if code := signIn(connectOutput); code != http.StatusSeeOther {
		t.Fatalf("sign-in with the link from connect = %d: %s", code, connectOutput)
	}
	if code := signIn(connectOutput); code != http.StatusForbidden {
		t.Fatalf("second sign-in with the same link = %d", code)
	}

	// orch panel open remembers the panel's URL.
	stdout.Reset()
	if code := Run([]string{"panel", "open", "--no-open", "--context", contextPath}, &stdout, &stderr); code != ExitOK || !strings.HasPrefix(stdout.String(), "Open "+hosted.URL+"/login?name="+panel.LoginTokenPrefix) {
		t.Fatalf("panel open code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if code := signIn(stdout.String()); code != http.StatusSeeOther {
		t.Fatalf("sign-in with the link from open = %d", code)
	}

	stdout.Reset()
	if code := Run([]string{"panel", "remove", "--context", contextPath}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("panel remove code=%d stderr=%s", code, stderr.String())
	}
	if tokens, err := remote.ListAccessTokens(ctx); err != nil || len(tokens) != 0 {
		t.Fatalf("access tokens after remove = %+v, %v", tokens, err)
	}

	// install deploys the panel with its setup code; without waiting it
	// prints the connect command.
	image := "ghcr.io/torstendittmann/orch-panel@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	stderr.Reset()
	if code := Run([]string{"panel", "install", "--domain", "panel.example.com", "--context", contextPath}, &stdout, &stderr); code != ExitInvalid || !strings.Contains(stderr.String(), "--image") {
		t.Fatalf("install from a development build without --image code=%d stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	if code := Run([]string{"panel", "install", "--domain", "panel.example.com", "--image", image, "--no-wait", "--context", contextPath}, &stdout, &stderr); code != ExitOK ||
		!strings.Contains(stdout.String(), "orch panel connect https://panel.example.com --code ") {
		t.Fatalf("panel install code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if secrets, err := remote.ListSecrets(ctx, panelApplication); err != nil || len(secrets) != 1 {
		t.Fatalf("panel secrets = %+v, %v", secrets, err)
	}
	if app, err := remote.GetApplication(ctx, panelApplication); err != nil || app.GetGeneration() == 0 {
		t.Fatalf("panel application = %+v, %v", app, err)
	}
	if code := Run([]string{"panel", "remove", "--context", contextPath}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("panel remove after install code=%d stderr=%s", code, stderr.String())
	}
}
