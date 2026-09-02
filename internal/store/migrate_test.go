package store

import (
	"context"
	"database/sql"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
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
