package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

var testCreatedAt = time.Date(2026, time.September, 2, 12, 0, 0, 123, time.UTC)

func TestOpenConfiguresAndReopensDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "state", "control.db")

	state, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	assertPragma(t, state, "journal_mode", "wal")
	assertPragma(t, state, "foreign_keys", "1")
	assertPragma(t, state, "synchronous", "2")
	assertPragma(t, state, "busy_timeout", "5000")
	var migrations int
	if err := state.db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if migrations != 20 {
		t.Fatalf("migration count = %d, want 20", migrations)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
	info, err := os.Stat(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("database permissions = %04o, want 0600", got)
	}

	reopened, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if migrations != 20 {
		t.Fatalf("migration count after reopen = %d, want 20", migrations)
	}
}

func TestOpenExistingRejectsUnrecognizedDatabaseWithoutChangingIt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "foreign.db")
	contents := []byte("not a sqlite database")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExisting(context.Background(), path); !errors.Is(err, ErrUnrecognized) {
		t.Fatalf("OpenExisting() = %v, want ErrUnrecognized", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(contents) {
		t.Fatalf("unrecognized database changed: %q", after)
	}
}

func TestInitializeClusterCommitsClusterAndEventTogether(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	cluster := testCluster("cluster-one")
	if err := state.InitializeCluster(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	got, err := state.Cluster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != cluster {
		t.Fatalf("Cluster() = %#v, want %#v", got, cluster)
	}
	events, err := state.Events(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Sequence != 1 || events[0].ClusterID != cluster.ID || events[0].Kind != model.EventClusterInitialized {
		t.Fatalf("Events() = %#v", events)
	}
	var payload map[string]string
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["name"] != cluster.Name || payload["endpoint"] != cluster.Endpoint {
		t.Fatalf("event payload = %#v", payload)
	}
	if err := state.InitializeCluster(ctx, testCluster("cluster-two")); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second InitializeCluster() = %v, want ErrAlreadyInitialized", err)
	}
}

func TestInitializeClusterRollsBackWhenEventFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if _, err := state.db.ExecContext(ctx, `CREATE TRIGGER reject_initial_event
		BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'event rejected'); END`); err != nil {
		t.Fatal(err)
	}
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err == nil || !strings.Contains(err.Error(), "event rejected") {
		t.Fatalf("InitializeCluster() = %v, want event failure", err)
	}
	if _, err := state.Cluster(ctx); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Cluster() error = %v, want ErrNotInitialized", err)
	}
	var events int
	if err := state.db.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatalf("event count = %d, want 0", events)
	}
}

func TestBackupIsOnlineVerifiedAndNoClobber(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	cluster := testCluster("cluster-one")
	if err := state.InitializeCluster(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	input := applicationInput(`{"name":"hello"}`, "revision-one", "idempotency-key-one", testCreatedAt)
	wantApply, err := state.ApplyApplication(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	destination := filepath.Join(directory, "backups", "cluster.db")
	if err := state.Backup(ctx, destination); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("backup permissions = %04o, want 0600", got)
	}
	backup, err := Open(ctx, destination)
	if err != nil {
		t.Fatal(err)
	}
	got, err := backup.Cluster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != cluster {
		t.Fatalf("backup cluster = %#v, want %#v", got, cluster)
	}
	application, err := backup.Application(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if application.RevisionID != wantApply.RevisionID || application.Digest != wantApply.Digest {
		t.Fatalf("backup application = %#v, want apply %#v", application, wantApply)
	}
	replayed, err := backup.ApplyApplication(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed, wantApply) {
		t.Fatalf("backup replay = %#v, want %#v", replayed, wantApply)
	}
	if err := backup.Close(); err != nil {
		t.Fatal(err)
	}

	original, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Backup(ctx, destination); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second Backup() = %v, want no-clobber error", err)
	}
	after, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("existing backup changed after no-clobber failure")
	}
	temporary, err := filepath.Glob(filepath.Join(filepath.Dir(destination), ".orch-backup-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temporary) != 0 {
		t.Fatalf("backup staging files remain: %v", temporary)
	}
}

func TestBackupDoesNotPublishFailedVerification(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "backups", "cluster.db")
	verificationError := errors.New("injected verification failure")
	err := state.backup(ctx, destination, func(context.Context, string) error {
		return verificationError
	})
	if !errors.Is(err, verificationError) {
		t.Fatalf("backup() = %v, want verification error", err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed backup was published: %v", err)
	}
	temporary, err := filepath.Glob(filepath.Join(filepath.Dir(destination), ".orch-backup-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temporary) != 0 {
		t.Fatalf("backup staging files remain: %v", temporary)
	}
}

func TestEventsRejectsInvalidLimits(t *testing.T) {
	t.Parallel()
	state := openTestStore(t)
	for _, limit := range []int{0, 1001} {
		if _, err := state.Events(context.Background(), limit); err == nil {
			t.Fatalf("Events(%d) succeeded", limit)
		}
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	state, err := Open(context.Background(), filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Errorf("Close() = %v", err)
		}
	})
	return state
}

func testCluster(id string) model.Cluster {
	return model.Cluster{
		ID: id, Name: "test", Endpoint: "https://controller.example:9443",
		Generation: 1, CreatedAt: testCreatedAt, WorkloadCIDR: model.DefaultWorkloadCIDR,
		NetworkGeneration: 1,
	}
}

func assertPragma(t *testing.T, state *Store, name, want string) {
	t.Helper()
	var got string
	if err := state.db.QueryRow("PRAGMA " + name).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("PRAGMA %s = %q, want %q", name, got, want)
	}
}
