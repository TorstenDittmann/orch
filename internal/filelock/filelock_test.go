package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTryLockIsExclusiveUntilUnlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	first, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	if err := TryLock(first); err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if err := TryLock(second); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock = %v, want ErrLocked", err)
	}
	if err := Unlock(first); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if err := TryLock(second); err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
}
