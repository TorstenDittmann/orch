package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
