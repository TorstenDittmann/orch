//go:build linux

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestVolumeBackupViewRefusesSymlinksOutOfTheVolume(t *testing.T) {
	if !canMountVolumes() {
		t.Skip("needs root to bind-mount the backup view")
	}
	host := t.TempDir()
	secret := filepath.Join(host, "secret")
	if err := os.WriteFile(secret, []byte("host only"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "data"), []byte("volume"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"file-link": secret, "dir-link": host} {
		if err := os.Symlink(target, filepath.Join(source, name)); err != nil {
			t.Fatal(err)
		}
	}
	view, release, err := volumeBackupView(source, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := release(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(view); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("view mountpoint left behind: %v", err)
		}
	}()
	if contents, err := os.ReadFile(filepath.Join(view, "data")); err != nil || string(contents) != "volume" {
		t.Fatalf("read volume data = %q, %v", contents, err)
	}
	for _, name := range []string{"file-link", "dir-link/secret"} {
		if contents, err := os.ReadFile(filepath.Join(view, name)); err == nil {
			t.Fatalf("read %s through the backup view = %q, want refused", name, contents)
		}
	}
}

func TestOpenRegularDoesNotBlockOnAFIFO(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		file, err := openRegular(fifo)
		if file != nil {
			_ = file.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errNotRegular) {
			t.Fatalf("openRegular(fifo) = %v, want errNotRegular", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("openRegular blocked on a FIFO")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	if _, err := openRegular(link); !errors.Is(err, errNotRegular) {
		t.Fatalf("openRegular(symlink) = %v, want errNotRegular", err)
	}
}
