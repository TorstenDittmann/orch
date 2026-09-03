package ingress

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewCaddySeparatesRootStateFromIngressData(t *testing.T) {
	directory := t.TempDir()
	dataDir := filepath.Join(directory, "data")
	stateDir := filepath.Join(directory, "state")
	caddy, err := NewCaddy(CaddyOptions{
		Binary: "/usr/bin/caddy", AdminSocket: filepath.Join(dataDir, "admin.sock"),
		DataDir: dataDir, StateDir: stateDir, User: "orch-ingress",
	})
	if err != nil {
		t.Fatal(err)
	}
	if caddy.options.StateDir != stateDir {
		t.Fatalf("state dir = %q", caddy.options.StateDir)
	}
	info, err := os.Lstat(stateDir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("state directory = %#v, %v", info, err)
	}
	if _, err := NewCaddy(CaddyOptions{
		Binary: "/usr/bin/caddy", AdminSocket: filepath.Join(dataDir, "admin.sock"),
		DataDir: dataDir, StateDir: filepath.Join(dataDir, "agent"), User: "orch-ingress",
	}); err == nil {
		t.Fatal("NewCaddy accepted agent state beneath the Caddy-writable directory")
	}
}

func TestAgentStateFilesAreAtomicPrivateAndBounded(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "state.json")
	if err := publishAgentFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	contents, err := readAgentFile(path, 16)
	if err != nil || string(contents) != "{}\n" {
		t.Fatalf("readAgentFile() = %q, %v", contents, err)
	}
	if _, err := readAgentFile(path, 1); err == nil {
		t.Fatal("readAgentFile accepted an oversized file")
	}
	unsafe := filepath.Join(directory, "unsafe")
	if err := os.Symlink(path, unsafe); err != nil {
		t.Fatal(err)
	}
	if _, err := readAgentFile(unsafe, 16); err == nil {
		t.Fatal("readAgentFile followed a symlink")
	}
}

func TestEmptyRouteApplyFailureIsNotMasked(t *testing.T) {
	statuses, err := applyFailure(nil, time.Now(), "caddy_unavailable", errors.New("offline"))
	if err == nil || len(statuses) != 0 {
		t.Fatalf("applyFailure() = %#v, %v", statuses, err)
	}
}
