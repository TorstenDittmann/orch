//go:build linux

package host

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestAgentMetadataRecoveryPreservesVolumeOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing ownership requires root")
	}
	root := t.TempDir()
	live, backup := filepath.Join(root, "agent"), filepath.Join(root, "backup")
	for _, dir := range []string{"pki", "volumes/emby"} {
		if err := os.MkdirAll(filepath.Join(live, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	volume := filepath.Join(live, "volumes/emby")
	file := filepath.Join(volume, "config")
	if err := os.WriteFile(file, []byte("application-data"), 0640); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{volume, file} {
		if err := os.Chown(path, 1000, 100); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(volume, 0750); err != nil {
		t.Fatal(err)
	}
	if err := snapshotAgentMetadata(live, backup); err != nil {
		t.Fatal(err)
	}
	if err := restoreAgentMetadata(backup, live); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{volume, file} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		stat := info.Sys().(*syscall.Stat_t)
		mode := os.FileMode(0640)
		if path == volume {
			mode = 0750
		}
		if stat.Uid != 1000 || stat.Gid != 100 || info.Mode().Perm() != mode {
			t.Fatalf("ownership/mode changed: %s: %d:%d %o", path, stat.Uid, stat.Gid, info.Mode().Perm())
		}
	}
}
