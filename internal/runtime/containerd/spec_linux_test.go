//go:build linux

package containerd

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"

	"change.codes/torsten/better-swarm/internal/model"
	orchruntime "change.codes/torsten/better-swarm/internal/runtime"
)

func TestProcessOwnerUsesResolvedProcessUser(t *testing.T) {
	t.Parallel()
	uid, gid, err := processOwner(&oci.Spec{Process: &specs.Process{User: specs.User{UID: 65532, GID: 65533}}})
	if err != nil || uid != 65532 || gid != 65533 {
		t.Fatalf("processOwner = %d, %d, %v", uid, gid, err)
	}
	if _, _, err := processOwner(&oci.Spec{}); err == nil {
		t.Fatal("processOwner accepted a spec without a process")
	}
}

func TestApplyWorkloadOwnershipHandsSecretsAndNewVolumesToProcessUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing ownership requires root")
	}
	t.Parallel()
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	volumes := filepath.Join(root, "volumes")
	allocation := model.AllocationDesired{
		ID: "alloc-1", SpecDigest: "sha256:abc", Application: "hello",
		SecretFiles: []model.SecretFileDesired{{Target: "/run/secret"}},
		Mounts:      []model.VolumeMount{{Name: "fresh"}, {Name: "data"}, {Name: "claimed"}, {Name: "missing"}},
	}
	objectDir := filepath.Join(secrets, orchruntime.ObjectID(allocation.ID, allocation.SpecDigest))
	applicationDir := filepath.Join(volumes, "hello")
	for _, dir := range []string{objectDir, filepath.Join(applicationDir, "fresh"), filepath.Join(applicationDir, "data"), filepath.Join(applicationDir, "claimed")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"value-0", ".orch-owned"} {
		if err := os.WriteFile(filepath.Join(objectDir, name), []byte("secret"), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(applicationDir, "data", "existing"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(filepath.Join(applicationDir, "claimed"), 2000, 2000); err != nil {
		t.Fatal(err)
	}
	client := &runtimeClient{options: orchruntime.Options{SecretsDir: secrets, VolumesDir: volumes}}
	spec := &oci.Spec{Process: &specs.Process{User: specs.User{UID: 1000, GID: 1001}}}
	if err := client.applyWorkloadOwnership(allocation, spec); err != nil {
		t.Fatal(err)
	}
	assertOwner(t, filepath.Join(objectDir, "value-0"), 1000, 1001, 0o400)
	assertOwner(t, filepath.Join(objectDir, ".orch-owned"), 0, 0, 0o400)
	assertOwner(t, objectDir, 0, 0, 0o700)
	assertOwner(t, filepath.Join(applicationDir, "fresh"), 1000, 1001, 0o700)
	assertOwner(t, filepath.Join(applicationDir, "data"), 0, 0, 0o700)
	assertOwner(t, filepath.Join(applicationDir, "data", "existing"), 0, 0, 0o600)
	assertOwner(t, filepath.Join(applicationDir, "claimed"), 2000, 2000, 0o700)
	assertOwner(t, applicationDir, 0, 0, 0o700)
}

func TestClaimVolumeRootKeepsRootWorkloadsRootOwned(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("inspecting root ownership requires root")
	}
	t.Parallel()
	volume := filepath.Join(t.TempDir(), "fresh")
	if err := os.Mkdir(volume, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := claimVolumeRoot(volume, 0, 0); err != nil {
		t.Fatal(err)
	}
	assertOwner(t, volume, 0, 0, 0o700)
}

func assertOwner(t *testing.T, path string, uid, gid uint32, mode os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid != uid || stat.Gid != gid || info.Mode().Perm() != mode {
		t.Fatalf("%s owner = %d:%d mode %v, want %d:%d mode %v", path, stat.Uid, stat.Gid, info.Mode().Perm(), uid, gid, mode)
	}
}
