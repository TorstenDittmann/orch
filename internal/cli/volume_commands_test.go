package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	agentctl "github.com/TorstenDittmann/orch/internal/agent"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/control"
	"github.com/TorstenDittmann/orch/internal/discovery"
	"github.com/TorstenDittmann/orch/internal/ingress"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

func TestVolumeRestartAndFollowCLIThroughControllerAndAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback controller/agent")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir, path := filepath.Join(root, "controller"), filepath.Join(root, "context.json")
	if _, err := clusterctl.Bootstrap(ctx, clusterctl.BootstrapOptions{Name: "test", Server: "https://" + address, DataDir: dir, ContextPath: path, Now: time.Now, Random: rand.Reader}); err != nil {
		t.Fatal(err)
	}
	server, err := control.Start(ctx, control.Options{DataDir: dir, Listen: address, BackupKeep: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown(context.Background())
	rewriteCLIContextServer(t, path, "https://"+server.Addr().String())
	agentDir := filepath.Join(root, "agent")
	if _, err := agentctl.Enroll(ctx, agentctl.EnrollOptions{Name: "node-one", ContextPath: path, DataDir: agentDir, Now: time.Now, Random: rand.Reader}); err != nil {
		t.Fatal(err)
	}
	runtime := orchruntime.NewFake()
	runtime.Capabilities.TotalCPUMillis = 4000
	runtime.Capabilities.TotalMemoryBytes = 8 << 30
	agentCtx, stopAgent := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- agentctl.Serve(agentCtx, agentctl.ServeOptions{DataDir: agentDir, Snapshotter: "overlayfs", Runtime: runtime, Discovery: discovery.NewFake(), Ingress: &ingress.Fake{}, Now: time.Now, Random: rand.Reader})
	}()
	defer func() {
		stopAgent()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("agent did not stop")
		}
	}()
	remote, err := controlclient.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.CloseIdleConnections()
	invoke := func(ctx context.Context, want int, args ...string) []byte {
		t.Helper()
		var out, errOut bytes.Buffer
		if code := RunContext(ctx, append(args, "--context", path), &out, &errOut); code != want {
			t.Fatalf("%v: code=%d want=%d out=%s err=%s", args, code, want, out.String(), errOut.String())
		}
		return append(out.Bytes(), errOut.Bytes()...)
	}
	volumeDir := filepath.Join(agentDir, agentctl.VolumesDirectory, "game", "data")
	if err := os.MkdirAll(volumeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(volumeDir, "world.dat"), []byte("day one"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := writeManifest(t, `amends "https://raw.githubusercontent.com/TorstenDittmann/orch/main/pkl/v1alpha1/Application.pkl"
name = "game"
services {
  ["server"] {
    image = "example/game@sha256:`+strings.Repeat("a", 64)+`"
    mounts { new Mount { source = "data"; target = "/data" } }
  }
}
volumes { ["data"] { backup = new VolumeBackup { keep = 3 } } }
`)
	invoke(ctx, ExitOK, "deploy", "-f", file)
	wait := func(what string, test func() bool) {
		t.Helper()
		for !test() {
			select {
			case <-ctx.Done():
				t.Fatal("timed out waiting for " + what)
			case <-time.After(30 * time.Millisecond):
			}
		}
	}
	wait("the application", func() bool {
		app, err := remote.GetApplication(ctx, "game")
		return err == nil && app.GetExecutionStatus() == orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_RUNNING
	})
	wait("the first scheduled backup", func() bool {
		var volumes []volumeOutput
		return json.Unmarshal(invoke(ctx, ExitOK, "volume", "list", "--json"), &volumes) == nil &&
			len(volumes) == 1 && volumes[0].BackupCount == 1
	})
	if output := string(invoke(ctx, ExitOK, "volume", "list")); !strings.Contains(output, `"0 3 * * *" keep 3, 1, latest`) || !strings.Contains(output, "node-one") {
		t.Fatalf("volume list:\n%s", output)
	}
	var volume volumeOutput
	if err := json.Unmarshal(invoke(ctx, ExitOK, "volume", "backups", "game", "data", "--json"), &volume); err != nil || len(volume.Backups) != 1 {
		t.Fatalf("volume backups = %#v, %v", volume, err)
	}
	firstBackup := volume.Backups[0].Name

	if err := os.WriteFile(filepath.Join(volumeDir, "world.dat"), []byte("day two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := string(invoke(ctx, ExitOK, "volume", "backup", "game", "data")); !strings.Contains(output, "-manual") {
		t.Fatalf("volume backup: %s", output)
	}
	downloaded := filepath.Join(root, "first.tar.gz")
	invoke(ctx, ExitOK, "volume", "download", "game", "data", firstBackup, "--output", downloaded)
	if info, err := os.Stat(downloaded); err != nil || uint64(info.Size()) != volume.Backups[0].SizeBytes {
		t.Fatalf("downloaded archive = %v, %v", info, err)
	}
	invoke(ctx, ExitInvalid, "volume", "download", "game", "data", firstBackup, "--output", downloaded)

	output := string(invoke(ctx, ExitOK, "volume", "restore", "game", "data", firstBackup))
	if !strings.Contains(output, "-before-restore. Undo with: orch volume restore game data ") {
		t.Fatalf("volume restore: %s", output)
	}
	if contents, _ := os.ReadFile(filepath.Join(volumeDir, "world.dat")); string(contents) != "day one" {
		t.Fatalf("restored world = %q", contents)
	}
	wait("the restored application", func() bool {
		app, err := remote.GetApplication(ctx, "game")
		return err == nil && app.GetExecutionStatus() == orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_RUNNING
	})

	wait("the restored replica", func() bool {
		for id, object := range runtime.SnapshotObjects() {
			if object.Running {
				runtime.SetLogs(id, []orchruntime.LogFrame{{Stream: "stdout", Data: []byte("saving world\n")}})
				return true
			}
		}
		return false
	})
	if output := string(invoke(ctx, ExitOK, "restart", "game", "server")); !strings.Contains(output, "Restarted game/server.") {
		t.Fatalf("restart: %s", output)
	}
	invoke(ctx, ExitRuntime, "restart", "game", "missing")
	if output := string(invoke(ctx, ExitOK, "logs", "game", "server", "--previous")); !strings.Contains(output, "saving world") {
		t.Fatalf("logs --previous printed:\n%s", output)
	}
	invoke(ctx, ExitInvalid, "logs", "game", "server", "--previous", "-f")

	objectID := ""
	for id, object := range runtime.SnapshotObjects() {
		if object.Running {
			objectID = id
		}
	}
	runtime.SetLogs(objectID, []orchruntime.LogFrame{{Stream: "stdout", Data: []byte("player joined\n")}})
	previous := logFollowInterval
	logFollowInterval = 20 * time.Millisecond
	defer func() { logFollowInterval = previous }()
	followContext, stopFollowing := context.WithTimeout(ctx, 500*time.Millisecond)
	defer stopFollowing()
	if output := string(invoke(followContext, ExitOK, "logs", "game", "server", "-f")); strings.Count(output, "player joined") != 1 {
		t.Fatalf("logs -f printed:\n%s", output)
	}
}
