package cluster

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/store"
)

var bootstrapTestTime = time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)

func TestBootstrapCreatesDurableClusterAndIsIdempotent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	options := BootstrapOptions{
		Name: "test", Server: "https://controller.example:9443",
		DataDir: filepath.Join(root, "state"), ContextPath: filepath.Join(root, "config", "context.json"),
		Now: func() time.Time { return bootstrapTestTime }, Random: rand.Reader,
	}
	first, err := Bootstrap(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.Cluster.ID == "" || first.Cluster.Generation != 1 || first.Cluster.CreatedAt != bootstrapTestTime {
		t.Fatalf("first result = %#v", first)
	}
	local, err := LoadContext(options.ContextPath)
	if err != nil {
		t.Fatal(err)
	}
	if local.Cluster.ID != first.Cluster.ID || local.Cluster.Name != "test" || local.Server != "https://controller.example:9443" {
		t.Fatalf("context = %#v", local)
	}
	for _, reference := range []string{local.Credentials.CertificateAuthority, local.Credentials.ClientCertificate, local.Credentials.ClientKey} {
		if filepath.IsAbs(reference) || strings.HasPrefix(reference, "..") {
			t.Fatalf("credential reference is not relocatable: %q", reference)
		}
	}

	sensitivePaths := bootstrapPaths(options, first.Cluster.ID)
	before := make(map[string][]byte, len(sensitivePaths))
	for _, path := range sensitivePaths {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = contents
		assertMode(t, path, 0o600)
	}
	for _, path := range []string{
		options.DataDir,
		filepath.Join(options.DataDir, controllerIdentityDirectory),
		filepath.Join(filepath.Dir(options.ContextPath), administratorRootDirectory),
		filepath.Join(filepath.Dir(options.ContextPath), administratorRootDirectory, first.Cluster.ID),
	} {
		assertMode(t, path, 0o700)
	}

	repeatOptions := options
	repeatOptions.Random = failingReader{}
	second, err := Bootstrap(context.Background(), repeatOptions)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.Cluster != first.Cluster {
		t.Fatalf("second result = %#v, want existing %#v", second, first.Cluster)
	}
	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s changed during idempotent initialization", path)
		}
	}

	state, err := store.Open(context.Background(), filepath.Join(options.DataDir, DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := state.Cluster(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stored != first.Cluster {
		t.Fatalf("stored cluster = %#v, want %#v", stored, first.Cluster)
	}
	events, err := state.Events(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != model.EventClusterInitialized {
		t.Fatalf("events = %#v", events)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := os.ReadFile(filepath.Join(options.DataDir, DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(database, []byte("PRIVATE KEY")) || bytes.Contains(database, []byte("BEGIN CERTIFICATE")) {
		t.Fatal("database contains PEM identity material")
	}
}

func TestBootstrapRandomFailureLeavesNoInitializedState(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	options := BootstrapOptions{
		Name: "test", Server: "https://controller.example",
		DataDir: filepath.Join(root, "state"), ContextPath: filepath.Join(root, "config", "context.json"),
		Now: func() time.Time { return bootstrapTestTime }, Random: failingReader{},
	}
	if _, err := Bootstrap(context.Background(), options); err == nil || !errors.Is(err, errRandomUnavailable) {
		t.Fatalf("Bootstrap() = %v, want random source error", err)
	}
	for _, path := range []string{options.DataDir, options.ContextPath, filepath.Join(root, "config", administratorRootDirectory)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed initialization left %s: %v", path, err)
		}
	}
}

func TestBootstrapAcceptsRelocatedContextTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	options := BootstrapOptions{
		Name: "test", Server: "https://controller.example", DataDir: filepath.Join(root, "state"),
		ContextPath: filepath.Join(root, "config", "context.json"), Now: func() time.Time { return bootstrapTestTime }, Random: rand.Reader,
	}
	first, err := Bootstrap(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	movedDirectory := filepath.Join(root, "relocated")
	if err := os.Rename(filepath.Dir(options.ContextPath), movedDirectory); err != nil {
		t.Fatal(err)
	}
	options.ContextPath = filepath.Join(movedDirectory, "context.json")
	options.Random = failingReader{}
	second, err := Bootstrap(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.Cluster != first.Cluster {
		t.Fatalf("relocated initialization = %#v, want existing %#v", second, first.Cluster)
	}
}

func TestBootstrapRefusesMissingOrMismatchedExistingContext(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	options := BootstrapOptions{
		Name: "test", Server: "https://controller.example",
		DataDir: filepath.Join(root, "state"), ContextPath: filepath.Join(root, "config", "context.json"),
		Now: func() time.Time { return bootstrapTestTime }, Random: rand.Reader,
	}
	result, err := Bootstrap(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	adminKey := filepath.Join(filepath.Dir(options.ContextPath), administratorRootDirectory, result.Cluster.ID, "admin-key.pem")
	keyBefore, err := os.ReadFile(adminKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(options.ContextPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(context.Background(), options); err == nil || !strings.Contains(err.Error(), "administrator context") {
		t.Fatalf("Bootstrap() with missing context = %v", err)
	}
	keyAfter, err := os.ReadFile(adminKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(keyAfter, keyBefore) {
		t.Fatal("missing context caused administrator key replacement")
	}

	options.ContextPath = filepath.Join(root, "config", "other-context.json")
	options.Name = "other"
	if _, err := Bootstrap(context.Background(), options); err == nil || !strings.Contains(err.Error(), "refusing to replace") {
		t.Fatalf("Bootstrap() with mismatched name = %v", err)
	}
}

func TestBootstrapRefusesExistingContextWithoutOverwriting(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	contextPath := filepath.Join(root, "config", "context.json")
	if err := os.MkdirAll(filepath.Dir(contextPath), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := []byte("do not replace")
	if err := os.WriteFile(contextPath, marker, 0o600); err != nil {
		t.Fatal(err)
	}
	options := BootstrapOptions{
		Name: "test", Server: "https://controller.example", DataDir: filepath.Join(root, "state"),
		ContextPath: contextPath, Now: func() time.Time { return bootstrapTestTime }, Random: rand.Reader,
	}
	if _, err := Bootstrap(context.Background(), options); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("Bootstrap() = %v, want no-clobber error", err)
	}
	got, err := os.ReadFile(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, marker) {
		t.Fatalf("existing context changed: %q", got)
	}
}

func TestBootstrapRefusesExistingDatabaseWithoutOverwriting(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dataDir := filepath.Join(root, "state")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(dataDir, DatabaseFilename)
	marker := []byte("existing database")
	if err := os.WriteFile(databasePath, marker, 0o600); err != nil {
		t.Fatal(err)
	}
	options := BootstrapOptions{
		Name: "test", Server: "https://controller.example", DataDir: dataDir,
		ContextPath: filepath.Join(root, "config", "context.json"), Random: rand.Reader,
	}
	if _, err := Bootstrap(context.Background(), options); err == nil {
		t.Fatal("Bootstrap() accepted an unrecognized existing database")
	}
	after, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, marker) {
		t.Fatalf("existing database changed: %q", after)
	}
}

func TestBootstrapRefusesRecognizedButUninitializedDatabase(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dataDir := filepath.Join(root, "state")
	databasePath := filepath.Join(dataDir, DatabaseFilename)
	state, err := store.Open(context.Background(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Bootstrap(context.Background(), BootstrapOptions{
		Name: "test", Server: "https://controller.example", DataDir: dataDir,
		ContextPath: filepath.Join(root, "config", "context.json"), Random: rand.Reader,
	})
	if err == nil || !strings.Contains(err.Error(), "not initialized") || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("Bootstrap() = %v, want safe uninitialized-database error", err)
	}
	after, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("recognized uninitialized database changed")
	}
}

func TestBootstrapRejectsInsecureExistingCredentials(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	options := BootstrapOptions{
		Name: "test", Server: "https://controller.example", DataDir: filepath.Join(root, "state"),
		ContextPath: filepath.Join(root, "config", "context.json"), Now: func() time.Time { return bootstrapTestTime }, Random: rand.Reader,
	}
	result, err := Bootstrap(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	adminKey := filepath.Join(filepath.Dir(options.ContextPath), administratorRootDirectory, result.Cluster.ID, "admin-key.pem")
	if err := os.Chmod(adminKey, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(context.Background(), options); err == nil || !strings.Contains(err.Error(), "group or world access") {
		t.Fatalf("Bootstrap() = %v, want insecure permission error", err)
	}
}

func TestBootstrapRejectsContextForAnotherCluster(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	options := BootstrapOptions{
		Name: "test", Server: "https://controller.example", DataDir: filepath.Join(root, "state"),
		ContextPath: filepath.Join(root, "config", "context.json"), Now: func() time.Time { return bootstrapTestTime }, Random: rand.Reader,
	}
	if _, err := Bootstrap(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	local, err := LoadContext(options.ContextPath)
	if err != nil {
		t.Fatal(err)
	}
	local.Cluster.ID = "00000000-0000-4000-8000-000000000000"
	contents, err := json.Marshal(local)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(options.ContextPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(context.Background(), options); err == nil || !strings.Contains(err.Error(), "does not match existing cluster") {
		t.Fatalf("Bootstrap() = %v, want context mismatch", err)
	}
}

func TestBootstrapValidatesOptionsBeforeWriting(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tests := []struct {
		name    string
		mutate  func(*BootstrapOptions)
		message string
	}{
		{name: "name", mutate: func(options *BootstrapOptions) { options.Name = "Not Valid" }, message: "lowercase DNS label"},
		{name: "scheme", mutate: func(options *BootstrapOptions) { options.Server = "http://controller.example" }, message: "HTTPS"},
		{name: "server path", mutate: func(options *BootstrapOptions) { options.Server = "https://controller.example/api" }, message: "path"},
		{name: "missing data", mutate: func(options *BootstrapOptions) { options.DataDir = "" }, message: "required"},
		{name: "overlap", mutate: func(options *BootstrapOptions) { options.ContextPath = filepath.Join(options.DataDir, "context.json") }, message: "must not overlap"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := BootstrapOptions{
				Name: "test", Server: "https://controller.example", DataDir: filepath.Join(root, test.name, "state"),
				ContextPath: filepath.Join(root, test.name, "config", "context.json"),
			}
			test.mutate(&options)
			_, err := Bootstrap(context.Background(), options)
			if !errors.Is(err, ErrInvalidOptions) || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("Bootstrap() = %v, want invalid options containing %q", err, test.message)
			}
		})
	}
}

func bootstrapPaths(options BootstrapOptions, clusterID string) []string {
	administratorDirectory := filepath.Join(filepath.Dir(options.ContextPath), administratorRootDirectory, clusterID)
	return []string{
		filepath.Join(options.DataDir, DatabaseFilename),
		filepath.Join(options.DataDir, controllerIdentityDirectory, "ca.pem"),
		filepath.Join(options.DataDir, controllerIdentityDirectory, "ca-key.pem"),
		filepath.Join(options.DataDir, controllerIdentityDirectory, "controller.pem"),
		filepath.Join(options.DataDir, controllerIdentityDirectory, "controller-key.pem"),
		options.ContextPath,
		filepath.Join(administratorDirectory, "ca.pem"),
		filepath.Join(administratorDirectory, "admin.pem"),
		filepath.Join(administratorDirectory, "admin-key.pem"),
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s permissions = %04o, want %04o", path, got, want)
	}
}

var errRandomUnavailable = errors.New("randomness unavailable")

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errRandomUnavailable
}

var _ io.Reader = failingReader{}
