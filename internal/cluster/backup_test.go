package cluster

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"change.codes/torsten/better-swarm/internal/security"
	"change.codes/torsten/better-swarm/internal/store"
)

func TestBackupCreatesReadableSnapshot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	initialized, err := Bootstrap(context.Background(), BootstrapOptions{
		Name: "test", Server: "https://controller.example", DataDir: filepath.Join(root, "state"),
		ContextPath: filepath.Join(root, "config", "context.json"), Now: func() time.Time { return bootstrapTestTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "backups", "test.db")
	result, err := Backup(context.Background(), BackupOptions{DataDir: initialized.DataDir, Output: output})
	if err != nil {
		t.Fatal(err)
	}
	if result.Cluster != initialized.Cluster || result.Output != output {
		t.Fatalf("Backup() = %#v", result)
	}
	assertMode(t, output, 0o600)
}

func TestBackupRequiresAndCopiesSecretKeyForEncryptedState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	initialized, err := Bootstrap(ctx, BootstrapOptions{
		Name: "test", Server: "https://controller.example", DataDir: filepath.Join(root, "state"),
		ContextPath: filepath.Join(root, "config", "context.json"), Now: func() time.Time { return bootstrapTestTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(initialized.DataDir, SecretKeyFilename)
	box, _, err := security.LoadOrCreateSecretBox(keyPath, bytes.NewReader(bytes.Repeat([]byte{7}, 4096)))
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.OpenInitialized(ctx, filepath.Join(initialized.DataDir, DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.EnsureSecretKeyFingerprint(ctx, box.Fingerprint(), bootstrapTestTime); err != nil {
		t.Fatal(err)
	}
	if _, err := state.SetApplicationSecret(ctx, box, "hello", "token", "secv_one", []byte("sensitive"), "idempotency-key-one", bootstrapTestTime); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	databaseOutput := filepath.Join(root, "backup", "cluster.db")
	if _, err := Backup(ctx, BackupOptions{DataDir: initialized.DataDir, Output: databaseOutput}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("Backup() without key output = %v", err)
	}
	keyOutput := filepath.Join(root, "backup", "secret.key")
	result, err := Backup(ctx, BackupOptions{DataDir: initialized.DataDir, Output: databaseOutput, SecretKeyOutput: keyOutput})
	if err != nil || result.SecretKeyOutput != keyOutput {
		t.Fatalf("Backup() = %#v, %v", result, err)
	}
	assertMode(t, databaseOutput, 0o600)
	assertMode(t, keyOutput, 0o600)
	original, _ := os.ReadFile(keyPath)
	copied, _ := os.ReadFile(keyOutput)
	if !bytes.Equal(original, copied) {
		t.Fatal("backup key does not match controller key")
	}
}

func TestBackupValidatesPathsAndRequiresExistingState(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dataDir := filepath.Join(root, "state")
	if _, err := Backup(context.Background(), BackupOptions{DataDir: dataDir, Output: filepath.Join(dataDir, "backup.db")}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("Backup() overlapping path = %v, want ErrInvalidOptions", err)
	}
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Backup(context.Background(), BackupOptions{DataDir: dataDir, Output: filepath.Join(root, "backups", "test.db")}); err == nil || !strings.Contains(err.Error(), DatabaseFilename) {
		t.Fatalf("Backup() missing state = %v", err)
	}
}

func TestBackupDoesNotMigrateOrAcceptNewerLiveSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	initialized, err := Bootstrap(ctx, BootstrapOptions{
		Name: "test", Server: "https://controller.example", DataDir: filepath.Join(root, "state"),
		ContextPath: filepath.Join(root, "config", "context.json"), Now: func() time.Time { return bootstrapTestTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(initialized.DataDir, DatabaseFilename)
	// Simulate an older controller: its history lacks the newest migration.
	mutateRecoveryDatabase(t, databasePath, "DELETE FROM schema_migrations WHERE version = (SELECT max(version) FROM schema_migrations)")
	countMigrations := func() int {
		t.Helper()
		db, err := sql.Open("sqlite", databasePath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var count int
		if err := db.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	before := countMigrations()
	if _, err := Backup(ctx, BackupOptions{DataDir: initialized.DataDir, Output: filepath.Join(root, "backups", "old.db")}); err != nil {
		t.Fatal(err)
	}
	if after := countMigrations(); after != before {
		t.Fatalf("backup migrated live database from %d to %d migrations", before, after)
	}

	mutateRecoveryDatabase(t, databasePath, `INSERT INTO schema_migrations(version, name, checksum, applied_at)
		VALUES (999, '999_future.sql', 'x', '2026-01-01T00:00:00Z')`)
	output := filepath.Join(root, "backups", "newer.db")
	if _, err := Backup(ctx, BackupOptions{DataDir: initialized.DataDir, Output: output}); err == nil || !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("Backup() newer schema = %v", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("refused backup left output behind: %v", err)
	}
}
