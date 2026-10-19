package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/buildinfo"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/control"
	"github.com/TorstenDittmann/orch/internal/discovery"
	"github.com/TorstenDittmann/orch/internal/ingress"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

// An older agent connecting to a newer controller downloads the controller's
// binary and hands it to orch node upgrade.
func TestOlderAgentUpgradesToControllerBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback controller test")
	}
	if runtime.GOOS != "linux" {
		t.Skip("the controller offers its own binary only on Linux")
	}
	previousVersion, previousAvailable, previousStart := buildinfo.Version, selfUpgradeAvailable, startNodeUpgrade
	t.Cleanup(func() {
		buildinfo.Version, selfUpgradeAvailable, startNodeUpgrade = previousVersion, previousAvailable, previousStart
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	root := t.TempDir()
	controllerDir := filepath.Join(root, "controller")
	contextPath := filepath.Join(root, "operator", "context.json")
	if _, err := clusterctl.Bootstrap(context.Background(), clusterctl.BootstrapOptions{
		Name: "test", Server: "https://" + address, DataDir: controllerDir, ContextPath: contextPath,
		Now: time.Now, Random: rand.Reader,
	}); err != nil {
		t.Fatal(err)
	}
	buildinfo.Version = "9.0.0"
	server, err := control.Start(context.Background(), control.Options{DataDir: controllerDir, Listen: address})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	agentDir := filepath.Join(root, "agent")
	if _, err := Enroll(context.Background(), EnrollOptions{
		Name: "node-1", ContextPath: contextPath, DataDir: agentDir, Now: time.Now, Random: rand.Reader,
	}); err != nil {
		t.Fatal(err)
	}

	buildinfo.Version = "8.0.0"
	selfUpgradeAvailable = func() bool { return true }
	started := make(chan string, 1)
	startNodeUpgrade = func(_ context.Context, candidate string) error {
		data, err := os.ReadFile(candidate)
		if err != nil {
			return err
		}
		select {
		case started <- string(data):
		default:
		}
		return nil
	}
	agentContext, stopAgent := context.WithCancel(context.Background())
	agentDone := make(chan error, 1)
	go func() {
		agentDone <- Serve(agentContext, ServeOptions{
			DataDir: agentDir, Snapshotter: "overlayfs", Runtime: orchruntime.NewFake(), Discovery: discovery.NewFake(),
			Ingress: &ingress.Fake{}, Now: time.Now, Random: rand.Reader, UpgradeStagingDir: filepath.Join(root, "staged"),
		})
	}()
	t.Cleanup(func() {
		stopAgent()
		<-agentDone
	})

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-started:
		if !bytes.Equal([]byte(got), want) {
			t.Fatalf("staged candidate has %d bytes, want the controller's %d", len(got), len(want))
		}
	case err := <-agentDone:
		t.Fatalf("agent stopped: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("agent did not start orch node upgrade")
	}
}
