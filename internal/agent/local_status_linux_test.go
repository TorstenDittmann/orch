//go:build linux

package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalReceiptRejectsStaleBootAndUnsafePermissions(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	status := LocalStatus{NodeID: "test", PID: os.Getpid(), BootID: strings.TrimSpace(string(boot)), UpdatedAt: now, Ready: true, Connected: true}
	path := filepath.Join(dir, LocalStatusFilename)
	write := func() {
		t.Helper()
		data, _ := json.Marshal(status)
		if err := writePrivateAtomic(path, data); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if _, err := ReadLocalStatus(dir, now); err != nil {
		t.Fatal(err)
	}
	status.BootID = "previous-boot"
	write()
	if _, err := ReadLocalStatus(dir, now); err == nil {
		t.Fatal("previous boot accepted")
	}
	status.BootID = strings.TrimSpace(string(boot))
	status.UpdatedAt = now.Add(-16 * time.Second)
	write()
	if _, err := ReadLocalStatus(dir, now); err == nil {
		t.Fatal("stale receipt accepted")
	}
	status.UpdatedAt = now.Add(2 * time.Second)
	write()
	if _, err := ReadLocalStatus(dir, now); err == nil {
		t.Fatal("future receipt accepted")
	}
	status.UpdatedAt = now
	write()
	os.Chmod(path, 0644)
	if _, err := ReadLocalStatus(dir, now); err == nil {
		t.Fatal("public receipt accepted")
	}
}
