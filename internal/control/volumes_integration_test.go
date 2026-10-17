package control

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	agentctl "github.com/TorstenDittmann/orch/internal/agent"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/discovery"
	"github.com/TorstenDittmann/orch/internal/ingress"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

func TestLoopbackVolumeBackupsRestoreDownloadAndRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback lifecycle test")
	}
	ctx := context.Background()
	address := reserveLoopbackAddress(t)
	root := t.TempDir()
	controllerDir := filepath.Join(root, "controller")
	contextPath := filepath.Join(root, "operator", "context.json")
	if _, err := clusterctl.Bootstrap(ctx, clusterctl.BootstrapOptions{
		Name: "test", Server: "https://" + address, DataDir: controllerDir, ContextPath: contextPath,
		Now: time.Now, Random: rand.Reader,
	}); err != nil {
		t.Fatal(err)
	}
	server, err := Start(ctx, Options{DataDir: controllerDir, Listen: address, BackupKeep: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	remote, err := controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.CloseIdleConnections()
	if _, err := remote.ConfigureNetwork(ctx, "10.96.0.0/16", "volume-network-key"); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(root, "agent")
	if _, err := agentctl.Enroll(ctx, agentctl.EnrollOptions{
		Name: "node-1", ContextPath: contextPath, DataDir: agentDir, Now: time.Now, Random: rand.Reader,
	}); err != nil {
		t.Fatal(err)
	}
	manifest := newTestManifest("game", map[string]config.Service{
		"server": {
			Image: executableImage, Command: []string{"/bin/sleep"}, Args: []string{"3600"}, Restart: "always",
			Mounts: []config.Mount{{Source: "data", Target: "/data"}},
		},
	})
	manifest.Volumes = map[string]config.Volume{"data": {Driver: "local", Backup: &config.VolumeBackup{Schedule: "0 3 * * *", Keep: 7}}}
	if _, err := remote.ApplyApplication(ctx, mustCanonicalConfig(t, manifest), "volume-deploy-key"); err != nil {
		t.Fatal(err)
	}
	// The fake runtime does not create volume directories; the agent would.
	volumeDir := filepath.Join(agentDir, agentctl.VolumesDirectory, "game", "data")
	if err := os.MkdirAll(volumeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	world := make([]byte, 3<<20)
	if _, err := rand.Read(world); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(volumeDir, "world.dat"), world, 0o600); err != nil {
		t.Fatal(err)
	}

	runtime := orchruntime.NewFake()
	agentContext, stopAgent := context.WithCancel(ctx)
	agentDone := make(chan error, 1)
	go func() {
		agentDone <- agentctl.Serve(agentContext, agentctl.ServeOptions{
			DataDir: agentDir, Snapshotter: "overlayfs", Runtime: runtime, Discovery: discovery.NewFake(), Ingress: &ingress.Fake{},
			Now: time.Now, Random: rand.Reader,
		})
	}()
	defer func() {
		stopAgent()
		<-agentDone
	}()
	waitForApplicationStatus(t, remote, "game", orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_RUNNING)

	// The first scheduled backup is taken as soon as the agent sees the policy.
	var scheduled *orchv1alpha1.VolumeBackupInfo
	deadline := time.Now().Add(10 * time.Second)
	for scheduled == nil && time.Now().Before(deadline) {
		volumes, err := remote.ListVolumes(ctx, "game", "data")
		if err != nil {
			t.Fatal(err)
		}
		if len(volumes) == 1 && len(volumes[0].GetBackups()) > 0 {
			scheduled = volumes[0].GetBackups()[0]
			if volumes[0].GetNode() != "node-1" || volumes[0].GetService() != "server" || volumes[0].GetBackupSchedule() != "0 3 * * *" {
				t.Fatalf("volume = %#v", volumes[0])
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if scheduled == nil {
		t.Fatal("no scheduled backup was taken")
	}

	// Downloading streams the archive in several windows.
	stream, err := remote.DownloadVolumeBackup(ctx, "game", "data", scheduled.GetName())
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	for stream.Receive() {
		archive.Write(stream.Msg().GetData())
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if uint64(archive.Len()) != scheduled.GetSizeBytes() || archive.Len() <= 1<<20 {
		t.Fatalf("downloaded %d bytes, backup has %d", archive.Len(), scheduled.GetSizeBytes())
	}
	if contents := archivedFile(t, archive.Bytes(), "world.dat"); !bytes.Equal(contents, world) {
		t.Fatal("downloaded archive does not hold the volume's data")
	}

	if err := os.WriteFile(filepath.Join(volumeDir, "world.dat"), []byte("griefed"), 0o600); err != nil {
		t.Fatal(err)
	}
	manual, err := remote.BackupVolume(ctx, "game", "data")
	if err != nil || manual.GetName() == "" {
		t.Fatalf("BackupVolume() = %#v, %v", manual, err)
	}
	time.Sleep(1100 * time.Millisecond)
	previous, err := remote.RestoreVolume(ctx, "game", "data", scheduled.GetName())
	if err != nil {
		t.Fatal(err)
	}
	if restored, _ := os.ReadFile(filepath.Join(volumeDir, "world.dat")); !bytes.Equal(restored, world) {
		t.Fatal("restore did not bring back the scheduled backup")
	}
	if previous.GetName() == "" {
		t.Fatal("restore did not keep the previous contents")
	}
	waitForApplicationStatus(t, remote, "game", orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_RUNNING)
	if _, err := remote.RestoreVolume(ctx, "game", "data", "20000101T000000Z"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("restore of a missing backup = %v", err)
	}

	before := runtime.SnapshotObjects()
	replicas, err := remote.RestartService(ctx, "game", "server", 0)
	if err != nil || len(replicas) != 1 || replicas[0] != 1 {
		t.Fatalf("RestartService() = %v, %v", replicas, err)
	}
	after := runtime.SnapshotObjects()
	for id, object := range after {
		if !object.Running || before[id].StartedAt.Equal(object.StartedAt) {
			t.Fatalf("restart did not start a fresh process: before %#v after %#v", before[id], object)
		}
	}
	if _, err := remote.RestartService(ctx, "game", "missing", 0); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("restart of a missing service = %v", err)
	}
}

func archivedFile(t *testing.T, archive []byte, name string) []byte {
	t.Helper()
	compressed, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(compressed)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			t.Fatalf("archive has no %s", name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == name {
			contents, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			return contents
		}
	}
}
