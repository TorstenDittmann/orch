package host

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinkCommandPrefersTheControllerAndKeepsOperatorFiles(t *testing.T) {
	commandLinkPath = filepath.Join(t.TempDir(), "bin", "orch")
	t.Cleanup(func() { commandLinkPath = "/usr/local/bin/orch" })
	node, controller := Prefix+"/bin/orch", ControllerPrefix+"/bin/orch"
	link := func() string {
		t.Helper()
		target, err := os.Readlink(commandLinkPath)
		if err != nil {
			t.Fatal(err)
		}
		return target
	}

	if changed, err := linkCommand(node, false); err != nil || !changed || link() != node {
		t.Fatalf("first link = %t, %v, %s", changed, err, link())
	}
	if changed, err := linkCommand(node, false); err != nil || changed {
		t.Fatalf("repeated link = %t, %v", changed, err)
	}
	if changed, err := linkCommand(controller, true); err != nil || !changed || link() != controller {
		t.Fatalf("controller link = %t, %v, %s", changed, err, link())
	}
	if err := os.Remove(commandLinkPath); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(t.TempDir(), "orch")
	if err := os.WriteFile(existing, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(existing, commandLinkPath); err != nil {
		t.Fatal(err)
	}
	if changed, err := linkCommand(node, true); err != nil || changed || link() != existing {
		t.Fatalf("foreign link replaced = %t, %v, %s", changed, err, link())
	}
	if err := os.Remove(commandLinkPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(commandLinkPath, []byte("operator"), 0o755); err != nil {
		t.Fatal(err)
	}
	if changed, err := linkCommand(node, true); err != nil || changed {
		t.Fatalf("operator file replaced = %t, %v", changed, err)
	}
}

func TestLinkCommandReplacesADanglingLinkToTheOtherInstall(t *testing.T) {
	commandLinkPath = filepath.Join(t.TempDir(), "orch")
	t.Cleanup(func() { commandLinkPath = "/usr/local/bin/orch" })
	if err := os.Symlink(ControllerPrefix+"/bin/orch", commandLinkPath); err != nil {
		t.Fatal(err)
	}
	if changed, err := linkCommand(Prefix+"/bin/orch", false); err != nil || !changed {
		t.Fatalf("dangling link kept = %t, %v", changed, err)
	}
}
