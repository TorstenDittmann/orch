//go:build linux

package containerd

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"

	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"

	"change.codes/torsten/better-swarm/internal/model"
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
		oci.WithMounts(mounts),
		hardenedSpec,
	)
	return opts
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
	return result, nil
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
