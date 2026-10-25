package host

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgentMetadataRecoveryPreservesWorkloadData(t *testing.T) {
	root := t.TempDir()
	live, backup := filepath.Join(root, "agent"), filepath.Join(root, "snapshot")
	for _, dir := range []string{"pki", "volumes/emby", "logs", "network", "runtime-secrets", "backups"} {
		if err := os.MkdirAll(filepath.Join(live, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range agentMetadataFiles {
		if err := os.WriteFile(filepath.Join(live, name), []byte("old:"+name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// The stopped database has no WAL; a candidate may subsequently create one.
	if err := os.Remove(filepath.Join(live, "agent.db-wal")); err != nil {
		t.Fatal(err)
	}
	var preserved []string
	for _, name := range []string{"volumes/emby/media", "logs/output", "network/wireguard.key", "runtime-secrets/password", "backups/archive"} {
		path := filepath.Join(live, name)
		if err := os.WriteFile(path, []byte(name), 0640); err != nil {
			t.Fatal(err)
		}
		preserved = append(preserved, path)
	}
	// A live-volume symlink must not even be examined by snapshotting.
	if err := os.Symlink("media", filepath.Join(live, "volumes/emby/link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(live, "volumes/emby"), 0755); err != nil {
		t.Fatal(err)
	}
	before := make(map[string]os.FileInfo)
	for _, path := range append(preserved, filepath.Join(live, "volumes/emby")) {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = info
	}
	if err := snapshotAgentMetadata(live, backup); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"logs", "volumes"} {
		entries, err := os.ReadDir(filepath.Join(backup, name))
		if err != nil || len(entries) != 0 {
			t.Fatalf("snapshot %s contains live data: %v, %v", name, entries, err)
		}
	}
	for _, name := range []string{"network", "runtime-secrets", "backups"} {
		if _, err := os.Lstat(filepath.Join(backup, name)); !os.IsNotExist(err) {
			t.Fatalf("snapshot includes %s: %v", name, err)
		}
	}
	for _, name := range agentMetadataFiles {
		if err := os.WriteFile(filepath.Join(live, name), []byte("candidate"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := restoreAgentMetadata(backup, live); err != nil {
		t.Fatal(err)
	}
	for _, name := range agentMetadataFiles {
		data, err := os.ReadFile(filepath.Join(live, name))
		if name == "agent.db-wal" {
			if !os.IsNotExist(err) {
				t.Fatalf("candidate WAL survived: %v", err)
			}
			continue
		}
		if err != nil || string(data) != "old:"+name {
			t.Fatalf("restored %s = %q, %v", name, data, err)
		}
	}
	for path, old := range before {
		info, err := os.Stat(path)
		if err != nil || !os.SameFile(old, info) || old.Mode() != info.Mode() {
			t.Fatalf("workload inode/permissions changed: %s", path)
		}
		if !info.IsDir() {
			data, err := os.ReadFile(path)
			relative, _ := filepath.Rel(live, path)
			if err != nil || string(data) != relative {
				t.Fatalf("workload data changed: %s", path)
			}
		}
	}
}

func TestAgentMetadataSnapshotRejectsIdentitySymlinks(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "agent")
	if err := os.MkdirAll(filepath.Join(live, "pki"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(live, "agent.json")); err != nil {
		t.Fatal(err)
	}
	if err := snapshotAgentMetadata(live, filepath.Join(root, "backup")); err == nil {
		t.Fatal("accepted symlink metadata")
	}
}
