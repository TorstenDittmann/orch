package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReceiveStoresABackupCopiedFromAnotherNode(t *testing.T) {
	v := newVolumeBackups(t.TempDir(), nil, nil, nil)
	const name = "20261103T120000Z-manual"
	data := bytes.Repeat([]byte("orch"), 1000)
	if _, err := v.receive("shop", "data", name, 0, data[:1500], uint64(len(data)), false); err != nil {
		t.Fatal(err)
	}
	if backups, err := v.list("shop", "data"); err != nil || len(backups) != 0 {
		t.Fatalf("partial copy listed: %v, %v", backups, err)
	}
	if _, err := v.receive("shop", "data", name, 1000, data[1000:], uint64(len(data)), true); err == nil {
		t.Fatal("a window that does not continue the copy was accepted")
	}
	// The failed window dropped the partial copy, so it has to start over.
	if _, err := v.receive("shop", "data", name, 1500, data[1500:], uint64(len(data)), true); err == nil {
		t.Fatal("a copy continued after it was dropped")
	}
	if _, err := v.receive("shop", "data", name, 0, data[:1500], uint64(len(data)), false); err != nil {
		t.Fatal(err)
	}
	backup, err := v.receive("shop", "data", name, 1500, data[1500:], uint64(len(data)), true)
	if err != nil || backup.Name != name || backup.Size != int64(len(data)) {
		t.Fatalf("receive() = %#v, %v", backup, err)
	}
	path, err := v.path("shop", "data", name)
	if err != nil {
		t.Fatal(err)
	}
	if stored, err := os.ReadFile(path); err != nil || !bytes.Equal(stored, data) {
		t.Fatalf("stored copy differs: %v", err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(v.archiveDir("shop", "data"), ".incoming-*")); len(leftovers) != 0 {
		t.Fatalf("partial files left: %v", leftovers)
	}
}

func TestReceiveRejectsShortCopiesAndBadNames(t *testing.T) {
	v := newVolumeBackups(t.TempDir(), nil, nil, nil)
	if _, err := v.receive("shop", "data", "../escape", 0, []byte("x"), 1, true); err == nil {
		t.Fatal("invalid backup name was accepted")
	}
	if _, err := v.receive("shop", "data", "20261103T120000Z", 0, []byte("abc"), 10, true); err == nil {
		t.Fatal("a copy shorter than its size was published")
	}
	if _, err := v.receive("shop", "data", "20261103T120000Z", 0, []byte("abc"), 2, false); err == nil {
		t.Fatal("data past the size was accepted")
	}
}
