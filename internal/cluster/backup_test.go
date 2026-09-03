package cluster

import (
	"bytes"
	"context"
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
