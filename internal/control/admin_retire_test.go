package control

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
)

func TestCertificateRenewalRetiresEarlierAdministratorContexts(t *testing.T) {
	ctx := context.Background()
	root, dataDir, oldContext, _ := bootstrapController(t)
	start := func() *Server {
		t.Helper()
		server, err := Start(ctx, Options{DataDir: dataDir, Listen: "127.0.0.1:0"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
		endpoint := "https://" + server.Addr().String()
		rewriteContextServer(t, oldContext, endpoint)
		if newContext := filepath.Join(root, "renewed", "context.json"); fileExistsForTest(newContext) {
			rewriteContextServer(t, newContext, endpoint)
		}
		return server
	}
	status := func(contextPath string) error {
		t.Helper()
		admin, err := controlclient.New(contextPath)
		if err != nil {
			t.Fatal(err)
		}
		defer admin.CloseIdleConnections()
		_, _, err = admin.GetClusterStatus(ctx)
		return err
	}

	first := start()
	if err := status(oldContext); err != nil {
		t.Fatalf("original context: %v", err)
	}
	if err := first.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	renewed, err := clusterctl.RenewCertificates(ctx, clusterctl.RenewCertificatesOptions{
		DataDir: dataDir, AdminDir: filepath.Join(root, "renewed"), Now: func() time.Time { return time.Now().Add(time.Minute) },
	})
	if err != nil {
		t.Fatal(err)
	}
	start()
	if err := status(oldContext); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("retired context = %v, want permission denied", err)
	}
	if err := status(renewed.ContextPath); err != nil {
		t.Fatalf("renewed context: %v", err)
	}
}

func fileExistsForTest(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
