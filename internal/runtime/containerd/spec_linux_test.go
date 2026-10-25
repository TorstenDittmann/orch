//go:build linux

package containerd

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/TorstenDittmann/orch/internal/model"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
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

func TestSpecOptionsApplyDefaultSeccompProfile(t *testing.T) {
	t.Parallel()
	client := &runtimeClient{}
	allocation := model.AllocationDesired{
		Command: []string{"/app"}, CPUMillis: 500, MemoryBytes: 64 << 20,
		Environment: map[string]string{"MODE": "test"},
	}
	// The first option resolves the image config, which needs a real image;
	// the remaining options are exercised on a default generated spec.
	opts := client.specOptions(nil, allocation, nil)[1:]
	ctx := namespaces.WithNamespace(context.Background(), "test")
	spec, err := oci.GenerateSpec(ctx, nil, &containers.Container{ID: "test"}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Linux.Seccomp == nil || spec.Linux.Seccomp.DefaultAction != specs.ActErrno || len(spec.Linux.Seccomp.Syscalls) == 0 {
		t.Fatalf("seccomp = %#v, want containerd default profile", spec.Linux.Seccomp)
	}
	for _, rule := range spec.Linux.Seccomp.Syscalls {
		for _, name := range rule.Names {
			if (name == "mount" || name == "bpf") && rule.Action == specs.ActAllow {
				t.Fatalf("default profile allows %s without capabilities", name)
			}
		}
	}
	if !spec.Process.NoNewPrivileges || len(spec.Process.Capabilities.Bounding) != 0 {
		t.Fatalf("process hardening = %#v", spec.Process)
	}
}

func TestSpecOptionsPrivilegedLiftsConfinement(t *testing.T) {
	t.Parallel()
	client := &runtimeClient{}
	allocation := model.AllocationDesired{
		Command: []string{"/app"}, CPUMillis: 500, MemoryBytes: 64 << 20, Privileged: true,
	}
	opts := client.specOptions(nil, allocation, nil)[1:]
	ctx := namespaces.WithNamespace(context.Background(), "test")
	spec, err := oci.GenerateSpec(ctx, nil, &containers.Container{ID: "test"}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Linux.Seccomp != nil || spec.Process.NoNewPrivileges {
		t.Fatalf("privileged process = %#v, seccomp = %#v", spec.Process, spec.Linux.Seccomp)
	}
	// Privileged replicas get the agent's own capabilities, which only a root
	// agent has.
	if os.Geteuid() == 0 && len(spec.Process.Capabilities.Bounding) == 0 {
		t.Fatalf("privileged capabilities = %#v", spec.Process.Capabilities)
	}
	if len(spec.Linux.Resources.Devices) != 1 || !spec.Linux.Resources.Devices[0].Allow || len(spec.Linux.MaskedPaths) != 0 {
		t.Fatalf("privileged devices = %#v, masked paths = %v", spec.Linux.Resources.Devices, spec.Linux.MaskedPaths)
	}
	if !hasWritableCgroupfs(spec) {
		t.Fatalf("privileged mounts = %#v, want writable /sys/fs/cgroup", spec.Mounts)
	}
}

func TestSpecOptionsCapabilitiesKeepConfinement(t *testing.T) {
	t.Parallel()
	client := &runtimeClient{}
	want := []string{"CAP_CHOWN", "CAP_KILL", "CAP_SETGID", "CAP_SETUID"}
	allocation := model.AllocationDesired{
		Command: []string{"/app"}, CPUMillis: 500, MemoryBytes: 64 << 20, Capabilities: want,
	}
	opts := client.specOptions(nil, allocation, nil)[1:]
	spec, err := oci.GenerateSpec(namespaces.WithNamespace(context.Background(), "test"), nil, &containers.Container{ID: "test"}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	caps := spec.Process.Capabilities
	for _, actual := range [][]string{caps.Bounding, caps.Effective, caps.Permitted} {
		if !slices.Equal(actual, want) {
			t.Fatalf("capabilities = %v, want %v", actual, want)
		}
	}
	if len(caps.Ambient) != 0 || len(caps.Inheritable) != 0 || !spec.Process.NoNewPrivileges || spec.Linux.Seccomp == nil {
		t.Fatalf("capabilities disabled process confinement: %#v", spec.Process)
	}
	if len(spec.Linux.Devices) != 0 || len(spec.Linux.Resources.Devices) != 1 || spec.Linux.Resources.Devices[0].Allow || hasWritableCgroupfs(spec) {
		t.Fatal("capabilities granted host devices or writable cgroups")
	}
	for _, rule := range spec.Linux.Seccomp.Syscalls {
		for _, name := range rule.Names {
			if (name == "mount" || name == "bpf") && rule.Action == specs.ActAllow {
				t.Fatalf("limited capabilities allow %s", name)
			}
		}
	}
}

func TestSpecOptionsUnprivilegedHasNoCgroupfs(t *testing.T) {
	t.Parallel()
	client := &runtimeClient{}
	allocation := model.AllocationDesired{Command: []string{"/app"}, CPUMillis: 500, MemoryBytes: 64 << 20}
	opts := client.specOptions(nil, allocation, nil)[1:]
	ctx := namespaces.WithNamespace(context.Background(), "test")
	spec, err := oci.GenerateSpec(ctx, nil, &containers.Container{ID: "test"}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	for _, mount := range spec.Mounts {
		if mount.Destination == "/sys/fs/cgroup" {
			t.Fatalf("unprivileged mounts include %#v", mount)
		}
	}
}

func TestEnsureVolumeDirectoryAcceptsWorkloadModes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	volume := filepath.Join(root, "docker")
	if err := ensureVolumeDirectory(volume); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(volume); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("new volume = %v, %v; want mode 0700", info, err)
	}
	// dockerd sets 0710 on its data root; a restart must still mount it.
	if err := os.Chmod(volume, 0o710); err != nil {
		t.Fatal(err)
	}
	if err := ensureVolumeDirectory(volume); err != nil {
		t.Fatalf("ensureVolumeDirectory after chmod: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(volume, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureVolumeDirectory(link); err == nil {
		t.Fatal("ensureVolumeDirectory accepted a symlink")
	}
}

func hasWritableCgroupfs(spec *oci.Spec) bool {
	for _, mount := range spec.Mounts {
		if mount.Destination != "/sys/fs/cgroup" || mount.Type != "cgroup" {
			continue
		}
		for _, option := range mount.Options {
			if option == "rw" {
				return true
			}
		}
	}
	return false
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
