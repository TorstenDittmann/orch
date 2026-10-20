package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/discovery"
	"github.com/TorstenDittmann/orch/internal/model"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

func TestVolumeArchiveRoundTripsFilesAndALiveSQLiteDatabase(t *testing.T) {
	ctx := context.Background()
	source := filepath.Join(t.TempDir(), "volume")
	if err := os.MkdirAll(filepath.Join(source, "crash-reports"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "crash-reports", "one.txt"), []byte("boom"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("crash-reports/one.txt", filepath.Join(source, "latest")); err != nil {
		t.Fatal(err)
	}
	// A writer keeps the database open in WAL mode with rows not yet
	// checkpointed, as a running libSQL server would.
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(source, "data.db"), RawQuery: "_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)"}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE players(name TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ada", "grace", "linus"} {
		if _, err := db.Exec(`INSERT INTO players VALUES (?)`, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(source, "data.db-wal")); err != nil {
		t.Fatalf("test database has no WAL: %v", err)
	}

	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if err := archiveVolume(ctx, source, archive, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, name := range archiveNames(t, archive) {
		if strings.HasPrefix(name, "data.db-") {
			t.Fatalf("archive contains SQLite sidecar %q", name)
		}
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if err := os.Mkdir(restored, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := extractVolume(ctx, archive, restored); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(filepath.Join(restored, "crash-reports", "one.txt")); err != nil || string(contents) != "boom" {
		t.Fatalf("restored file = %q, %v", contents, err)
	}
	if info, err := os.Stat(filepath.Join(restored, "crash-reports", "one.txt")); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("restored mode = %v, %v", info, err)
	}
	if link, err := os.Readlink(filepath.Join(restored, "latest")); err != nil || link != "crash-reports/one.txt" {
		t.Fatalf("restored link = %q, %v", link, err)
	}
	copyDB, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(restored, "data.db"), RawQuery: "mode=ro"}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var count int
	if err := copyDB.QueryRow(`SELECT count(*) FROM players`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("restored players = %d, %v", count, err)
	}
}

func TestExtractVolumeRefusesEntriesOutsideTheVolume(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "evil.tar.gz")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	writer := tar.NewWriter(compressed)
	_ = writer.WriteHeader(&tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o600})
	_ = writer.Close()
	_ = compressed.Close()
	_ = file.Close()
	destination := t.TempDir()
	if err := extractVolume(context.Background(), archive, destination); err == nil || !strings.Contains(err.Error(), "outside the volume") {
		t.Fatalf("extractVolume() = %v", err)
	}
}

func TestVolumeBackupsRunOnScheduleAndPruneScheduledOnes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 17, 2, 0, 0, 0, time.UTC)
	backups := testVolumeBackups(t, nil, func() time.Time { return now })
	writeVolumeFile(t, backups, "hello", "data", "save.dat", "one")
	policy := model.VolumeBackupDesired{Volume: "data", Schedule: "0 3 * * *", Keep: 2}
	owner := model.AllocationDesired{Application: "hello"}

	// A volume without backups is backed up right away.
	if due, err := backups.due("hello", policy); err != nil || !due {
		t.Fatalf("due without backups = %t, %v", due, err)
	}
	for _, at := range []time.Time{now, now.Add(25 * time.Hour), now.Add(49 * time.Hour)} {
		now = at
		if _, err := backups.backup(ctx, owner, "data", nil, ""); err != nil {
			t.Fatal(err)
		}
		if due, _ := backups.due("hello", policy); due {
			t.Fatalf("due right after the backup at %s", at)
		}
	}
	now = now.Add(24 * time.Hour)
	if due, _ := backups.due("hello", policy); !due {
		t.Fatal("not due after the schedule fired")
	}
	if _, err := backups.backup(ctx, owner, "data", nil, volumeBackupManual); err != nil {
		t.Fatal(err)
	}
	if err := backups.prune("hello", "data", 2); err != nil {
		t.Fatal(err)
	}
	listed, err := backups.list("hello", "data")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, backup := range listed {
		names = append(names, backup.Name)
	}
	if got, want := strings.Join(names, ","), "20261020T030000Z-manual,20261019T030000Z,20261018T030000Z"; got != want {
		t.Fatalf("backups = %s, want %s", got, want)
	}
}

func TestVolumeBackupSkipsWhenTheBeforeCommandFails(t *testing.T) {
	ctx := context.Background()
	runtime := orchruntime.NewFake()
	supervisor := NewSupervisorWithServices(supervisorCache(t), runtime, func() time.Time { return supervisorTestTime },
		SupervisorServices{Discovery: discovery.NewFake()})
	allocation := supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))
	if _, err := supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{allocation}}); err != nil {
		t.Fatal(err)
	}
	runtime.SetExecFrames(orchruntime.ObjectID(allocation.ID, allocation.SpecDigest), "save-world",
		[]orchruntime.ExecFrame{{Data: []byte("disk busy")}, {Exited: true, ExitCode: 3}})
	now := supervisorTestTime
	backups := testVolumeBackups(t, supervisor, func() time.Time { return now })
	writeVolumeFile(t, backups, allocation.Application, "data", "world.dat", "state")
	_, err := backups.backup(ctx, allocation, "data", []string{"save-world"}, "")
	if err == nil || !strings.Contains(err.Error(), "exited with code 3: disk busy") {
		t.Fatalf("backup() = %v", err)
	}
	if listed, _ := backups.list(allocation.Application, "data"); len(listed) != 0 {
		t.Fatalf("a failed before command left backups %v", listed)
	}
	status, err := backups.status(allocation.Application, "data")
	if err != nil || !strings.Contains(status.Failure, "disk busy") {
		t.Fatalf("status = %#v, %v", status, err)
	}
	policy := model.VolumeBackupDesired{Volume: "data", Schedule: "0 3 * * *", Keep: 7}
	if due, _ := backups.due(allocation.Application, policy); due {
		t.Fatal("a failed backup is retried before an hour passed")
	}
	now = now.Add(volumeBackupRetryAfter)
	if due, _ := backups.due(allocation.Application, policy); !due {
		t.Fatal("a failed backup is not retried after an hour")
	}
}

func TestVolumeRestoreSwapsTheDataAndRestartsTheOwner(t *testing.T) {
	ctx := context.Background()
	runtime := orchruntime.NewFake()
	cache := supervisorCache(t)
	supervisor := NewSupervisorWithServices(cache, runtime, func() time.Time { return supervisorTestTime },
		SupervisorServices{Discovery: discovery.NewFake()})
	allocation := supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))
	assignment := model.Assignment{Allocations: []model.AllocationDesired{allocation}}
	if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	now := supervisorTestTime
	backups := testVolumeBackups(t, supervisor, func() time.Time { return now })
	writeVolumeFile(t, backups, allocation.Application, "data", "world.dat", "monday")
	saved, err := backups.backup(ctx, allocation, "data", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	writeVolumeFile(t, backups, allocation.Application, "data", "world.dat", "tuesday")
	now = now.Add(time.Minute)

	previous, err := backups.restore(ctx, allocation.Application, "data", saved.Name, allocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if contents, _ := os.ReadFile(filepath.Join(backups.volumeDir(allocation.Application, "data"), "world.dat")); string(contents) != "monday" {
		t.Fatalf("restored contents = %q", contents)
	}
	if !strings.HasSuffix(previous.Name, "-"+volumeBackupBeforeRestore) {
		t.Fatalf("previous contents backup = %q", previous.Name)
	}
	objectID := orchruntime.ObjectID(allocation.ID, allocation.SpecDigest)
	if _, exists := runtime.SnapshotObjects()[objectID]; exists {
		t.Fatal("the owner kept running during the restore")
	}
	statuses, err := supervisor.Reconcile(ctx, assignment)
	if err != nil {
		t.Fatal(err)
	}
	if object, exists := runtime.SnapshotObjects()[objectID]; !exists || !object.Running {
		t.Fatal("reconciliation did not start the owner again")
	}
	if statuses[0].RestartCount != 0 {
		t.Fatalf("restore counted as a crash: %d restarts", statuses[0].RestartCount)
	}
	// The previous contents can be restored in turn.
	if _, err := backups.restore(ctx, allocation.Application, "data", previous.Name, ""); err != nil {
		t.Fatal(err)
	}
	if contents, _ := os.ReadFile(filepath.Join(backups.volumeDir(allocation.Application, "data"), "world.dat")); string(contents) != "tuesday" {
		t.Fatalf("contents after restoring the previous backup = %q", contents)
	}
	if _, err := backups.restore(ctx, allocation.Application, "data", "../../etc", ""); !errors.Is(err, errVolumeBackupNotFound) {
		t.Fatalf("restore of an unknown backup = %v", err)
	}
}

func TestSupervisorRestartStartsAFreshProcess(t *testing.T) {
	ctx := context.Background()
	runtime := orchruntime.NewFake()
	supervisor := NewSupervisorWithServices(supervisorCache(t), runtime, func() time.Time { return supervisorTestTime },
		SupervisorServices{Discovery: discovery.NewFake()})
	allocation := supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))
	assignment := model.Assignment{Allocations: []model.AllocationDesired{allocation}}
	if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Restart(ctx, allocation.ID); err != nil {
		t.Fatal(err)
	}
	objectID := orchruntime.ObjectID(allocation.ID, allocation.SpecDigest)
	if _, exists := runtime.SnapshotObjects()[objectID]; exists {
		t.Fatal("restart left the old process")
	}
	statuses, err := supervisor.Reconcile(ctx, assignment)
	if err != nil {
		t.Fatal(err)
	}
	if object, exists := runtime.SnapshotObjects()[objectID]; !exists || !object.Running || statuses[0].Phase != model.AllocationRunning {
		t.Fatalf("restart did not start a fresh process: %#v", statuses[0])
	}
}

func testVolumeBackups(t *testing.T, supervisor *Supervisor, now func() time.Time) *volumeBackups {
	t.Helper()
	return newVolumeBackups(t.TempDir(), supervisor, nil, now)
}

func writeVolumeFile(t *testing.T, backups *volumeBackups, application, volume, name, contents string) {
	t.Helper()
	directory := backups.volumeDir(application, volume)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func archiveNames(t *testing.T, archive string) []string {
	t.Helper()
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(compressed)
	var names []string
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
}

func TestVolumeBackupFailuresListAssignedVolumes(t *testing.T) {
	now := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	backups := newVolumeBackups(t.TempDir(), nil, nil, func() time.Time { return now })
	backups.recordStatus("shop", "main-backup", errors.New("pg_dump: connection refused"))
	backups.recordStatus("other", "data", errors.New("not assigned here"))
	assignment := model.Assignment{Allocations: []model.AllocationDesired{
		{Application: "shop", VolumeBackups: []model.VolumeBackupDesired{{Volume: "main-backup"}, {Volume: "cache-data"}}},
	}}
	failures := backups.failures(assignment)
	if len(failures) != 1 || failures[0].Volume != "main-backup" || !failures[0].FailedAt.Equal(now) || failures[0].Message != "pg_dump: connection refused" {
		t.Fatalf("failures = %#v", failures)
	}
	backups.recordStatus("shop", "main-backup", nil)
	if failures := backups.failures(assignment); len(failures) != 0 {
		t.Fatalf("a recovered volume is still reported: %#v", failures)
	}
}
