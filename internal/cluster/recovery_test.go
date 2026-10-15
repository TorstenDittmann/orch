package cluster

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/security"
	"github.com/TorstenDittmann/orch/internal/store"
)

func recoveryFixture(t *testing.T, encrypted bool) RecoveryOptions {
	t.Helper()
	root := t.TempDir()
	ctx := context.Background()
	initialized, err := Bootstrap(ctx, BootstrapOptions{Name: "test", Server: "https://controller.example", DataDir: filepath.Join(root, "live"), ContextPath: filepath.Join(root, "config", "context.json"), Now: func() time.Time { return bootstrapTestTime }})
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(initialized.DataDir, SecretKeyFilename)
	box, _, err := security.LoadOrCreateSecretBox(key, nil)
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
	if encrypted {
		if _, err := state.SetApplicationSecret(ctx, box, "hello", "token", "secv_one", []byte("private-recovery-value"), "secret-recovery-key", bootstrapTestTime); err != nil {
			t.Fatal(err)
		}
		if _, err := state.SetRegistryCredential(ctx, box, "registry.example", "robot", "regv_one", []byte("private-registry-value"), "registry-recovery-key", bootstrapTestTime); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, "backup", "cluster.db")
	keyBackup := filepath.Join(root, "keys", "master.key")
	if _, err := Backup(ctx, BackupOptions{DataDir: initialized.DataDir, Output: backup, SecretKeyOutput: keyBackup}); err != nil {
		t.Fatal(err)
	}
	return RecoveryOptions{Backup: backup, PKIDir: filepath.Join(initialized.DataDir, "pki"), SecretKey: keyBackup, DataDir: filepath.Join(root, "recovered"), Now: func() time.Time { return bootstrapTestTime }}
}

func TestRecoveryPreservesIdentityAndSecretsWithoutChangingSources(t *testing.T) {
	options := recoveryFixture(t, true)
	before, err := os.ReadFile(options.Backup)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	missing := options
	missing.PKIDir = ""
	missing.SecretKey = ""
	info, err := InspectBackup(ctx, missing)
	if err != nil || info.Ready || !info.SecretKeyRequired || len(info.Missing) != 2 {
		t.Fatalf("missing material: %+v %v", info, err)
	}
	if _, err := Restore(ctx, missing); err == nil {
		t.Fatal("restored without keys")
	}
	if _, err := os.Lstat(options.DataDir); !os.IsNotExist(err) {
		t.Fatal("failed restore left destination")
	}
	info, err = InspectBackup(ctx, options)
	if err != nil || !info.Ready {
		t.Fatalf("inspection: %+v %v", info, err)
	}
	result, err := Restore(ctx, options)
	if err != nil || !result.Ready || result.Cluster.ID != info.Cluster.ID {
		t.Fatalf("restore: %+v %v", result, err)
	}
	assertMode(t, options.DataDir, 0o700)
	assertMode(t, filepath.Join(options.DataDir, DatabaseFilename), 0o600)
	for _, name := range controllerPKIFiles {
		original, _ := os.ReadFile(filepath.Join(options.PKIDir, name))
		restored, _ := os.ReadFile(filepath.Join(options.DataDir, "pki", name))
		if !bytes.Equal(original, restored) {
			t.Fatal("identity changed")
		}
		assertMode(t, filepath.Join(options.DataDir, "pki", name), 0o600)
	}
	if _, err := LoadControllerCredentials(options.DataDir, result.Cluster, bootstrapTestTime); err != nil {
		t.Fatal(err)
	}
	state, err := store.OpenInitialized(ctx, filepath.Join(options.DataDir, DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	box, err := security.LoadSecretBox(filepath.Join(options.DataDir, SecretKeyFilename), nil)
	if err != nil {
		t.Fatal(err)
	}
	value, err := state.ResolveApplicationSecretVersion(ctx, box, "hello", "token", "secv_one")
	if err != nil || string(value.Value) != "private-recovery-value" {
		t.Fatal("secret did not survive recovery")
	}
	registry, err := state.ResolveRegistryCredentialVersion(ctx, box, "registry.example", "regv_one")
	if err != nil || string(registry.Secret) != "private-registry-value" {
		t.Fatal("registry credential did not survive recovery")
	}
	if _, err := Restore(ctx, options); err == nil {
		t.Fatal("restore overwrote destination")
	}
	after, _ := os.ReadFile(options.Backup)
	if !bytes.Equal(before, after) {
		t.Fatal("source backup was modified")
	}
	entries, err := os.ReadDir(filepath.Dir(options.Backup))
	if err != nil || len(entries) != 1 {
		t.Fatal("inspection wrote source sidecars")
	}
}

func mutateRecoveryDatabase(t *testing.T, path, query string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(query); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryRejectsInvalidInputs(t *testing.T) {
	for _, name := range []string{"wrong-key", "missing-key", "wrong-pki", "expired-pki", "corrupt", "foreign", "newer", "checksum", "ciphertext", "nonce", "sidecar", "symlink", "public", "existing", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			options := recoveryFixture(t, true)
			ctx := context.Background()
			switch name {
			case "wrong-key":
				if err := os.WriteFile(options.SecretKey, bytes.Repeat([]byte{9}, 32), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing-key":
				options.SecretKey = ""
			case "wrong-pki":
				other := recoveryFixture(t, false)
				options.PKIDir = other.PKIDir
			case "expired-pki":
				options.Now = func() time.Time { return bootstrapTestTime.AddDate(20, 0, 0) }
			case "corrupt":
				if err := os.WriteFile(options.Backup, []byte("not sqlite"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "foreign":
				mutateRecoveryDatabase(t, options.Backup, "DROP TABLE schema_migrations")
			case "newer":
				mutateRecoveryDatabase(t, options.Backup, "INSERT INTO schema_migrations VALUES(999,'future','future','future')")
			case "checksum":
				mutateRecoveryDatabase(t, options.Backup, "UPDATE schema_migrations SET checksum='changed' WHERE version=1")
			case "ciphertext":
				mutateRecoveryDatabase(t, options.Backup, "DROP TRIGGER application_secret_versions_immutable_update; UPDATE application_secret_versions SET ciphertext=X'1234'")
			case "nonce":
				mutateRecoveryDatabase(t, options.Backup, "DROP TRIGGER application_secret_versions_immutable_update; UPDATE application_secret_versions SET wrapped_key_nonce=X'12'")
			case "sidecar":
				if err := os.WriteFile(options.Backup+"-wal", nil, 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				original := options.Backup
				options.Backup += ".link"
				if err := os.Symlink(original, options.Backup); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.Chmod(options.SecretKey, 0o644); err != nil {
					t.Fatal(err)
				}
			case "existing":
				if err := os.Mkdir(options.DataDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(options.DataDir, "sentinel"), []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			_, err := Restore(ctx, options)
			if err == nil {
				t.Fatal("invalid recovery accepted")
			}
			if strings.Contains(err.Error(), "private-recovery-value") || strings.Contains(err.Error(), "private-registry-value") {
				t.Fatal("error leaked secret")
			}
			if name == "existing" {
				contents, _ := os.ReadFile(filepath.Join(options.DataDir, "sentinel"))
				if string(contents) != "keep" {
					t.Fatal("existing directory changed")
				}
			} else if _, err := os.Lstat(options.DataDir); !os.IsNotExist(err) {
				t.Fatal("failed recovery published destination")
			}
		})
	}
}

func TestRecoveryRequiresRecordedKeyWithoutSecretVersions(t *testing.T) {
	options := recoveryFixture(t, false)
	options.SecretKey = ""
	result, err := InspectBackup(context.Background(), options)
	if err != nil || !result.SecretKeyRequired || result.Ready {
		t.Fatalf("metadata key lost: %+v %v", result, err)
	}
}

func TestConcurrentRecoveryDoesNotClobberWinner(t *testing.T) {
	options := recoveryFixture(t, true)
	results := make(chan error, 2)
	for range 2 {
		go func() { _, err := Restore(context.Background(), options); results <- err }()
	}
	first, second := <-results, <-results
	if (first == nil) == (second == nil) {
		t.Fatalf("expected one exclusive winner: %v, %v", first, second)
	}
	state, err := store.OpenInitialized(context.Background(), filepath.Join(options.DataDir, DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	stored, err := state.Cluster(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadControllerCredentials(options.DataDir, stored, bootstrapTestTime); err != nil {
		t.Fatal(err)
	}
}
