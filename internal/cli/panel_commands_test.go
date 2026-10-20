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

	controlclient "github.com/TorstenDittmann/orch/internal/client"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/control"
)

func TestPanelManifestIsValid(t *testing.T) {
	if err := config.Validate(panelManifest("panel.example.com", "ghcr.io/torstendittmann/orch-panel:1.0.0")); err != nil {
		t.Fatal(err)
	}
}

func TestPanelInstallDeploysWithAnAdministratorToken(t *testing.T) {
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
	image := "ghcr.io/torstendittmann/orch-panel@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	if code := Run([]string{"panel", "install", "--domain", "panel.example.com", "--context", contextPath}, &stdout, &stderr); code != ExitInvalid || !strings.Contains(stderr.String(), "--image") {
		t.Fatalf("install from a development build without --image code=%d stderr=%s", code, stderr.String())
	}
	for range 2 {
		stdout.Reset()
		stderr.Reset()
		if code := Run([]string{"panel", "install", "--domain", "panel.example.com", "--image", image, "--no-wait", "--context", contextPath}, &stdout, &stderr); code != ExitOK {
			t.Fatalf("panel install code=%d stderr=%s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "https://panel.example.com/setup?token=") {
			t.Fatalf("panel install printed no setup link: %s", stdout.String())
		}
	}

	remote, err := controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.CloseIdleConnections()
	ctx := context.Background()
	tokens, err := remote.ListAccessTokens(ctx)
	if err != nil || len(tokens) != 1 || tokens[0].GetName() != panelTokenName || !tokens[0].GetAdministrator() {
		t.Fatalf("access tokens after installing twice = %+v, %v", tokens, err)
	}
	secrets, err := remote.ListSecrets(ctx, panelApplication)
	if err != nil || len(secrets) != 2 {
		t.Fatalf("panel secrets = %+v, %v", secrets, err)
	}
	app, err := remote.GetApplication(ctx, panelApplication)
	if err != nil || app.GetGeneration() == 0 {
		t.Fatalf("panel application = %+v, %v", app, err)
	}

	stdout.Reset()
	if code := Run([]string{"panel", "remove", "--context", contextPath}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("panel remove code=%d stderr=%s", code, stderr.String())
	}
	if tokens, err := remote.ListAccessTokens(ctx); err != nil || len(tokens) != 0 {
		t.Fatalf("access tokens after remove = %+v, %v", tokens, err)
	}
}
