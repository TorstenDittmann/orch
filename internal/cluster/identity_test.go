package cluster

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/store"
)

func TestCredentialLoadersValidateBootstrapIdentity(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	options := BootstrapOptions{
		Name: "test", Server: "https://127.0.0.1:9443",
		DataDir: filepath.Join(root, "state"), ContextPath: filepath.Join(root, "config", "context.json"),
		Now: func() time.Time { return bootstrapTestTime }, Random: rand.Reader,
	}
	result, err := Bootstrap(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.OpenInitialized(context.Background(), filepath.Join(options.DataDir, DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := state.Cluster(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	controller, err := LoadControllerCredentials(options.DataDir, stored, bootstrapTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if controller.Certificate.Leaf == nil || controller.ClientCAs == nil {
		t.Fatal("controller credentials are incomplete")
	}
	administrator, err := LoadAdministratorCredentials(options.ContextPath, bootstrapTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if administrator.Certificate.Leaf == nil || administrator.RootCAs == nil || administrator.Context.Cluster.ID != result.Cluster.ID || administrator.ServerName != "127.0.0.1" {
		t.Fatalf("administrator credentials = %#v", administrator)
	}

	mismatched := stored
	mismatched.ID = "00000000-0000-4000-8000-000000000000"
	if _, err := LoadControllerCredentials(options.DataDir, mismatched, bootstrapTestTime); err == nil || !strings.Contains(err.Error(), "does not contain identity") {
		t.Fatalf("LoadControllerCredentials() = %v, want cluster mismatch", err)
	}
}

func TestAdministratorLoaderAllowsPublicContextParentButProtectsIdentityDirectories(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	contextDirectory := filepath.Join(root, "config")
	contextPath := filepath.Join(contextDirectory, "context.json")
	result, err := Bootstrap(context.Background(), BootstrapOptions{
		Name: "test", Server: "https://controller.example", DataDir: filepath.Join(root, "state"),
		ContextPath: contextPath, Now: func() time.Time { return bootstrapTestTime }, Random: rand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(contextDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAdministratorCredentials(contextPath, bootstrapTestTime); err != nil {
		t.Fatalf("public context parent was rejected: %v", err)
	}
	identityRoot := filepath.Join(contextDirectory, administratorRootDirectory)
	if err := os.Chmod(identityRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAdministratorCredentials(contextPath, bootstrapTestTime); err == nil || !strings.Contains(err.Error(), "group or world access") {
		t.Fatalf("LoadAdministratorCredentials() = %v, want identity directory permission error for %s", err, result.Cluster.ID)
	}
}
