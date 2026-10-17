package control

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	controlclient "github.com/TorstenDittmann/orch/internal/client"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/store"
)

func TestBackupSchedulerTakesOneBackupADayAndKeepsTheNewest(t *testing.T) {
	_, dir, _, _ := bootstrapController(t)
	ctx := context.Background()
	state, err := store.OpenInitialized(ctx, filepath.Join(dir, clusterctl.DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	now := time.Date(2026, time.October, 17, 3, 0, 0, 0, time.UTC)
	scheduler := newBackupScheduler(state, dir, 2, func() time.Time { return now })
	scheduler.runOnce(ctx)
	now = now.Add(time.Hour)
	scheduler.runOnce(ctx)
	if sets, err := clusterctl.ListBackupSets(dir); err != nil || len(sets) != 1 {
		t.Fatalf("sets after one day = %#v, %v", sets, err)
	}
	for range 3 {
		now = now.Add(backupInterval)
		scheduler.runOnce(ctx)
	}
	sets, err := clusterctl.ListBackupSets(dir)
	if err != nil || len(sets) != 2 || !sets[0].CreatedAt.Equal(now) {
		t.Fatalf("sets after four days = %#v, %v", sets, err)
	}
	if status := scheduler.status(); status.Latest.Path != sets[0].Path || status.Failure != "" {
		t.Fatalf("status = %#v", status)
	}
	// A restarted controller picks up where the last one left off.
	if restarted := newBackupScheduler(state, dir, 2, func() time.Time { return now }); restarted.due(now) {
		t.Fatal("restarted scheduler wants another backup right away")
	}
}

func TestStatusReportsControllerBackups(t *testing.T) {
	_, dir, path, _ := bootstrapController(t)
	ctx := context.Background()
	server, err := Start(ctx, Options{DataDir: dir, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	rewriteContextServer(t, path, "https://"+server.Addr().String())
	remote, err := controlclient.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.CloseIdleConnections)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, backup, err := remote.GetClusterStatus(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if backup.GetEnabled() && backup.GetLatestAt() != nil {
			if !strings.HasPrefix(backup.GetLatestPath(), filepath.Join(dir, clusterctl.BackupsDirectory)) {
				t.Fatalf("backup = %#v", backup)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no first backup reported: %#v", backup)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBackupsCanBeTurnedOff(t *testing.T) {
	_, dir, path, _ := bootstrapController(t)
	ctx := context.Background()
	server, err := Start(ctx, Options{DataDir: dir, Listen: "127.0.0.1:0", BackupKeep: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	rewriteContextServer(t, path, "https://"+server.Addr().String())
	remote, err := controlclient.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.CloseIdleConnections)
	if _, backup, err := remote.GetClusterStatus(ctx); err != nil || backup.GetEnabled() {
		t.Fatalf("backup = %#v, %v", backup, err)
	}
}

func TestMetricsReportControllerBackups(t *testing.T) {
	created := time.Date(2026, time.October, 17, 3, 0, 0, 0, time.UTC)
	output := string(renderMetrics(metricsInput{now: created, backups: &backupStatus{
		Latest: clusterctl.BackupSet{CreatedAt: created}, Failure: "disk full",
	}}))
	for _, want := range []string{"orch_controller_backup_timestamp_seconds 1792206000", "orch_controller_backup_failed 1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("metrics lack %q:\n%s", want, output)
		}
	}
	if off := string(renderMetrics(metricsInput{now: created})); strings.Contains(off, "orch_controller_backup") {
		t.Fatalf("metrics report backups while they are off:\n%s", off)
	}
}
