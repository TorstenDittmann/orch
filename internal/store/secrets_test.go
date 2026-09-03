package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"change.codes/torsten/better-swarm/internal/security"
)

func TestApplicationSecretsAreVersionedEncryptedAndIdempotent(t *testing.T) {
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
	plaintext := []byte("uniquely-sensitive-value")
	first, err := state.SetApplicationSecret(ctx, box, "hello", "token", "secv_one", plaintext, "idempotency-key-one", testCreatedAt)
	if err != nil || !first.Changed {
		t.Fatalf("SetApplicationSecret() = %#v, %v", first, err)
	}
	replayed, err := state.SetApplicationSecret(ctx, box, "hello", "token", "secv_unused", plaintext, "idempotency-key-one", testCreatedAt.Add(time.Minute))
	if err != nil || replayed.VersionID != first.VersionID || replayed.Changed != first.Changed {
		t.Fatalf("idempotent replay = %#v, %v", replayed, err)
	}
	if _, err := state.SetApplicationSecret(ctx, box, "hello", "token", "secv_conflict", []byte("different"), "idempotency-key-one", testCreatedAt); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	resolved, err := state.ResolveApplicationSecret(ctx, box, "hello", "token")
	if err != nil || resolved.VersionID != first.VersionID || !bytes.Equal(resolved.Value, plaintext) {
		t.Fatalf("ResolveApplicationSecret() = %#v, %v", resolved, err)
	}
	hasEncrypted, err := state.HasEncryptedMaterial(ctx)
	if err != nil || !hasEncrypted {
		t.Fatalf("HasEncryptedMaterial() = %t, %v", hasEncrypted, err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		contents, readErr := os.ReadFile(state.path + suffix)
		if readErr == nil && bytes.Contains(contents, plaintext) {
			t.Fatalf("plaintext secret appears in controller database%s", suffix)
		}
	}
}

func TestRegistryCredentialsAreExactHostScoped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	box, err := security.NewSecretBox(bytes.Repeat([]byte{3}, security.SecretKeySize), bytes.NewReader(bytes.Repeat([]byte{4}, 1024)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := state.SetRegistryCredential(ctx, box, "registry.example:5443", "robot", "regv_one", []byte("token"), "registry-key-one", testCreatedAt)
	if err != nil || result.Host != "registry.example:5443" {
		t.Fatalf("SetRegistryCredential() = %#v, %v", result, err)
	}
	resolved, err := state.ResolveRegistryCredential(ctx, box, "registry.example:5443")
	if err != nil || resolved.Username != "robot" || string(resolved.Secret) != "token" {
		t.Fatalf("ResolveRegistryCredential() = %#v, %v", resolved, err)
	}
	if _, err := state.ResolveRegistryCredential(ctx, box, "other.example:5443"); !errors.Is(err, ErrRegistryCredentialNotFound) {
		t.Fatalf("cross-host resolve = %v", err)
	}
}
