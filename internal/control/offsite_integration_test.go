package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
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
	"github.com/TorstenDittmann/orch/internal/objectstore"
	"github.com/TorstenDittmann/orch/internal/objectstore/s3test"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

func TestLoopbackBackupsCopyToBucketAndRestoreFromIt(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback lifecycle test")
	}
	ctx := context.Background()
	bucket := s3test.New("backups")
	defer bucket.Close()
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
	server, err := Start(ctx, Options{DataDir: controllerDir, Listen: address})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	remote, err := controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.CloseIdleConnections()

	// A prefix another cluster claimed is refused.
	bucket.SetObject("taken/orch-cluster", []byte("cluster-other\n"))
	request := &orchv1alpha1.SetBackupDestinationRequest{Url: "s3://backups/taken", Endpoint: bucket.URL, AccessKeyId: "id", SecretAccessKey: "secret"}
	if _, err := remote.SetBackupDestination(ctx, request); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("SetBackupDestination() on a claimed prefix = %v", err)
	}
	request.Url = "s3://backups/prod"
	destination, err := remote.SetBackupDestination(ctx, request)
	if err != nil || destination.GetUrl() != "s3://backups/prod" || destination.GetRegion() != "us-east-1" {
		t.Fatalf("SetBackupDestination() = %#v, %v", destination, err)
	}

	// The controller's own backup set is copied right away.
	waitForBucketKey(t, bucket, "prod/controller/")

	if _, err := remote.ConfigureNetwork(ctx, "10.96.0.0/16", "offsite-network-key"); err != nil {
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
	if _, err := remote.ApplyApplication(ctx, mustCanonicalConfig(t, manifest), "offsite-deploy-key"); err != nil {
		t.Fatal(err)
	}
	volumeDir := filepath.Join(agentDir, agentctl.VolumesDirectory, "game", "data")
	if err := os.MkdirAll(volumeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	world := []byte("the world before the disk died")
	if err := os.WriteFile(filepath.Join(volumeDir, "world.dat"), world, 0o600); err != nil {
		t.Fatal(err)
	}
	agentContext, stopAgent := context.WithCancel(ctx)
	agentDone := make(chan error, 1)
	go func() {
		agentDone <- agentctl.Serve(agentContext, agentctl.ServeOptions{
			DataDir: agentDir, Snapshotter: "overlayfs", Runtime: orchruntime.NewFake(), Discovery: discovery.NewFake(), Ingress: &ingress.Fake{},
			Now: time.Now, Random: rand.Reader,
		})
	}()
	defer func() {
		stopAgent()
		<-agentDone
	}()
	waitForApplicationStatus(t, remote, "game", orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_RUNNING)

	// A manual backup starts a copy at once.
	manual, err := remote.BackupVolume(ctx, "game", "data")
	if err != nil {
		t.Fatal(err)
	}
	key := waitForBucketKey(t, bucket, "prod/volumes/game/data/"+manual.GetName()+".tar.gz")
	if archive, _ := bucket.Object(key); !bytes.Equal(archivedFile(t, archive, "world.dat"), world) {
		t.Fatal("the bucket's copy does not hold the volume's data")
	}

	// Losing the node's copy leaves the bucket's, which a restore fetches.
	if err := os.Remove(filepath.Join(agentDir, agentctl.VolumeBackupsDirectory, "game", "data", manual.GetName()+".tar.gz")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(volumeDir, "world.dat"), []byte("griefed"), 0o600); err != nil {
		t.Fatal(err)
	}
	volumes, err := remote.ListVolumes(ctx, "game", "data")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, backup := range volumes[0].GetBackups() {
		if backup.GetName() == manual.GetName() {
			found = backup.GetInBucket() && !backup.GetOnNode()
		}
	}
	if !found {
		t.Fatalf("listing does not show %s as only in the bucket: %#v", manual.GetName(), volumes[0].GetBackups())
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := remote.RestoreVolume(ctx, "game", "data", manual.GetName()); err != nil {
		t.Fatal(err)
	}
	if restored, _ := os.ReadFile(filepath.Join(volumeDir, "world.dat")); !bytes.Equal(restored, world) {
		t.Fatal("restore did not bring back the bucket's backup")
	}

	status, err := remote.GetBackupDestination(ctx)
	if err != nil || status.GetDestination().GetUrl() != "s3://backups/prod" {
		t.Fatalf("GetBackupDestination() = %#v, %v", status, err)
	}
	if changed, err := remote.RemoveBackupDestination(ctx); err != nil || !changed {
		t.Fatalf("RemoveBackupDestination() = %t, %v", changed, err)
	}
}

func waitForBucketKey(t *testing.T, bucket *s3test.Server, prefix string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, key := range bucket.Keys() {
			if strings.HasPrefix(key, prefix) {
				return key
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("bucket has no %s*: %v", prefix, bucket.Keys())
	return ""
}

func TestPruneRemoteKeepsNewestScheduledAndLabeledBackups(t *testing.T) {
	bucket := s3test.New("backups")
	defer bucket.Close()
	client := objectstore.New(objectstore.Destination{Bucket: "backups", Endpoint: bucket.URL, AccessKeyID: "id", SecretAccessKey: "secret"}, nil)
	names := map[string]objectstore.Object{}
	for _, name := range []string{"20261014T030000Z", "20261015T030000Z", "20261016T030000Z", "20261014T120000Z-manual", "20261013T120000Z-before-restore"} {
		bucket.SetObject("v/"+name+".tar.gz", []byte("x"))
		names[name] = objectstore.Object{Key: "v/" + name + ".tar.gz"}
	}
	if err := pruneRemote(context.Background(), client, "v/", names, 2); err != nil {
		t.Fatal(err)
	}
	want := []string{"v/20261013T120000Z-before-restore.tar.gz", "v/20261014T120000Z-manual.tar.gz", "v/20261015T030000Z.tar.gz", "v/20261016T030000Z.tar.gz"}
	if got := bucket.Keys(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("kept %v, want %v", got, want)
	}
}

func TestMissingNewestSkipsBackupsPruningWouldDelete(t *testing.T) {
	names := []string{"20261016T030000Z", "20261015T120000Z-manual", "20261015T030000Z", "20261014T030000Z", "20261013T030000Z"}
	remote := map[string]objectstore.Object{"20261015T030000Z": {}}
	got := missingNewest(names, remote, 2)
	if len(got) != 2 || names[got[0]] != "20261015T120000Z-manual" || names[got[1]] != "20261016T030000Z" {
		t.Fatalf("missingNewest() = %v", got)
	}
}
