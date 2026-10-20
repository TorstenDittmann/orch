//go:build linux

package containerd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/contrib/seccomp"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"github.com/TorstenDittmann/orch/internal/model"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

func (c *runtimeClient) specOptions(image containerdclient.Image, allocation model.AllocationDesired, mounts []specs.Mount) []oci.SpecOpts {
	opts := []oci.SpecOpts{oci.WithImageConfig(image)}
	if len(allocation.Command) != 0 {
		arguments := append(append([]string(nil), allocation.Command...), allocation.Args...)
		opts = append(opts, oci.WithProcessArgs(arguments...))
	} else if len(allocation.Args) != 0 {
		opts[0] = oci.WithImageConfigArgs(image, allocation.Args)
	}
	environment := make([]string, 0, len(allocation.Environment))
	for key, value := range allocation.Environment {
		environment = append(environment, key+"="+value)
	}
	sort.Strings(environment)
	quota := int64(allocation.CPUMillis * 100)
	opts = append(opts,
		oci.WithEnv(environment),
		oci.WithMemoryLimit(allocation.MemoryBytes),
		withNoSwap,
		oci.WithCPUCFS(quota, 100_000),
		oci.WithNoNewPrivileges,
		oci.WithCapabilities(nil),
		// The default profile is derived from the process capabilities, so it
		// must follow WithCapabilities.
		seccomp.WithDefaultProfile(),
		oci.WithMounts(mounts),
		hardenedSpec,
	)
	if allocation.Privileged {
		// These must follow hardenedSpec, which denies devices and new
		// privileges for every other workload.
		opts = append(opts, oci.WithPrivileged, oci.WithAllDevicesAllowed, oci.WithHostDevices, oci.WithNewPrivileges)
	}
	if len(allocation.SecretEnvironment) != 0 {
		opts = append(opts, withSecretEnvironment(allocation.SecretEnvironment))
	}
	opts = append(opts, c.withWorkloadOwnership(allocation))
	return opts
}

// withWorkloadOwnership hands the host-side files the workload must access to
// the process user resolved from the image config. It must follow
// WithImageConfig so that USER directives (including named users) are resolved.
func (c *runtimeClient) withWorkloadOwnership(allocation model.AllocationDesired) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, spec *oci.Spec) error {
		return c.applyWorkloadOwnership(allocation, spec)
	}
}

func (c *runtimeClient) applyWorkloadOwnership(allocation model.AllocationDesired, spec *oci.Spec) error {
	uid, gid, err := processOwner(spec)
	if err != nil {
		return err
	}
	if err := c.chownSecretProjection(allocation, uid, gid); err != nil {
		return err
	}
	applicationDir := filepath.Join(c.options.VolumesDir, allocation.Application)
	for _, mount := range allocation.Mounts {
		if err := claimVolumeRoot(filepath.Join(applicationDir, mount.Name), uid, gid); err != nil {
			return fmt.Errorf("prepare local volume %q ownership: %w", mount.Name, err)
		}
	}
	return nil
}

func processOwner(spec *oci.Spec) (uint32, uint32, error) {
	if spec == nil || spec.Process == nil {
		return 0, 0, fmt.Errorf("image did not produce a process user")
	}
	return spec.Process.User.UID, spec.Process.User.GID, nil
}

// chownSecretProjection gives the projected secret values to the workload user
// so that runtime-secret-init and secret file readers work for non-root images.
// Values stay 0400; the projection directory and ownership marker stay
// root-owned because only the individual value files are bind-mounted.
func (c *runtimeClient) chownSecretProjection(allocation model.AllocationDesired, uid, gid uint32) error {
	if len(allocation.SecretEnvironment) == 0 && len(allocation.SecretFiles) == 0 {
		return nil
	}
	objectDir := filepath.Join(c.options.SecretsDir, orchruntime.ObjectID(allocation.ID, allocation.SpecDigest))
	if filepath.Dir(objectDir) != c.options.SecretsDir {
		return fmt.Errorf("invalid runtime secret projection identity")
	}
	entries, err := os.ReadDir(objectDir)
	if err != nil {
		return fmt.Errorf("inspect runtime secret projection: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "value-") || !entry.Type().IsRegular() {
			continue
		}
		if err := os.Lchown(filepath.Join(objectDir, entry.Name()), int(uid), int(gid)); err != nil {
			return fmt.Errorf("assign runtime secret projection ownership: %w", err)
		}
	}
	return nil
}

// claimVolumeRoot transfers a local volume root to the workload user only when
// it is still the empty root-owned directory created by volumeMounts. Existing
// data and directories already owned by another user are never re-owned, and
// nothing below the root is changed; the 0700 mode is preserved.
func claimVolumeRoot(path string, uid, gid uint32) error {
	if uid == 0 && gid == 0 {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok {
		return fmt.Errorf("runtime path %s must be a non-symlink directory", path)
	}
	if stat.Uid != 0 || (stat.Uid == uid && stat.Gid == gid) {
		return nil
	}
	empty, err := directoryEmpty(path)
	if err != nil || !empty {
		return err
	}
	return os.Lchown(path, int(uid), int(gid))
}

func directoryEmpty(path string) (bool, error) {
	directory, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer directory.Close()
	if _, err := directory.Readdirnames(1); err != nil {
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func withSecretEnvironment(environment map[string]model.SecretReference) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, spec *oci.Spec) error {
		if spec.Process == nil || len(spec.Process.Args) == 0 {
			return fmt.Errorf("image did not produce a process for secret environment initialization")
		}
		names := make([]string, 0, len(environment))
		for name := range environment {
			names = append(names, name)
		}
		sort.Strings(names)
		arguments := []string{"/run/orch/secret-init", "runtime-secret-init"}
		for index, name := range names {
			arguments = append(arguments, fmt.Sprintf("%s=/run/orch/secrets/%d", name, index))
		}
		arguments = append(arguments, "--")
		arguments = append(arguments, spec.Process.Args...)
		spec.Process.Args = arguments
		return nil
	}
}

func withNoSwap(_ context.Context, _ oci.Client, _ *containers.Container, spec *oci.Spec) error {
	if spec.Linux == nil || spec.Linux.Resources == nil || spec.Linux.Resources.Memory == nil {
		return fmt.Errorf("memory resources are unavailable")
	}
	if spec.Linux.Resources.Memory.Limit == nil {
		return fmt.Errorf("memory limit is unavailable")
	}
	// OCI models Swap as the combined memory and swap limit. Setting it equal
	// to Limit converts to memory.swap.max=0 under cgroups v2.
	combinedLimit := *spec.Linux.Resources.Memory.Limit
	spec.Linux.Resources.Memory.Swap = &combinedLimit
	return nil
}

func hardenedSpec(_ context.Context, _ oci.Client, _ *containers.Container, spec *oci.Spec) error {
	if spec.Process == nil || spec.Linux == nil {
		return fmt.Errorf("image did not produce a Linux OCI process spec")
	}
	spec.Process.Terminal = false
	spec.Process.NoNewPrivileges = true
	spec.Linux.Namespaces = []specs.LinuxNamespace{
		{Type: specs.PIDNamespace}, {Type: specs.MountNamespace}, {Type: specs.IPCNamespace},
		{Type: specs.UTSNamespace}, {Type: specs.NetworkNamespace}, {Type: specs.CgroupNamespace},
	}
	spec.Linux.Devices = nil
	if spec.Linux.Resources == nil {
		spec.Linux.Resources = &specs.LinuxResources{}
	}
	spec.Linux.Resources.Devices = []specs.LinuxDeviceCgroup{{Allow: false, Access: "rwm"}}
	return nil
}

func (c *runtimeClient) volumeMounts(allocation model.AllocationDesired) ([]specs.Mount, error) {
	result := make([]specs.Mount, 0, len(allocation.Mounts))
	applicationDir := filepath.Join(c.options.VolumesDir, allocation.Application)
	if err := ensurePrivateDirectory(applicationDir); err != nil {
		return nil, err
	}
	for _, mount := range allocation.Mounts {
		hostPath := filepath.Join(applicationDir, mount.Name)
		if filepath.Dir(hostPath) != applicationDir {
			return nil, fmt.Errorf("invalid local volume name %q", mount.Name)
		}
		if err := ensurePrivateDirectory(hostPath); err != nil {
			return nil, err
		}
		options := []string{"rbind", "rw"}
		if mount.ReadOnly {
			options[1] = "ro"
		}
		result = append(result, specs.Mount{Destination: mount.Target, Type: "bind", Source: hostPath, Options: options})
	}
	secretMounts, err := c.secretMounts(allocation)
	if err != nil {
		return nil, err
	}
	result = append(result, secretMounts...)
	if allocation.NetworkGeneration != 0 {
		if c.options.ResolverPath == "" {
			return nil, fmt.Errorf("workload resolver path is unavailable")
		}
		c.mu.RLock()
		networkConfig := c.networkConfig
		c.mu.RUnlock()
		if networkConfig.Generation != allocation.NetworkGeneration || networkConfig.Gateway == "" || networkConfig.DNSZone == "" {
			return nil, fmt.Errorf("workload network generation is not prepared")
		}
		resolverPath := applicationResolverPath(c.options.ResolverPath, allocation.Application)
		contents := []byte("nameserver " + networkConfig.Gateway + "\nsearch " + allocation.Application + "." +
			strings.TrimSuffix(networkConfig.DNSZone, ".") + "\noptions ndots:1 timeout:1 attempts:2\n")
		if err := writeResolverFile(resolverPath, contents); err != nil {
			return nil, fmt.Errorf("write application resolver: %w", err)
		}
		info, err := os.Lstat(resolverPath)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o644 {
			return nil, fmt.Errorf("workload resolver must be a read-only-to-workloads regular file")
		}
		result = append(result, specs.Mount{
			Destination: "/etc/resolv.conf", Type: "bind", Source: resolverPath,
			Options: []string{"rbind", "ro"},
		})
	}
	return result, nil
}

func (c *runtimeClient) secretMounts(allocation model.AllocationDesired) ([]specs.Mount, error) {
	if len(allocation.SecretEnvironment) == 0 && len(allocation.SecretFiles) == 0 {
		return nil, nil
	}
	if c.options.SecretsDir == "" {
		return nil, fmt.Errorf("runtime secret tmpfs path is unavailable")
	}
	if err := ensureSecretTmpfs(c.options.SecretsDir); err != nil {
		return nil, err
	}
	objectID := orchruntime.ObjectID(allocation.ID, allocation.SpecDigest)
	objectDir := filepath.Join(c.options.SecretsDir, objectID)
	if filepath.Dir(objectDir) != c.options.SecretsDir {
		return nil, fmt.Errorf("invalid runtime secret projection identity")
	}
	if err := os.RemoveAll(objectDir); err != nil {
		return nil, fmt.Errorf("clear runtime secret projection: %w", err)
	}
	if err := os.Mkdir(objectDir, 0o700); err != nil {
		return nil, fmt.Errorf("create runtime secret projection: %w", err)
	}
	marker := []byte(c.options.ClusterID + "\x00" + c.options.NodeID + "\x00" + objectID)
	if err := os.WriteFile(filepath.Join(objectDir, ".orch-owned"), marker, 0o400); err != nil {
		return nil, fmt.Errorf("mark runtime secret projection ownership: %w", err)
	}
	paths := make(map[string]string)
	writeVersion := func(version string) (string, error) {
		if existing := paths[version]; existing != "" {
			return existing, nil
		}
		value := allocation.RuntimeSecretValues[version]
		if len(value) == 0 {
			return "", fmt.Errorf("secret material version %q is unavailable", version)
		}
		path := filepath.Join(objectDir, fmt.Sprintf("value-%d", len(paths)))
		if err := os.WriteFile(path, value, 0o400); err != nil {
			return "", fmt.Errorf("write runtime secret projection: %w", err)
		}
		paths[version] = path
		return path, nil
	}
	result := make([]specs.Mount, 0, len(allocation.SecretEnvironment)+len(allocation.SecretFiles)+1)
	if len(allocation.SecretEnvironment) != 0 {
		result = append(result, specs.Mount{Destination: "/run/orch/secret-init", Type: "bind", Source: c.options.LoggerPath, Options: []string{"rbind", "ro"}})
		names := make([]string, 0, len(allocation.SecretEnvironment))
		for name := range allocation.SecretEnvironment {
			names = append(names, name)
		}
		sort.Strings(names)
		for index, name := range names {
			hostPath, err := writeVersion(allocation.SecretEnvironment[name].VersionID)
			if err != nil {
				return nil, err
			}
			result = append(result, specs.Mount{Destination: fmt.Sprintf("/run/orch/secrets/%d", index), Type: "bind", Source: hostPath, Options: []string{"rbind", "ro"}})
		}
	}
	for _, file := range allocation.SecretFiles {
		hostPath, err := writeVersion(file.VersionID)
		if err != nil {
			return nil, err
		}
		result = append(result, specs.Mount{Destination: file.Target, Type: "bind", Source: hostPath, Options: []string{"rbind", "ro"}})
	}
	return result, nil
}

func (c *runtimeClient) reconcileSecretProjections(owned map[string]struct{}) error {
	if c.options.SecretsDir == "" {
		return nil
	}
	var status unix.Statfs_t
	if err := unix.Statfs(c.options.SecretsDir, &status); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if uint64(status.Type) != uint64(unix.TMPFS_MAGIC) {
		return nil
	}
	entries, err := os.ReadDir(c.options.SecretsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, exists := owned[entry.Name()]; exists {
			continue
		}
		objectDir := filepath.Join(c.options.SecretsDir, entry.Name())
		marker, err := os.ReadFile(filepath.Join(objectDir, ".orch-owned"))
		if err != nil || string(marker) != c.options.ClusterID+"\x00"+c.options.NodeID+"\x00"+entry.Name() {
			continue
		}
		if err := os.RemoveAll(objectDir); err != nil {
			return fmt.Errorf("remove stale runtime secret projection %q: %w", entry.Name(), err)
		}
	}
	return nil
}

func ensureSecretTmpfs(path string) error {
	if err := ensurePrivateDirectory(path); err != nil {
		return fmt.Errorf("prepare runtime secret tmpfs: %w", err)
	}
	var status unix.Statfs_t
	if err := unix.Statfs(path, &status); err != nil {
		return fmt.Errorf("inspect runtime secret tmpfs: %w", err)
	}
	if uint64(status.Type) == uint64(unix.TMPFS_MAGIC) {
		return nil
	}
	if err := unix.Mount("tmpfs", path, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0700,size=64m"); err != nil {
		return fmt.Errorf("mount runtime secret tmpfs: %w", err)
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("runtime path %s must be a private non-symlink directory", path)
	}
	return nil
}

func taskLogCreator(loggerPath, logPrefix string) cio.Creator {
	query := url.Values{}
	query.Set("runtime-log-sink", logPrefix)
	logURI := (&url.URL{Scheme: "binary-v2", Path: filepath.ToSlash(loggerPath), RawQuery: query.Encode()}).String()
	return func(string) (cio.IO, error) {
		return &directLogIO{config: cio.Config{Stdout: logURI, Stderr: logURI}}, nil
	}
}

type directLogIO struct {
	config cio.Config
}

func (io *directLogIO) Config() cio.Config { return io.config }
func (*directLogIO) Cancel()               {}
func (*directLogIO) Wait()                 {}
func (*directLogIO) Close() error          { return nil }
