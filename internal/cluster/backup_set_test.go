package cluster

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/security"
	"github.com/TorstenDittmann/orch/internal/store"
)

func TestBackupSetRestoresFromItsDirectory(t *testing.T) {
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
	box, _, err := security.LoadOrCreateSecretBox(filepath.Join(initialized.DataDir, SecretKeyFilename), bytes.NewReader(bytes.Repeat([]byte{7}, 4096)))
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.OpenInitialized(ctx, filepath.Join(initialized.DataDir, DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if err := state.EnsureSecretKeyFingerprint(ctx, box.Fingerprint(), bootstrapTestTime); err != nil {
		t.Fatal(err)
	}
	if _, err := state.SetApplicationSecret(ctx, box, "hello", "token", "secv_one", []byte("sensitive"), "idempotency-key-one", bootstrapTestTime); err != nil {
		t.Fatal(err)
	}
	set, err := WriteBackupSet(ctx, state, initialized.DataDir, bootstrapTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(set.Path) != filepath.Join(initialized.DataDir, BackupsDirectory) || !set.CreatedAt.Equal(bootstrapTestTime.UTC().Truncate(time.Second)) {
		t.Fatalf("set path = %s", set.Path)
	}
	if _, err := WriteBackupSet(ctx, state, initialized.DataDir, bootstrapTestTime); err == nil {
		t.Fatal("a second set with the same timestamp was written")
	}
	restored, err := Restore(ctx, RecoveryOptions{Backup: set.Path, DataDir: filepath.Join(root, "restored"), Now: func() time.Time { return bootstrapTestTime }})
	if err != nil {
		t.Fatal(err)
	}
	if !restored.Ready || restored.Cluster.ID != initialized.Cluster.ID {
		t.Fatalf("Restore() = %#v", restored)
	}
	for _, name := range []string{DatabaseFilename, SecretKeyFilename, filepath.Join(controllerIdentityDirectory, "ca.pem")} {
		assertMode(t, filepath.Join(root, "restored", name), 0o600)
	}
}

func TestPruneBackupSetsKeepsTheNewest(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	root := filepath.Join(dataDir, BackupsDirectory)
	start := time.Date(2026, time.October, 1, 3, 0, 0, 0, time.UTC)
	for day := range 5 {
		if err := os.MkdirAll(filepath.Join(root, start.AddDate(0, 0, day).Format(backupSetLayout)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{".partial-123", "notes"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := PruneBackupSets(dataDir, 2); err != nil {
		t.Fatal(err)
	}
	sets, err := ListBackupSets(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 2 || !sets[0].CreatedAt.Equal(start.AddDate(0, 0, 4)) || !sets[1].CreatedAt.Equal(start.AddDate(0, 0, 3)) {
		t.Fatalf("sets = %#v", sets)
	}
	if _, err := os.Stat(filepath.Join(root, ".partial-123")); !os.IsNotExist(err) {
		t.Fatalf("partial set kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "notes")); err != nil {
		t.Fatalf("unrelated directory removed: %v", err)
	}
}
