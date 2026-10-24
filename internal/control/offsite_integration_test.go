package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
			Mounts: []config.Mount{{Source: "data", Target: "/data"}, {Source: "logs", Target: "/logs"}},
		},
	})
	manifest.Volumes = map[string]config.Volume{
		"data": {Driver: "local", Backup: &config.VolumeBackup{Schedule: "0 3 * * *", Keep: 7}},
		"logs": {Driver: "local"},
	}
	if _, err := remote.ApplyApplication(ctx, mustCanonicalConfig(t, manifest), "offsite-deploy-key"); err != nil {
		t.Fatal(err)
	}
	volumeDir := filepath.Join(agentDir, agentctl.VolumesDirectory, "game", "data")
	for _, directory := range []string{volumeDir, filepath.Join(agentDir, agentctl.VolumesDirectory, "game", "logs")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
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
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { stopAgent(); <-agentDone }) }
	defer stop()
	waitForApplicationStatus(t, remote, "game", orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_RUNNING)

	// A manual backup starts a copy at once.
	manual, err := remote.BackupVolume(ctx, "game", "data")
	if err != nil || !manual.GetOnNode() {
		t.Fatalf("BackupVolume() = %v, %v; want a backup on the node", manual, err)
	}
	key := waitForBucketKey(t, bucket, "prod/volumes/game/data/"+manual.GetName()+".tar.gz")
	if archive, _ := bucket.Object(key); !bytes.Equal(archivedFile(t, archive, "world.dat"), world) {
		t.Fatal("the bucket's copy does not hold the volume's data")
	}

	// A manual backup of a volume without a schedule is copied too.
	logs, err := remote.BackupVolume(ctx, "game", "logs")
	if err != nil {
		t.Fatal(err)
	}
	waitForBucketKey(t, bucket, "prod/volumes/game/logs/"+logs.GetName()+".tar.gz")

	// Losing the node's copy leaves the bucket's, which a restore fetches.
	if err := os.Remove(filepath.Join(agentDir, agentctl.VolumeBackupsDirectory, "game", "data", manual.GetName()+".tar.gz")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(volumeDir, "world.dat"), []byte("griefed"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A single node never records a claim; record one as a move would.
	node, err := server.state.NodeByName(ctx, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	claimedAt := time.Now().Add(-time.Hour).UTC()
	if err := server.state.ClaimVolume(ctx, "game", "data", node.ID, claimedAt); err != nil {
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
	// db restore relies on the claim time to spot backups from a lost node.
	if claimed := volumes[0].GetClaimedAt(); claimed == nil || !claimed.AsTime().Equal(claimedAt) {
		t.Fatalf("claimed_at = %v, want %s", claimed, claimedAt)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := remote.RestoreVolume(ctx, "game", "data", manual.GetName()); err != nil {
		t.Fatal(err)
	}
	if restored, _ := os.ReadFile(filepath.Join(volumeDir, "world.dat")); !bytes.Equal(restored, world) {
		t.Fatal("restore did not bring back the bucket's backup")
	}

	// With the node offline, a download streams the bucket's copy.
	stop()
	deadline := time.Now().Add(15 * time.Second)
	for {
		volumes, err := remote.ListVolumes(ctx, "game", "logs")
		if err == nil && !volumes[0].GetReachable() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node still reachable: %v, %v", volumes, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	stream, err := remote.DownloadVolumeBackup(ctx, "game", "logs", logs.GetName())
	if err != nil {
		t.Fatal(err)
	}
	var downloaded []byte
	for stream.Receive() {
		downloaded = append(downloaded, stream.Msg().GetData()...)
		if stream.Msg().GetSizeBytes() != logs.GetSizeBytes() {
			t.Fatalf("download reports %d bytes, want %d", stream.Msg().GetSizeBytes(), logs.GetSizeBytes())
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if stored, _ := bucket.Object("prod/volumes/game/logs/" + logs.GetName() + ".tar.gz"); !bytes.Equal(downloaded, stored) {
		t.Fatal("download from the bucket differs from its copy")
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
	names := map[string]bool{}
	for _, name := range []string{"20261014T030000Z", "20261015T030000Z", "20261016T030000Z", "20261014T120000Z-manual", "20261013T120000Z-before-restore"} {
		bucket.SetObject("v/"+name+".tar.gz", []byte("x"))
		names[name] = true
	}
	if err := pruneRemote(context.Background(), client, "v/", names, 2, time.Time{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"v/20261013T120000Z-before-restore.tar.gz", "v/20261014T120000Z-manual.tar.gz", "v/20261015T030000Z.tar.gz", "v/20261016T030000Z.tar.gz"}
	if got := bucket.Keys(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("kept %v, want %v", got, want)
	}
}

func TestPruneRemoteKeepsBackupsFromBeforeTheClaim(t *testing.T) {
	bucket := s3test.New("backups")
	defer bucket.Close()
	client := objectstore.New(objectstore.Destination{Bucket: "backups", Endpoint: bucket.URL, AccessKeyID: "id", SecretAccessKey: "secret"}, nil)
	names := map[string]bool{}
	for _, name := range []string{"20261010T030000Z", "20261011T030000Z", "20261015T030000Z", "20261016T030000Z", "20261017T030000Z"} {
		bucket.SetObject("v/"+name+".tar.gz", []byte("x"))
		names[name] = true
	}
	// The volume moved to a new node on the 14th; the lost node's backups stay.
	claimed := time.Date(2026, 10, 14, 9, 30, 15, 500, time.UTC)
	if err := pruneRemote(context.Background(), client, "v/", names, 2, claimed); err != nil {
		t.Fatal(err)
	}
	want := []string{"v/20261010T030000Z.tar.gz", "v/20261011T030000Z.tar.gz", "v/20261016T030000Z.tar.gz", "v/20261017T030000Z.tar.gz"}
	if got := bucket.Keys(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("kept %v, want %v", got, want)
	}
}

func TestMissingNewestSkipsBackupsPruningWouldDelete(t *testing.T) {
	names := []string{"20261016T030000Z", "20261015T120000Z-manual", "20261015T030000Z", "20261014T030000Z", "20261013T030000Z"}
	remote := map[string]bool{"20261015T030000Z": true}
	got := missingNewest(names, remote, 2)
	if len(got) != 2 || names[got[0]] != "20261015T120000Z-manual" || names[got[1]] != "20261016T030000Z" {
		t.Fatalf("missingNewest() = %v", got)
	}
}

func TestUploadBackupSetRetriesFailedParts(t *testing.T) {
	singlePut, minPart, delay := offsiteSinglePutLimit, offsiteMinPartSize, offsiteRetryDelay
	offsiteSinglePutLimit, offsiteMinPartSize, offsiteRetryDelay = 1, 1024, time.Millisecond
	defer func() { offsiteSinglePutLimit, offsiteMinPartSize, offsiteRetryDelay = singlePut, minPart, delay }()

	set := filepath.Join(t.TempDir(), "20261017T030000Z")
	if err := os.MkdirAll(filepath.Join(set, "pki"), 0o700); err != nil {
		t.Fatal(err)
	}
	database := make([]byte, 8<<10)
	_, _ = rand.Read(database)
	files := map[string][]byte{clusterctl.DatabaseFilename: database}
	for _, name := range []string{"ca.pem", "ca-key.pem", "controller.pem", "controller-key.pem"} {
		files[filepath.Join("pki", name)] = []byte(name)
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(set, name), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bucket := s3test.New("backups")
	defer bucket.Close()
	client := objectstore.New(objectstore.Destination{Bucket: "backups", Endpoint: bucket.URL, AccessKeyID: "id", SecretAccessKey: "secret"}, nil)
	// The first part fails twice and goes through on its last attempt.
	bucket.FailNext(2)
	if err := uploadBackupSet(context.Background(), client, set, "controller/20261017T030000Z.tar.gz"); err != nil {
		t.Fatal(err)
	}
	archive, ok := bucket.Object("controller/20261017T030000Z.tar.gz")
	if !ok || len(archive) <= 1024 {
		t.Fatalf("uploaded %d bytes, want a multipart archive", len(archive))
	}
	extracted := filepath.Join(t.TempDir(), "set")
	if err := clusterctl.ExtractBackupSet(bytes.NewReader(archive), extracted); err != nil {
		t.Fatal(err)
	}
	for name, contents := range files {
		if got, err := os.ReadFile(filepath.Join(extracted, name)); err != nil || !bytes.Equal(got, contents) {
			t.Fatalf("%s differs after the multipart upload: %v", name, err)
		}
	}

	// A part that keeps failing aborts the upload and leaves nothing behind.
	bucket.FailNext(3)
	if err := uploadBackupSet(context.Background(), client, set, "controller/20261018T030000Z.tar.gz"); err == nil {
		t.Fatal("uploadBackupSet succeeded although a part failed three times")
	}
	if _, ok := bucket.Object("controller/20261018T030000Z.tar.gz"); ok {
		t.Fatal("a failed multipart upload left an object")
	}
}
