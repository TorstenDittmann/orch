package target

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	clusterctl "change.codes/torsten/better-swarm/internal/cluster"
)

func TestImportUseAndTransport(t *testing.T) {
	root := t.TempDir()
	contextPath := bootstrapContext(t, root)
	store, err := Open(filepath.Join(root, "config", "orch"))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := store.Import("production", contextPath)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Name != "production" || resolved.Transport.Type != TransportDirect {
		t.Fatalf("unexpected target: %#v", resolved)
	}
	if resolved.ContextPath == contextPath {
		t.Fatal("import retained the source context path")
	}
	if err := store.Use("production"); err != nil {
		t.Fatal(err)
	}
	current, err := store.Current()
	if err != nil || current.Name != "production" {
		t.Fatalf("current target: %#v, %v", current, err)
	}
	transport := Transport{Type: TransportWG, Address: "10.20.0.2:9443"}
	if err := store.SetTransport("production", transport); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Get("production")
	if err != nil || updated.Transport != transport {
		t.Fatalf("updated target: %#v, %v", updated, err)
	}
	if _, err := store.Import("production", contextPath); err == nil {
		t.Fatal("duplicate import succeeded")
	}
	listed, err := store.List()
	if err != nil || len(listed) != 1 || listed[0].Name != "production" {
		t.Fatalf("listed targets: %#v, %v", listed, err)
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{
			filepath.Join(root, "config", "orch"),
			filepath.Join(root, "config", "orch", "targets"),
			filepath.Dir(updated.ContextPath),
			filepath.Join(filepath.Dir(updated.ContextPath), "identities"),
			updated.ContextPath,
			filepath.Join(root, "config", "orch", "current.json"),
		} {
			info, statErr := os.Stat(path)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if info.Mode().Perm()&0o077 != 0 {
				t.Fatalf("%s has public permissions %04o", path, info.Mode().Perm())
			}
		}
	}
}

func TestReadersObserveCompleteAtomicTargets(t *testing.T) {
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "config", "orch"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Import("production", bootstrapContext(t, root)); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errorsFound := make(chan error, 64)
	for reader := 0; reader < 16; reader++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < 20; iteration++ {
				resolved, readErr := store.Get("production")
				if readErr != nil {
					errorsFound <- readErr
					return
				}
				if resolved.Name != "production" || resolved.ContextPath == "" {
					errorsFound <- errors.New("reader observed an incomplete target")
					return
				}
			}
		}()
	}
	wait.Wait()
	close(errorsFound)
	for readErr := range errorsFound {
		t.Fatal(readErr)
	}
}

func TestGetRejectsCorruptAndUnsupportedManifest(t *testing.T) {
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "config", "orch"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Import("production", bootstrapContext(t, root)); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, "config", "orch", "targets", "production", "target.json")
	if err := os.WriteFile(manifest, []byte(`{"version":"future","name":"production","context":"context.json","transport":{"type":"direct"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("production"); err == nil {
		t.Fatal("unsupported target manifest succeeded")
	}
	if err := os.WriteFile(manifest, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("production"); err == nil {
		t.Fatal("corrupt target manifest succeeded")
	}
}

func TestImportRejectsUnsafeInputs(t *testing.T) {
	root := t.TempDir()
	contextPath := bootstrapContext(t, root)
	store, err := Open(filepath.Join(root, "config", "orch"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "../escape", "Uppercase", "-option", "a/b"} {
		if _, err := store.Import(name, contextPath); err == nil {
			t.Fatalf("unsafe target name %q succeeded", name)
		}
	}
	link := filepath.Join(root, "context-link.json")
	if err := os.Symlink(contextPath, link); err == nil {
		if _, err := store.Import("linked", link); err == nil {
			t.Fatal("symlinked context import succeeded")
		}
	}
	if _, err := store.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing target error = %v", err)
	}
}

func TestValidateTransport(t *testing.T) {
	valid := []Transport{
		{Type: TransportDirect},
		{Type: TransportWG, Address: "[fd00::2]:9443"},
		{Type: TransportSSH, SSHHost: "prod-controller"},
		{Type: TransportSSH, SSHHost: "user@example", SSHDestination: "127.0.0.1:9443"},
	}
	for _, transport := range valid {
		if err := ValidateTransport(transport); err != nil {
			t.Fatalf("valid transport %#v: %v", transport, err)
		}
	}
	invalid := []Transport{
		{}, {Type: TransportDirect, Address: "host:443"},
		{Type: TransportWG, Address: "https://host:443"},
		{Type: TransportWG, Address: "host"},
		{Type: TransportSSH, SSHHost: "-oProxyCommand=bad"},
		{Type: TransportSSH, SSHHost: "host\ncommand"},
		{Type: TransportSSH, SSHHost: "host", SSHDestination: "missing-port"},
	}
	for _, transport := range invalid {
		if err := ValidateTransport(transport); err == nil {
			t.Fatalf("invalid transport %#v succeeded", transport)
		}
	}
}

func bootstrapContext(t *testing.T, root string) string {
	t.Helper()
	contextPath := filepath.Join(root, "source", "context.json")
	_, err := clusterctl.Bootstrap(context.Background(), clusterctl.BootstrapOptions{
		Name: "test", Server: "https://controller.example:9443",
		DataDir: filepath.Join(root, "state"), ContextPath: contextPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	return contextPath
}
