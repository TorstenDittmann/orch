package store

import (
	"bytes"
	"context"
	"testing"

	"github.com/TorstenDittmann/orch/internal/objectstore"
	"github.com/TorstenDittmann/orch/internal/security"
)

func TestBackupDestinationIsEncryptedAndReplaced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	box, err := security.NewSecretBox(bytes.Repeat([]byte{7}, security.SecretKeySize), bytes.NewReader(bytes.Repeat([]byte{9}, 1024)))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.EnsureSecretKeyFingerprint(ctx, box.Fingerprint(), testCreatedAt); err != nil {
		t.Fatal(err)
	}
	if _, found, err := state.BackupDestination(ctx, box); err != nil || found {
		t.Fatalf("BackupDestination() before set = %t, %v", found, err)
	}
	first := objectstore.Destination{Bucket: "backups", Prefix: "prod", Region: "auto",
		Endpoint: "https://abc.r2.cloudflarestorage.com", AccessKeyID: "key-one", SecretAccessKey: "secret-one"}
	if err := state.SetBackupDestination(ctx, box, first, testCreatedAt); err != nil {
		t.Fatal(err)
	}
	second := first
	second.AccessKeyID, second.SecretAccessKey = "key-two", "secret-two"
	if err := state.SetBackupDestination(ctx, box, second, testCreatedAt); err != nil {
		t.Fatal(err)
	}
	got, found, err := state.BackupDestination(ctx, box)
	if err != nil || !found || got != second {
		t.Fatalf("BackupDestination() = %+v, %t, %v", got, found, err)
	}
	if encrypted, err := state.HasEncryptedMaterial(ctx); err != nil || !encrypted {
		t.Fatalf("HasEncryptedMaterial() = %t, %v", encrypted, err)
	}
	if err := state.VerifyRecoverySecrets(ctx, box); err != nil {
		t.Fatalf("VerifyRecoverySecrets() = %v", err)
	}
	if deleted, err := state.DeleteBackupDestination(ctx); err != nil || !deleted {
		t.Fatalf("DeleteBackupDestination() = %t, %v", deleted, err)
	}
	if _, found, err := state.BackupDestination(ctx, box); err != nil || found {
		t.Fatalf("BackupDestination() after delete = %t, %v", found, err)
	}
}
