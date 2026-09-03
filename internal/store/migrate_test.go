package store

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestLoadMigrationsRequiresContiguousNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		files  fstest.MapFS
		needle string
	}{
		{name: "none", files: fstest.MapFS{}, needle: "no embedded migrations"},
		{name: "gap", files: migrationFS("migrations/002_second.sql", "SELECT 1;"), needle: "contiguous version 001"},
		{name: "bad name", files: migrationFS("migrations/001.sql", "SELECT 1;"), needle: "invalid migration filename"},
		{name: "padding", files: migrationFS("migrations/1_first.sql", "SELECT 1;"), needle: "contiguous version 001"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := loadMigrations(test.files)
			if err == nil || !strings.Contains(err.Error(), test.needle) {
				t.Fatalf("loadMigrations() = %v, want %q", err, test.needle)
			}
		})
	}
}

func TestMigrationsRollBackAllPendingSchema(t *testing.T) {
	t.Parallel()
	db := openMigrationDB(t)
	source := fstest.MapFS{
		"migrations/001_first.sql":  &fstest.MapFile{Data: []byte("CREATE TABLE first_table (id INTEGER);")},
		"migrations/002_broken.sql": &fstest.MapFile{Data: []byte("CREATE TABLE second_table (id INTEGER); definitely not SQL;")},
	}
	if err := applyMigrations(context.Background(), db, source, testCreatedAt); err == nil {
		t.Fatal("applyMigrations() succeeded")
	}
	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master
		WHERE type = 'table' AND name IN ('schema_migrations', 'first_table', 'second_table')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("partially applied tables = %d, want 0", tables)
	}
}

func TestMigrationsFailClosedOnChangedHistory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*testing.T, *sql.DB)
		next   fs.FS
		needle string
	}{
		{
			name:   "checksum",
			next:   migrationFS("migrations/001_first.sql", "CREATE TABLE first_table (id INTEGER, changed TEXT);"),
			needle: "checksum changed",
		},
		{
			name: "name",
			mutate: func(t *testing.T, db *sql.DB) {
				if _, err := db.Exec("UPDATE schema_migrations SET name = 'renamed.sql' WHERE version = 1"); err != nil {
					t.Fatal(err)
				}
			},
			next:   migrationFS("migrations/001_first.sql", "CREATE TABLE first_table (id INTEGER);"),
			needle: "name changed",
		},
		{
			name: "newer",
			mutate: func(t *testing.T, db *sql.DB) {
				if _, err := db.Exec(`INSERT INTO schema_migrations(version, name, checksum, applied_at)
					VALUES (2, 'future.sql', 'future', '2026-09-02T12:00:00Z')`); err != nil {
					t.Fatal(err)
				}
			},
			next:   migrationFS("migrations/001_first.sql", "CREATE TABLE first_table (id INTEGER);"),
			needle: "newer than this binary",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			db := openMigrationDB(t)
			initial := migrationFS("migrations/001_first.sql", "CREATE TABLE first_table (id INTEGER);")
			if err := applyMigrations(context.Background(), db, initial, testCreatedAt); err != nil {
				t.Fatal(err)
			}
			if test.mutate != nil {
				test.mutate(t, db)
			}
			if err := applyMigrations(context.Background(), db, test.next, testCreatedAt); err == nil || !strings.Contains(err.Error(), test.needle) {
				t.Fatalf("applyMigrations() = %v, want %q", err, test.needle)
			}
		})
	}
}

func TestOpenExistingMigratesVersionOneStateWithoutLoss(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "control.db")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	firstMigration, err := fs.ReadFile(embeddedMigrations, "migrations/001_control_core.sql")
	if err != nil {
		t.Fatal(err)
	}
	v1 := fstest.MapFS{
		"migrations/001_control_core.sql": &fstest.MapFile{Data: firstMigration},
	}
	if err := applyMigrations(ctx, db, v1, testCreatedAt); err != nil {
		t.Fatal(err)
	}
	stamp := testCreatedAt.Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `INSERT INTO clusters
		(singleton, id, name, endpoint, generation, created_at)
		VALUES (1, 'cluster-one', 'test', 'https://controller.example', 1, ?)`, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO events
		(cluster_id, kind, payload, created_at)
		VALUES ('cluster-one', 'cluster.initialized', '{}', ?)`, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO idempotency_keys
		(key, operation, response, created_at)
		VALUES ('legacy-key', 'reserved', '{}', ?)`, stamp); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(databasePath, 0o600); err != nil {
		t.Fatal(err)
	}

	state, err := OpenExisting(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	cluster, err := state.Cluster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cluster.ID != "cluster-one" || cluster.Name != "test" {
		t.Fatalf("migrated cluster = %#v", cluster)
	}
	var migrationCount, eventCount int
	if err := state.db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if err := state.db.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	var operation, requestHash, response string
	if err := state.db.QueryRowContext(ctx, `SELECT operation, request_hash, response
		FROM idempotency_keys WHERE key = 'legacy-key'`).Scan(&operation, &requestHash, &response); err != nil {
		t.Fatal(err)
	}
	if migrationCount != 3 || eventCount != 1 || operation != "reserved" || requestHash != strings.Repeat("0", 64) || response != "{}" {
		t.Fatalf("migrated state: migrations=%d events=%d operation=%q hash=%q response=%q", migrationCount, eventCount, operation, requestHash, response)
	}
}

func openMigrationDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close() = %v", err)
		}
	})
	return db
}

func migrationFS(name, contents string) fstest.MapFS {
	return fstest.MapFS{name: &fstest.MapFile{Data: []byte(contents)}}
}
