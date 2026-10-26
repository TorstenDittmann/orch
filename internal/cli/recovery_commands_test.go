package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/objectstore/s3test"
	"github.com/TorstenDittmann/orch/internal/security"
	"github.com/TorstenDittmann/orch/internal/store"
)

func TestClusterRestoreFromBucketTakesTheNewestBackup(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	initialized, err := clusterctl.Bootstrap(ctx, clusterctl.BootstrapOptions{
		Name: "test", Server: "https://controller.example", DataDir: filepath.Join(root, "state"),
		ContextPath: filepath.Join(root, "config", "context.json"), Random: rand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	box, _, err := security.LoadOrCreateSecretBox(filepath.Join(initialized.DataDir, clusterctl.SecretKeyFilename), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.OpenInitialized(ctx, filepath.Join(initialized.DataDir, clusterctl.DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if err := state.EnsureSecretKeyFingerprint(ctx, box.Fingerprint(), time.Now()); err != nil {
		t.Fatal(err)
	}
	bucket := s3test.New("backups")
	defer bucket.Close()
	// The newer backup was copied after the destination got a recovery key.
	recoveryKey, recoveryPublicKey, err := security.NewRecoveryKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	older := time.Date(2026, 10, 16, 3, 0, 0, 0, time.UTC)
	for _, created := range []time.Time{older, older.AddDate(0, 0, 1)} {
		set, err := clusterctl.WriteBackupSet(ctx, state, initialized.DataDir, created)
		if err != nil {
			t.Fatal(err)
		}
		var archive bytes.Buffer
		if err := clusterctl.ArchiveBackupSet(set.Path, &archive); err != nil {
			t.Fatal(err)
		}
		object := archive.Bytes()
		if created.After(older) {
			var encrypted bytes.Buffer
			if err := security.EncryptBackup(&encrypted, &archive, recoveryPublicKey, rand.Reader); err != nil {
				t.Fatal(err)
			}
			object = encrypted.Bytes()
		}
		bucket.SetObject("prod/controller/"+filepath.Base(set.Path)+".tar.gz", object)
	}
	// Objects that are not backups are passed over.
	bucket.SetObject("prod/controller/notes.tar.gz", []byte("not an archive"))
	bucket.SetObject("prod/controller/20261099T030000Z.tar.gz", []byte("not a timestamp"))
	t.Setenv("AWS_ACCESS_KEY_ID", "id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	run := func(restore bool, args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		args = append([]string{"--from", "s3://backups/prod", "--endpoint", bucket.URL, "--region", "us-east-1"}, args...)
		if code := runClusterRecovery(ctx, args, restore, &stdout, &stderr); code != ExitOK {
			t.Fatalf("%v: code %d, stderr %s", args, code, stderr.String())
		}
		return stderr.String() + stdout.String()
	}
	fail := func(want string, args ...string) {
		t.Helper()
		var stderr bytes.Buffer
		args = append([]string{"--from", "s3://backups/prod", "--endpoint", bucket.URL, "--region", "us-east-1"}, args...)
		if code := runClusterRecovery(withCommandInput(ctx, strings.NewReader("")), args, false, &bytes.Buffer{}, &stderr); code == ExitOK || !strings.Contains(stderr.String(), want) {
			t.Fatalf("%v: code %d, stderr %s; want %q", args, code, stderr.String(), want)
		}
	}
	fail("ORCH_RECOVERY_KEY")
	other, _, _ := security.NewRecoveryKey(rand.Reader)
	t.Setenv("ORCH_RECOVERY_KEY", other)
	fail("recovery key does not open")
	t.Setenv("ORCH_RECOVERY_KEY", recoveryKey)
	recovered := filepath.Join(root, "recovered")
	output := run(true, "--data-dir", recovered)
	if !strings.Contains(output, "Downloaded controller backup 20261017T030000Z") || !strings.Contains(output, initialized.Cluster.ID) {
		t.Fatalf("restore output:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(recovered, clusterctl.DatabaseFilename)); err != nil {
		t.Fatal(err)
	}
	if output := run(false, "--backup", "20261016T030000Z"); !strings.Contains(output, "Downloaded controller backup 20261016T030000Z") {
		t.Fatalf("inspect output:\n%s", output)
	}
	// The download is staged next to the data directory and cleaned up.
	if entries, _ := filepath.Glob(filepath.Join(root, ".orch-bucket-*")); len(entries) != 0 {
		t.Fatalf("staged download left behind: %v", entries)
	}
	var stderr bytes.Buffer
	if code := runClusterRecovery(ctx, []string{"--from", "s3://backups/prod", "--endpoint", bucket.URL, "--region", "us-east-1",
		"--backup", "20261001T030000Z"}, false, &bytes.Buffer{}, &stderr); code == ExitOK || !strings.Contains(stderr.String(), "has no controller backup") {
		t.Fatalf("a missing backup name: code %d, stderr %s", code, stderr.String())
	}
}
