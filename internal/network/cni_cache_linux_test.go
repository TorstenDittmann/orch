//go:build linux

package network

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/containernetworking/cni/libcni"
)

// cachedManager builds a prepared manager over cacheDir without running CNI
// plugins, the way a restarted agent finds its CNI cache on disk.
func cachedManager(t *testing.T, cacheDir string) *linuxManager {
	t.Helper()
	list, err := libcni.ConfListFromBytes([]byte(`{"cniVersion":"` + cniSpecVersion + `","name":"orch-test","plugins":[{"type":"bridge"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return &linuxManager{
		cni:    libcni.NewCNIConfigWithCacheDir(nil, cacheDir, nil),
		list:   list,
		config: model.NetworkConfig{Generation: 1, NodeSubnet: "10.88.0.0/24", Gateway: "10.88.0.1"},
	}
}

func writeCachedResult(t *testing.T, cacheDir string, request AttachmentRequest, address string) string {
	t.Helper()
	path := filepath.Join(cacheDir, "results", "orch-test-"+request.ObjectID+"-"+request.IfName)
	data, err := json.Marshal(map[string]any{
		"kind": "cniCacheV1", "containerId": request.ObjectID, "ifName": request.IfName, "networkName": "orch-test",
		"result": map[string]any{"cniVersion": cniSpecVersion, "ips": []any{map[string]any{"address": address}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A restarted agent must treat a missing CNI cache entry as a missing
// attachment. libcni returns a nil result without an error for it, which used
// to be dereferenced and crashed the agent on every restart.
func TestInspectTreatsMissingCNICacheAsNotFound(t *testing.T) {
	cacheDir := t.TempDir()
	request := AttachmentRequest{AllocationID: "alloc_1", ObjectID: "orch-alloc_1-abc", SpecDigest: "abc",
		NetworkGeneration: 1, IfName: "eth0"}

	// Before the restart the cache entry exists and is read back.
	path := writeCachedResult(t, cacheDir, request, "10.88.0.2/24")
	attachment, err := cachedManager(t, cacheDir).Inspect(context.Background(), request)
	if err != nil || attachment.Address.String() != "10.88.0.2" {
		t.Fatalf("Inspect with cache = %+v, %v", attachment, err)
	}

	// The agent and runtime restart and the entry is gone.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	restarted := cachedManager(t, cacheDir)
	if _, err := restarted.Inspect(context.Background(), request); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Inspect without cache error = %v, want ErrNotFound", err)
	}
	if _, err := restarted.cachedResult(request); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cachedResult without cache error = %v, want ErrNotFound", err)
	}
	if _, err := restarted.attachmentFromResult(request, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("attachmentFromResult(nil) error = %v, want ErrNotFound", err)
	}
}
