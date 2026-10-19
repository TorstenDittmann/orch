package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/buildinfo"
)

func sendChunks(chunks ...string) func(func([]byte) error) error {
	return func(write func([]byte) error) error {
		for _, chunk := range chunks {
			if err := write([]byte(chunk)); err != nil {
				return err
			}
		}
		return nil
	}
}

func TestStageBinaryChecksSizeAndDigest(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "staged")
	digest := sha256.Sum256([]byte("new orch"))
	sum := hex.EncodeToString(digest[:])
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "orch-old"), []byte("old"), 0700); err != nil {
		t.Fatal(err)
	}

	path, err := stageBinary(directory, sum, 8, sendChunks("new ", "orch"))
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "new orch" || filepath.Base(path) != "orch-"+sum {
		t.Fatalf("staged %s = %q, %v", path, data, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0022 != 0 || info.Mode().Perm()&0100 == 0 {
		t.Fatalf("staged mode = %v, %v", info.Mode(), err)
	}
	if _, err := os.Stat(filepath.Join(directory, "orch-old")); !os.IsNotExist(err) {
		t.Fatal("an earlier candidate was kept")
	}

	for name, receive := range map[string]func(func([]byte) error) error{
		"wrong digest": sendChunks("old orch"),
		"short":        sendChunks("new"),
		"too long":     sendChunks("new orch", "!"),
	} {
		if _, err := stageBinary(directory, sum, 8, receive); err == nil {
			t.Errorf("%s binary was staged", name)
		}
		entries, _ := os.ReadDir(directory)
		if len(entries) != 0 {
			t.Errorf("%s left %d files", name, len(entries))
		}
	}
}

func TestHandleUpgradeStartsOnlyForAnotherBuild(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	command := &orchv1alpha1.UpgradeCommand{Version: "9.9.9", BuildId: "next", Sha256: sum, SizeBytes: 10}
	for _, invalid := range []*orchv1alpha1.UpgradeCommand{
		nil,
		{Version: "9.9.9", Sha256: sum},
		{Version: "9.9.9", Sha256: "abc", SizeBytes: 10},
		{Version: "9.9.9", Sha256: strings.ToUpper(sum), SizeBytes: 10},
		{Sha256: sum, SizeBytes: 10},
	} {
		if err := (&sessionRunner{selfUpgrade: true}).handleUpgrade(context.Background(), invalid); err == nil {
			t.Errorf("invalid command %v was accepted", invalid)
		}
	}

	runner := &sessionRunner{}
	if err := runner.handleUpgrade(context.Background(), command); err != nil || runner.upgrading.Load() {
		t.Fatalf("unmanaged agent started an upgrade: %v", err)
	}
	runner = &sessionRunner{selfUpgrade: true}
	same := &orchv1alpha1.UpgradeCommand{Version: buildinfo.Version, BuildId: buildinfo.BuildID, Sha256: sum, SizeBytes: 10}
	if err := runner.handleUpgrade(context.Background(), same); err != nil || runner.upgrading.Load() {
		t.Fatalf("upgrade to the running build started: %v", err)
	}
	runner.upgrading.Store(true)
	if err := runner.handleUpgrade(context.Background(), command); err != nil {
		t.Fatalf("second upgrade while one runs = %v", err)
	}
}
