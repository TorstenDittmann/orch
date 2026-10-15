package target

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
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

func TestReplaceSwapsUnusableCredentialsAndKeepsTransport(t *testing.T) {
	root := t.TempDir()
	contextPath := bootstrapContext(t, root)
	store, err := Open(filepath.Join(root, "config", "orch"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Replace("production", contextPath); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replace of missing target = %v", err)
	}
	if _, err := store.Import("production", contextPath); err != nil {
		t.Fatal(err)
	}
	transport := Transport{Type: TransportSSH, SSHHost: "prod-controller"}
	if err := store.SetTransport("production", transport); err != nil {
		t.Fatal(err)
	}
	local, err := clusterctl.LoadContext(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	// Stand in for an expired certificate: the stored target no longer loads.
	stored := filepath.Join(root, "config", "orch", "targets", "production", "identities", local.Cluster.ID, "admin.pem")
	if err := os.WriteFile(stored, []byte("expired"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("production"); err == nil {
		t.Fatal("damaged target loaded")
	}
	renewed, err := clusterctl.RenewCertificates(context.Background(), clusterctl.RenewCertificatesOptions{
		DataDir: filepath.Join(root, "state"), AdminDir: filepath.Join(root, "renewed"),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := store.Replace("production", renewed.ContextPath)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Transport != transport {
		t.Fatalf("replaced transport = %#v, want %#v", resolved.Transport, transport)
	}
	entries, err := os.ReadDir(filepath.Join(root, "config", "orch", "targets"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("targets after replace = %v, %v", entries, err)
	}

	other := t.TempDir()
	foreign := bootstrapContext(t, other)
	if _, err := store.Replace("production", foreign); err == nil {
		t.Fatal("replaced a target with another cluster's credentials")
	}
	if _, err := store.Get("production"); err != nil {
		t.Fatalf("refused replace damaged the target: %v", err)
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

func TestAddSocketTargetHasNoCredentials(t *testing.T) {
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "orch"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Current(); !errors.Is(err, ErrNoCurrent) {
		t.Fatalf("Current() before selection = %v", err)
	}
	transport := Transport{Type: TransportSocket, SSHHost: "root@203.0.113.10", SSHPort: 2222, Socket: DefaultAdminSocket}
	cluster := Cluster{ID: "0f6f4b8e-2f43-4b55-9d1c-1f7a3a9c0b11", Name: "production"}
	resolved, err := store.AddSocket("production", transport, cluster)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ContextPath != "" || resolved.Transport != transport || resolved.Cluster != cluster {
		t.Fatalf("socket target = %#v", resolved)
	}
	if _, err := store.AddSocket("production", transport, cluster); err == nil {
		t.Fatal("duplicate socket target succeeded")
	}
	if err := store.SetTransport("production", Transport{Type: TransportDirect}); err == nil {
		t.Fatal("socket target switched to a certificate transport")
	}
	moved := Transport{Type: TransportSocket, SSHHost: "root@203.0.113.20", Socket: DefaultAdminSocket}
	if err := store.SetTransport("production", moved); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Get("production")
	if err != nil || updated.Transport != moved || updated.Cluster != cluster {
		t.Fatalf("updated socket target = %#v, %v", updated, err)
	}
	if _, err := store.AddSocket("broken", transport, Cluster{ID: "not-a-uuid", Name: "x"}); err == nil {
		t.Fatal("invalid cluster identity accepted")
	}
}

func TestValidateSocketTransport(t *testing.T) {
	valid := []Transport{
		{Type: TransportSocket, Socket: "/var/lib/orch-controller/admin.sock"},
		{Type: TransportSocket, Socket: "/run/orch.sock", SSHHost: "prod", SSHPort: 22},
	}
	for _, transport := range valid {
		if err := ValidateTransport(transport); err != nil {
			t.Fatalf("ValidateTransport(%#v) = %v", transport, err)
		}
	}
	invalid := []Transport{
		{Type: TransportSocket},
		{Type: TransportSocket, Socket: "relative.sock"},
		{Type: TransportSocket, Socket: "/a:b.sock", SSHHost: "prod"},
		{Type: TransportSocket, Socket: "/run/orch.sock", SSHPort: 22},
		{Type: TransportSocket, Socket: "/run/orch.sock", SSHHost: "-oProxyCommand=x"},
		{Type: TransportSocket, Socket: "/run/orch.sock", Address: "10.0.0.1:9443"},
		{Type: TransportDirect, Socket: "/run/orch.sock"},
		{Type: TransportSSH, SSHHost: "prod", SSHPort: 22},
	}
	for _, transport := range invalid {
		if err := ValidateTransport(transport); err == nil {
			t.Fatalf("ValidateTransport(%#v) unexpectedly succeeded", transport)
		}
	}
}
