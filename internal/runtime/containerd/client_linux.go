//go:build linux

package containerd

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"

	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/network"
	orchruntime "change.codes/torsten/better-swarm/internal/runtime"
)

const (
	labelManaged           = "io.orch.managed"
	labelCluster           = "io.orch.cluster"
	labelNode              = "io.orch.node"
	labelAllocation        = "io.orch.allocation"
	labelSpec              = "io.orch.spec-digest"
	labelNetworkGeneration = "io.orch.network-generation"
)

type runtimeClient struct {
	client              *containerdclient.Client
	options             orchruntime.Options
	mu                  sync.RWMutex
	networkConfig       model.NetworkConfig
	networkCapabilities network.Capabilities
	networkPreflightErr error
	attachmentStore     network.AttachmentStore
}

func New(socket string, options orchruntime.Options) (orchruntime.Runtime, error) {
	if strings.TrimSpace(socket) == "" {
		return nil, fmt.Errorf("containerd socket is required")
	}
	if err := orchruntime.ValidateOptions(options); err != nil {
		return nil, err
	}
	var err error
	if options.LoggerPath == "" {
		options.LoggerPath, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolve orch log sink executable: %w", err)
		}
	}
	options.LoggerPath, err = filepath.Abs(options.LoggerPath)
	if err != nil {
		return nil, fmt.Errorf("resolve orch log sink path: %w", err)
	}
	loggerInfo, err := os.Lstat(options.LoggerPath)
	if err != nil || !loggerInfo.Mode().IsRegular() || loggerInfo.Mode().Perm()&0o111 == 0 || loggerInfo.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("orch log sink must be an executable regular file that is not group/world-writable: %s", options.LoggerPath)
	}
	if options.SecretsDir != "" {
		options.SecretsDir, err = filepath.Abs(options.SecretsDir)
		if err != nil {
			return nil, fmt.Errorf("resolve runtime secrets path: %w", err)
		}
	}
	namespace := "orch-" + strings.ReplaceAll(options.ClusterID, "_", "-")
	client, err := containerdclient.New(socket,
		containerdclient.WithDefaultNamespace(namespace),
		containerdclient.WithDefaultPlatform(platforms.DefaultStrict()),
		containerdclient.WithTimeout(10*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to containerd: %w", err)
	}
	return &runtimeClient{client: client, options: options}, nil
}

func (c *runtimeClient) Preflight(ctx context.Context) (orchruntime.Capabilities, error) {
	version, err := c.client.Version(ctx)
	if err != nil {
		return orchruntime.Capabilities{}, fmt.Errorf("read containerd version: %w", err)
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return orchruntime.Capabilities{}, fmt.Errorf("cgroups v2 is unavailable: %w", err)
	}
	if normalized := strings.TrimPrefix(version.Version, "v"); normalized != "2.3.4" {
		return orchruntime.Capabilities{}, fmt.Errorf("containerd 2.3.4 is required, found %q: %w", version.Version, orchruntime.ErrUnsupported)
	}
	for _, executable := range []string{"containerd-shim-runc-v2", "runc"} {
		if _, err := exec.LookPath(executable); err != nil {
			return orchruntime.Capabilities{}, fmt.Errorf("required runtime executable %q is unavailable: %w", executable, err)
		}
	}
	if err := c.client.SnapshotService(c.options.Snapshotter).Walk(ctx, func(context.Context, snapshots.Info) error { return nil }, "name==__orch_preflight_missing__"); err != nil {
		return orchruntime.Capabilities{}, fmt.Errorf("snapshotter %q is unavailable: %w", c.options.Snapshotter, err)
	}
	capabilities := orchruntime.Capabilities{
		OperatingSystem: "linux", Architecture: runtime.GOARCH,
		RuntimeVersion: "containerd " + version.Version, Snapshotter: c.options.Snapshotter, CgroupsV2: true,
	}
	if c.options.Network != nil {
		networkCapabilities, networkErr := c.options.Network.Preflight(ctx)
		c.mu.Lock()
		c.networkPreflightErr = networkErr
		if networkErr == nil {
			c.networkCapabilities = networkCapabilities
			capabilities.CNIVersion = networkCapabilities.CNIVersion
			capabilities.CNIPluginsVersion = networkCapabilities.CNIPluginsVersion
			capabilities.Nftables = networkCapabilities.Nftables
		}
		c.mu.Unlock()
	}
	return capabilities, nil
}

func (c *runtimeClient) PrepareNetwork(ctx context.Context, config model.NetworkConfig) error {
	if config.Generation == 0 {
		return nil
	}
	if c.options.Network == nil {
		return fmt.Errorf("CNI manager is unavailable: %w", orchruntime.ErrUnsupported)
	}
	c.mu.RLock()
	preflightReady := c.networkCapabilities.CNIVersion != "" && c.networkPreflightErr == nil
	c.mu.RUnlock()
	if !preflightReady {
		capabilities, err := c.options.Network.Preflight(ctx)
		c.mu.Lock()
		c.networkPreflightErr = err
		if err == nil {
			c.networkCapabilities = capabilities
		}
		c.mu.Unlock()
		if err != nil {
			return fmt.Errorf("network preflight: %w", err)
		}
	}
	if err := c.options.Network.Prepare(ctx, config); err != nil {
		return err
	}
	if c.options.ResolverPath == "" {
		return fmt.Errorf("workload resolver path is unavailable")
	}
	contents := []byte("nameserver " + config.Gateway + "\nsearch " + strings.TrimSuffix(config.DNSZone, ".") + "\noptions ndots:1 timeout:1 attempts:2\n")
	if err := writeResolverFile(c.options.ResolverPath, contents); err != nil {
		return err
	}
	c.mu.Lock()
	c.networkConfig = config
	c.mu.Unlock()
	return nil
}

func (c *runtimeClient) SetAttachmentStore(store network.AttachmentStore) {
	c.mu.Lock()
	c.attachmentStore = store
	c.mu.Unlock()
}

func (c *runtimeClient) networkAttachmentStore() network.AttachmentStore {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.attachmentStore
}

func (c *runtimeClient) ListOwned(ctx context.Context) ([]orchruntime.Object, error) {
	containers, err := c.client.Containers(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]orchruntime.Object, 0)
	seen := make(map[string]struct{})
	for _, container := range containers {
		labels, err := container.Labels(ctx)
		if err != nil {
			return nil, err
		}
		if labels[labelManaged] != "true" || labels[labelCluster] != c.options.ClusterID || labels[labelNode] != c.options.NodeID {
			continue
		}
		object, err := c.object(ctx, container, labels)
		if err != nil {
			return nil, err
		}
		result = append(result, object)
		seen[object.ID] = struct{}{}
	}
	allocationLeases, err := c.client.LeasesService().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list allocation leases: %w", err)
	}
	for _, lease := range allocationLeases {
		labels := lease.Labels
		if labels[labelManaged] != "true" || labels[labelCluster] != c.options.ClusterID || labels[labelNode] != c.options.NodeID {
			continue
		}
		if !strings.HasSuffix(lease.ID, "-lease") {
			return nil, orchruntime.ErrOwnershipMismatch
		}
		objectID := strings.TrimSuffix(lease.ID, "-lease")
		if err := c.verifyObjectLabels(objectID, labels); err != nil {
			return nil, err
		}
		if _, exists := seen[objectID]; exists {
			continue
		}
		result = append(result, orchruntime.Object{
			ID: objectID, AllocationID: labels[labelAllocation], SpecDigest: labels[labelSpec],
		})
		seen[objectID] = struct{}{}
	}
	if err := c.reconcileNetworkState(ctx, result, seen); err != nil {
		return nil, err
	}
	if err := c.reconcileSecretProjections(seen); err != nil {
		return nil, fmt.Errorf("reconcile runtime secret projections: %w", err)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (c *runtimeClient) Ensure(ctx context.Context, allocation model.AllocationDesired) (orchruntime.Object, error) {
	if err := validateAllocation(allocation); err != nil {
		return orchruntime.Object{}, err
	}
	id := orchruntime.ObjectID(allocation.ID, allocation.SpecDigest)
	if existing, err := c.client.LoadContainer(ctx, id); err == nil {
		labels, err := existing.Labels(ctx)
		if err != nil {
			return orchruntime.Object{}, err
		}
		if err := c.verifyLabels(labels, allocation.ID, allocation.SpecDigest); err != nil {
			return orchruntime.Object{}, err
		}
		object, err := c.object(ctx, existing, labels)
		if err != nil {
			return orchruntime.Object{}, err
		}
		if object.Running {
			return object, nil
		}
		task, taskErr := existing.Task(ctx, nil)
		if taskErr != nil && !errdefs.IsNotFound(taskErr) {
			return orchruntime.Object{}, taskErr
		}
		netNS := ""
		if taskErr == nil {
			netNS = existingTaskNetNS(task.Pid())
		}
		if err := c.detachNetwork(ctx, id, labels, netNS); err != nil {
			return orchruntime.Object{}, err
		}
		if taskErr == nil {
			_, _ = task.Delete(ctx)
		}
		if _, err := c.secretMounts(allocation); err != nil {
			return orchruntime.Object{}, err
		}
		return c.startTask(ctx, existing, allocation)
	} else if !errdefs.IsNotFound(err) {
		return orchruntime.Object{}, err
	}

	labels := c.labels(allocation.ID, allocation.SpecDigest)
	if allocation.NetworkGeneration != 0 {
		labels[labelNetworkGeneration] = strconv.FormatUint(allocation.NetworkGeneration, 10)
	}
	lease, err := c.ensureLease(ctx, id, labels)
	if err != nil {
		return orchruntime.Object{}, err
	}
	keepLease := false
	defer func() {
		if !keepLease {
			_ = c.client.LeasesService().Delete(context.Background(), lease, leases.SynchronousDelete)
		}
	}()
	leaseContext := leases.WithLease(ctx, lease.ID)
	image, err := c.resolveImage(leaseContext, allocation)
	if err != nil {
		return orchruntime.Object{}, err
	}
	mounts, err := c.volumeMounts(allocation)
	if err != nil {
		return orchruntime.Object{}, err
	}
	container, err := c.client.NewContainer(leaseContext, id,
		containerdclient.WithImage(image),
		containerdclient.WithSnapshotter(c.options.Snapshotter),
		containerdclient.WithNewSnapshot(id+"-rootfs", image, snapshots.WithLabels(labels)),
		containerdclient.WithNewSpec(c.specOptions(image, allocation, mounts)...),
		containerdclient.WithContainerLabels(labels),
		containerdclient.WithRuntime(plugins.RuntimeRuncV2, nil),
	)
	if err != nil {
		_ = c.removeSecretProjection(id)
		if errdefs.IsAlreadyExists(err) {
			return orchruntime.Object{}, fmt.Errorf("runtime id %q already exists: %w", id, orchruntime.ErrOwnershipMismatch)
		}
		return orchruntime.Object{}, fmt.Errorf("create container %q: %w", id, err)
	}
	object, err := c.startTask(leaseContext, container, allocation)
	if err != nil {
		_ = container.Delete(context.Background(), containerdclient.WithSnapshotCleanup)
		_ = c.removeSecretProjection(id)
		return orchruntime.Object{}, err
	}
	keepLease = true
	return object, nil
}

func (c *runtimeClient) resolveImage(ctx context.Context, allocation model.AllocationDesired) (containerdclient.Image, error) {
	reference := allocation.Image
	image, err := c.client.GetImage(ctx, reference)
	if err == nil {
		if !imageMatchesDigest(image, reference) {
			return nil, fmt.Errorf("local image %q target does not match its digest", reference)
		}
		if err := image.Unpack(ctx, c.options.Snapshotter); err != nil && !errdefs.IsAlreadyExists(err) {
			return nil, fmt.Errorf("unpack local digest image %q: %w", reference, err)
		}
		return image, nil
	}
	if !errdefs.IsNotFound(err) {
		return nil, fmt.Errorf("inspect local digest image %q: %w", reference, err)
	}
	pullOptions := []containerdclient.RemoteOpt{
		containerdclient.WithPullUnpack,
		containerdclient.WithPullSnapshotter(c.options.Snapshotter),
		containerdclient.WithPlatform(platforms.DefaultString()),
	}
	if credential := allocation.RegistryCredential; credential != nil {
		secret := append([]byte(nil), allocation.RuntimeRegistrySecrets[credential.VersionID]...)
		if len(secret) == 0 {
			return nil, fmt.Errorf("registry credential material for %q is unavailable", credential.Host)
		}
		defer wipeSecretBytes(secret)
		authorizer := docker.NewDockerAuthorizer(docker.WithAuthCreds(func(host string) (string, string, error) {
			if host != credential.Host && !(credential.Host == "docker.io" && host == "registry-1.docker.io") {
				return "", "", nil
			}
			return credential.Username, string(secret), nil
		}))
		resolver := docker.NewResolver(docker.ResolverOptions{Hosts: docker.ConfigureDefaultRegistries(docker.WithAuthorizer(authorizer))})
		pullOptions = append(pullOptions, containerdclient.WithResolver(resolver))
	}
	image, err = c.client.Pull(ctx, reference, pullOptions...)
	if err != nil {
		return nil, fmt.Errorf("pull digest image %q: %w", reference, err)
	}
	if !imageMatchesDigest(image, reference) {
		return nil, fmt.Errorf("pulled image %q target does not match its digest", reference)
	}
	return image, nil
}

func wipeSecretBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func imageMatchesDigest(image containerdclient.Image, reference string) bool {
	separator := strings.LastIndexByte(reference, '@')
	return separator >= 0 && separator+1 < len(reference) && image.Target().Digest.String() == reference[separator+1:]
}

func (c *runtimeClient) startTask(ctx context.Context, container containerdclient.Container, allocation model.AllocationDesired) (orchruntime.Object, error) {
	logPrefix, err := ensureLogFiles(c.options.LogsDir, container.ID())
	if err != nil {
		return orchruntime.Object{}, err
	}
	task, err := container.NewTask(ctx, taskLogCreator(c.options.LoggerPath, logPrefix))
	if err != nil {
		return orchruntime.Object{}, fmt.Errorf("create task %q: %w", container.ID(), err)
	}
	var attachment network.Attachment
	if allocation.NetworkGeneration != 0 {
		if c.options.Network == nil {
			_, _ = task.Delete(context.Background(), containerdclient.WithProcessKill)
			return orchruntime.Object{}, fmt.Errorf("CNI manager is unavailable: %w", orchruntime.ErrUnsupported)
		}
		attachment, err = c.options.Network.Attach(ctx, network.AttachmentRequest{
			AllocationID: allocation.ID, ObjectID: container.ID(), SpecDigest: allocation.SpecDigest,
			NetworkGeneration: allocation.NetworkGeneration, NetNS: fmt.Sprintf("/proc/%d/ns/net", task.Pid()), IfName: "eth0",
		})
		if err != nil {
			_, _ = task.Delete(context.Background(), containerdclient.WithProcessKill)
			return orchruntime.Object{}, fmt.Errorf("attach task %q to workload network: %w", container.ID(), err)
		}
		attachment.CreatedAt = time.Now().UTC()
		attachmentStore := c.networkAttachmentStore()
		if attachmentStore == nil {
			cleanupErr := c.detachAttachment(context.Background(), network.AttachmentRequest{
				AllocationID: allocation.ID, ObjectID: container.ID(), SpecDigest: allocation.SpecDigest,
				NetworkGeneration: allocation.NetworkGeneration, NetNS: attachment.NetNS, IfName: "eth0",
			})
			_, _ = task.Delete(context.Background(), containerdclient.WithProcessKill)
			return orchruntime.Object{}, errors.Join(fmt.Errorf("durable network attachment store is unavailable"), cleanupErr)
		}
		if err := attachmentStore.SaveAttachment(ctx, attachment); err != nil {
			cleanupErr := c.detachAttachment(context.Background(), network.AttachmentRequest{
				AllocationID: allocation.ID, ObjectID: container.ID(), SpecDigest: allocation.SpecDigest,
				NetworkGeneration: allocation.NetworkGeneration, NetNS: attachment.NetNS, IfName: "eth0",
			})
			_, _ = task.Delete(context.Background(), containerdclient.WithProcessKill)
			return orchruntime.Object{}, errors.Join(fmt.Errorf("persist task %q network attachment: %w", container.ID(), err), cleanupErr)
		}
	}
	if err := task.Start(ctx); err != nil {
		var cleanupErr error
		if allocation.NetworkGeneration != 0 {
			cleanupErr = c.detachAttachment(context.Background(), network.AttachmentRequest{
				AllocationID: allocation.ID, ObjectID: container.ID(), SpecDigest: allocation.SpecDigest,
				NetworkGeneration: allocation.NetworkGeneration, NetNS: attachment.NetNS, IfName: "eth0",
			})
		}
		_, _ = task.Delete(context.Background(), containerdclient.WithProcessKill)
		return orchruntime.Object{}, errors.Join(fmt.Errorf("start task %q: %w", container.ID(), err), cleanupErr)
	}
	address := ""
	if attachment.Address.IsValid() {
		address = attachment.Address.String()
	}
	return orchruntime.Object{
		ID: container.ID(), AllocationID: allocation.ID, SpecDigest: allocation.SpecDigest,
		Running: true, StartedAt: time.Now().UTC(), Address: address, NetworkGeneration: allocation.NetworkGeneration,
	}, nil
}

func (c *runtimeClient) Inspect(ctx context.Context, id string) (orchruntime.Object, error) {
	container, err := c.client.LoadContainer(ctx, id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return orchruntime.Object{}, orchruntime.ErrNotFound
		}
		return orchruntime.Object{}, err
	}
	labels, err := container.Labels(ctx)
	if err != nil {
		return orchruntime.Object{}, err
	}
	if err := c.verifyLabels(labels, labels[labelAllocation], labels[labelSpec]); err != nil {
		return orchruntime.Object{}, err
	}
	return c.object(ctx, container, labels)
}

func (c *runtimeClient) Wait(ctx context.Context, id string) (<-chan orchruntime.Object, error) {
	container, err := c.ownedContainer(ctx, id)
	if err != nil {
		return nil, err
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		return nil, mapNotFound(err)
	}
	wait, err := task.Wait(ctx)
	if err != nil {
		return nil, err
	}
	labels, _ := container.Labels(ctx)
	output := make(chan orchruntime.Object, 1)
	go func() {
		defer close(output)
		select {
		case <-ctx.Done():
		case exit := <-wait:
			code := int32(exit.ExitCode())
			output <- orchruntime.Object{ID: id, AllocationID: labels[labelAllocation], SpecDigest: labels[labelSpec], ExitCode: &code}
		}
	}()
	return output, nil
}

func (c *runtimeClient) Stop(ctx context.Context, id string, grace time.Duration) error {
	container, err := c.ownedContainer(ctx, id)
	if err != nil {
		if errors.Is(err, orchruntime.ErrNotFound) {
			return nil
		}
		return err
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	status, err := task.Status(ctx)
	if err != nil {
		return err
	}
	if status.Status == containerdclient.Stopped {
		return nil
	}
	wait, err := task.Wait(ctx)
	if err != nil {
		return err
	}
	if err := task.Kill(ctx, syscall.SIGTERM, containerdclient.WithKillAll); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wait:
		return nil
	case <-timer.C:
		if err := task.Kill(ctx, syscall.SIGKILL, containerdclient.WithKillAll); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
		killTimer := time.NewTimer(grace)
		defer killTimer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wait:
			return nil
		case <-killTimer.C:
			return fmt.Errorf("task %q did not exit after SIGKILL", id)
		}
	}
}

func (c *runtimeClient) Remove(ctx context.Context, id string) error {
	container, err := c.ownedContainer(ctx, id)
	if err != nil {
		if errors.Is(err, orchruntime.ErrNotFound) {
			if err := c.detachStoredAttachment(ctx, id); err != nil {
				return err
			}
			if err := c.deleteOwnedLease(ctx, id); err != nil {
				return err
			}
			return errors.Join(removeObjectLogs(c.options.LogsDir, id), c.removeSecretProjection(id))
		}
		return err
	}
	labels, err := container.Labels(ctx)
	if err != nil {
		return err
	}
	task, taskErr := container.Task(ctx, nil)
	netNS := ""
	if taskErr == nil {
		status, statusErr := task.Status(ctx)
		if statusErr != nil {
			return statusErr
		}
		if status.Status != containerdclient.Stopped {
			return fmt.Errorf("task %q is still running", id)
		}
		netNS = existingTaskNetNS(task.Pid())
	} else if !errdefs.IsNotFound(taskErr) {
		return taskErr
	}
	if err := c.detachNetwork(ctx, id, labels, netNS); err != nil {
		return err
	}
	if taskErr == nil {
		if _, err := task.Delete(ctx); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	}
	if err := container.Delete(ctx, containerdclient.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	if err := c.deleteOwnedLease(ctx, id); err != nil {
		return err
	}
	return errors.Join(removeObjectLogs(c.options.LogsDir, id), c.removeSecretProjection(id))
}

func (c *runtimeClient) removeSecretProjection(objectID string) error {
	if c.options.SecretsDir == "" || objectID == "" {
		return nil
	}
	path := filepath.Join(c.options.SecretsDir, objectID)
	if filepath.Dir(path) != c.options.SecretsDir {
		return fmt.Errorf("invalid runtime secret projection identity")
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove runtime secret projection: %w", err)
	}
	return nil
}

func (c *runtimeClient) TailLogs(ctx context.Context, id string, lines uint32) ([]orchruntime.LogFrame, error) {
	if _, err := c.Inspect(ctx, id); err != nil {
		return nil, err
	}
	return tailObjectLogs(c.options.LogsDir, id, lines)
}

func (c *runtimeClient) Exec(ctx context.Context, id string, command []string) (<-chan orchruntime.ExecFrame, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("exec command is required")
	}
	container, err := c.ownedContainer(ctx, id)
	if err != nil {
		return nil, err
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		return nil, mapNotFound(err)
	}
	status, err := task.Status(ctx)
	if err != nil || status.Status != containerdclient.Running {
		return nil, orchruntime.ErrNotFound
	}
	spec, err := container.Spec(ctx)
	if err != nil || spec.Process == nil {
		return nil, fmt.Errorf("load container process spec: %w", err)
	}
	processSpec := *spec.Process
	processSpec.Args = append([]string(nil), command...)
	processSpec.Terminal = false
	output := make(chan orchruntime.ExecFrame, 16)
	stdout := &execStreamWriter{ctx: ctx, output: output, stream: "stdout"}
	stderr := &execStreamWriter{ctx: ctx, output: output, stream: "stderr"}
	execID, err := randomExecID()
	if err != nil {
		return nil, err
	}
	process, err := task.Exec(ctx, execID, &processSpec, cio.NewCreator(cio.WithStreams(nil, stdout, stderr)))
	if err != nil {
		return nil, err
	}
	wait, err := process.Wait(ctx)
	if err != nil {
		_, _ = process.Delete(context.Background(), containerdclient.WithProcessKill)
		return nil, err
	}
	if err := process.Start(ctx); err != nil {
		_, _ = process.Delete(context.Background(), containerdclient.WithProcessKill)
		return nil, err
	}
	go func() {
		defer close(output)
		var exitCode int32 = 124
		completed := false
		select {
		case <-ctx.Done():
			_ = process.Kill(context.Background(), syscall.SIGKILL, containerdclient.WithKillAll)
		case exit := <-wait:
			exitCode = int32(exit.ExitCode())
			completed = true
		}
		if !completed {
			select {
			case exit := <-wait:
				exitCode = int32(exit.ExitCode())
			case <-time.After(5 * time.Second):
			}
			process.IO().Cancel()
		}
		process.IO().Wait()
		_, _ = process.Delete(context.Background())
		_ = process.IO().Close()
		select {
		case output <- orchruntime.ExecFrame{Exited: true, ExitCode: exitCode}:
		case <-ctx.Done():
		}
	}()
	return output, nil
}

type execStreamWriter struct {
	ctx    context.Context
	output chan<- orchruntime.ExecFrame
	stream string
}

func (w *execStreamWriter) Write(contents []byte) (int, error) {
	written := 0
	for len(contents) != 0 {
		size := min(len(contents), 64<<10)
		chunk := append([]byte(nil), contents[:size]...)
		select {
		case <-w.ctx.Done():
			return written, w.ctx.Err()
		case w.output <- orchruntime.ExecFrame{Stream: w.stream, Data: chunk}:
			written += size
			contents = contents[size:]
		}
	}
	return written, nil
}

func (c *runtimeClient) Close() error {
	var networkErr error
	if c.options.Network != nil {
		networkErr = c.options.Network.Close()
	}
	return errors.Join(c.client.Close(), networkErr)
}

func (c *runtimeClient) object(ctx context.Context, container containerdclient.Container, labels map[string]string) (orchruntime.Object, error) {
	if err := c.verifyObjectLabels(container.ID(), labels); err != nil {
		return orchruntime.Object{}, err
	}
	info, err := container.Info(ctx)
	if err != nil {
		return orchruntime.Object{}, err
	}
	object := orchruntime.Object{
		ID: container.ID(), AllocationID: labels[labelAllocation], SpecDigest: labels[labelSpec], StartedAt: info.CreatedAt,
	}
	task, taskErr := container.Task(ctx, nil)
	if taskErr != nil && !errdefs.IsNotFound(taskErr) {
		return orchruntime.Object{}, taskErr
	}
	netNS := ""
	if taskErr == nil {
		status, err := task.Status(ctx)
		if err != nil {
			return orchruntime.Object{}, err
		}
		object.Running = status.Status == containerdclient.Running
		if status.Status == containerdclient.Stopped {
			code := int32(status.ExitStatus)
			object.ExitCode = &code
		}
		if task.Pid() > 0 {
			netNS = fmt.Sprintf("/proc/%d/ns/net", task.Pid())
		}
	}
	if value := labels[labelNetworkGeneration]; value != "" {
		if c.options.Network == nil {
			return orchruntime.Object{}, fmt.Errorf("owned networked task has no CNI manager: %w", orchruntime.ErrOwnershipMismatch)
		}
		generation, parseErr := strconv.ParseUint(value, 10, 64)
		if parseErr != nil {
			return orchruntime.Object{}, orchruntime.ErrOwnershipMismatch
		}
		object.NetworkGeneration = generation
		store := c.networkAttachmentStore()
		if store == nil {
			return orchruntime.Object{}, fmt.Errorf("durable network attachment store is unavailable")
		}
		attachment, inspectErr := c.options.Network.Inspect(ctx, network.AttachmentRequest{
			AllocationID: labels[labelAllocation], ObjectID: container.ID(), SpecDigest: labels[labelSpec],
			NetworkGeneration: generation, NetNS: netNS, IfName: "eth0",
		})
		if inspectErr != nil && !errors.Is(inspectErr, network.ErrNotFound) {
			return orchruntime.Object{}, inspectErr
		}
		stored, storedErr := store.Attachment(ctx, container.ID())
		if storedErr != nil && !errors.Is(storedErr, network.ErrNotFound) {
			return orchruntime.Object{}, storedErr
		}
		if inspectErr == nil && storedErr == nil {
			if stored.AllocationID != attachment.AllocationID || stored.ObjectID != attachment.ObjectID ||
				stored.SpecDigest != attachment.SpecDigest || stored.NetworkGeneration != attachment.NetworkGeneration ||
				stored.Address != attachment.Address {
				return orchruntime.Object{}, orchruntime.ErrOwnershipMismatch
			}
			object.Address = attachment.Address.String()
		} else if inspectErr == nil {
			attachment.CreatedAt = info.CreatedAt.UTC()
			if err := store.SaveAttachment(ctx, attachment); err != nil {
				return orchruntime.Object{}, fmt.Errorf("recover durable network attachment %q: %w", container.ID(), err)
			}
			object.Address = attachment.Address.String()
		} else if storedErr == nil {
			if object.Running {
				return orchruntime.Object{}, fmt.Errorf("running task %q lost its CNI cache: %w", container.ID(), orchruntime.ErrOwnershipMismatch)
			}
			object.Address = stored.Address.String()
		}
		if object.Running {
			if netNS == "" {
				return orchruntime.Object{}, fmt.Errorf("running task %q has no inspectable network namespace: %w", container.ID(), orchruntime.ErrOwnershipMismatch)
			}
			if err := c.options.Network.Check(ctx, network.AttachmentRequest{
				AllocationID: labels[labelAllocation], ObjectID: container.ID(), SpecDigest: labels[labelSpec],
				NetworkGeneration: generation, NetNS: netNS, IfName: "eth0",
			}); err != nil {
				return orchruntime.Object{}, fmt.Errorf("check running task %q network attachment: %w", container.ID(), err)
			}
		}
	}
	return object, nil
}

func (c *runtimeClient) reconcileNetworkState(ctx context.Context, objects []orchruntime.Object, seen map[string]struct{}) error {
	store := c.networkAttachmentStore()
	if store == nil {
		return nil
	}
	attachments, err := store.Attachments(ctx)
	if err != nil {
		return fmt.Errorf("list durable network attachments: %w", err)
	}
	for _, attachment := range attachments {
		if _, exists := seen[attachment.ObjectID]; exists {
			continue
		}
		if orchruntime.ObjectID(attachment.AllocationID, attachment.SpecDigest) != attachment.ObjectID {
			return orchruntime.ErrOwnershipMismatch
		}
		if err := c.detachAttachment(ctx, network.AttachmentRequest{
			AllocationID: attachment.AllocationID, ObjectID: attachment.ObjectID,
			SpecDigest: attachment.SpecDigest, NetworkGeneration: attachment.NetworkGeneration,
			IfName: "eth0",
		}); err != nil {
			return fmt.Errorf("clean orphan network attachment %q: %w", attachment.ObjectID, err)
		}
	}
	c.mu.RLock()
	config := c.networkConfig
	c.mu.RUnlock()
	if config.Generation == 0 || c.options.Network == nil {
		return nil
	}
	valid := make([]network.AttachmentRequest, 0, len(objects))
	for _, object := range objects {
		if object.NetworkGeneration != config.Generation {
			continue
		}
		valid = append(valid, network.AttachmentRequest{
			AllocationID: object.AllocationID, ObjectID: object.ID, SpecDigest: object.SpecDigest,
			NetworkGeneration: object.NetworkGeneration, IfName: "eth0",
		})
	}
	if err := c.options.Network.GC(ctx, valid); err != nil {
		return fmt.Errorf("garbage-collect owned CNI state: %w", err)
	}
	return nil
}

func (c *runtimeClient) ownedContainer(ctx context.Context, id string) (containerdclient.Container, error) {
	container, err := c.client.LoadContainer(ctx, id)
	if err != nil {
		return nil, mapNotFound(err)
	}
	labels, err := container.Labels(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.verifyObjectLabels(id, labels); err != nil {
		return nil, err
	}
	return container, nil
}

func (c *runtimeClient) labels(allocationID, specDigest string) map[string]string {
	return map[string]string{
		labelManaged: "true", labelCluster: c.options.ClusterID, labelNode: c.options.NodeID,
		labelAllocation: allocationID, labelSpec: specDigest,
	}
}

func (c *runtimeClient) verifyLabels(labels map[string]string, allocationID, specDigest string) error {
	if labels[labelManaged] != "true" || labels[labelCluster] != c.options.ClusterID || labels[labelNode] != c.options.NodeID ||
		allocationID == "" || specDigest == "" || labels[labelAllocation] != allocationID || labels[labelSpec] != specDigest {
		return orchruntime.ErrOwnershipMismatch
	}
	return nil
}

func (c *runtimeClient) verifyObjectLabels(id string, labels map[string]string) error {
	allocationID := labels[labelAllocation]
	specDigest := labels[labelSpec]
	if err := c.verifyLabels(labels, allocationID, specDigest); err != nil || orchruntime.ObjectID(allocationID, specDigest) != id {
		return orchruntime.ErrOwnershipMismatch
	}
	return nil
}

func (c *runtimeClient) detachNetwork(ctx context.Context, id string, labels map[string]string, netNS string) error {
	if labels[labelNetworkGeneration] == "" {
		return nil
	}
	if c.options.Network == nil {
		return fmt.Errorf("CNI manager is unavailable: %w", orchruntime.ErrUnsupported)
	}
	generation, err := strconv.ParseUint(labels[labelNetworkGeneration], 10, 64)
	if err != nil {
		return orchruntime.ErrOwnershipMismatch
	}
	return c.detachAttachment(ctx, network.AttachmentRequest{
		AllocationID: labels[labelAllocation], ObjectID: id, SpecDigest: labels[labelSpec],
		NetworkGeneration: generation, NetNS: netNS, IfName: "eth0",
	})
}

func existingTaskNetNS(pid uint32) string {
	if pid == 0 {
		return ""
	}
	path := fmt.Sprintf("/proc/%d/ns/net", pid)
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

func (c *runtimeClient) detachStoredAttachment(ctx context.Context, objectID string) error {
	store := c.networkAttachmentStore()
	if store == nil {
		return nil
	}
	attachment, err := store.Attachment(ctx, objectID)
	if errors.Is(err, network.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if orchruntime.ObjectID(attachment.AllocationID, attachment.SpecDigest) != attachment.ObjectID {
		return orchruntime.ErrOwnershipMismatch
	}
	return c.detachAttachment(ctx, network.AttachmentRequest{
		AllocationID: attachment.AllocationID, ObjectID: attachment.ObjectID,
		SpecDigest: attachment.SpecDigest, NetworkGeneration: attachment.NetworkGeneration,
		IfName: "eth0",
	})
}

func (c *runtimeClient) detachAttachment(ctx context.Context, request network.AttachmentRequest) error {
	if c.options.Network == nil {
		return fmt.Errorf("CNI manager is unavailable: %w", orchruntime.ErrUnsupported)
	}
	if err := c.options.Network.Detach(ctx, request); err != nil && !errors.Is(err, network.ErrNotFound) {
		return err
	}
	if store := c.networkAttachmentStore(); store != nil {
		if err := store.DeleteAttachment(ctx, request.ObjectID); err != nil {
			return fmt.Errorf("delete durable network attachment %q: %w", request.ObjectID, err)
		}
	}
	return nil
}

func (c *runtimeClient) ensureLease(ctx context.Context, objectID string, labels map[string]string) (leases.Lease, error) {
	manager := c.client.LeasesService()
	lease, err := manager.Create(ctx, leases.WithID(leaseID(objectID)), leases.WithLabels(labels))
	if err == nil {
		return lease, nil
	}
	if !errdefs.IsAlreadyExists(err) {
		return leases.Lease{}, fmt.Errorf("create allocation lease %q: %w", leaseID(objectID), err)
	}
	existing, listErr := manager.List(ctx, "id=="+leaseID(objectID))
	if listErr != nil {
		return leases.Lease{}, fmt.Errorf("inspect allocation lease %q: %w", leaseID(objectID), listErr)
	}
	for _, candidate := range existing {
		if candidate.ID == leaseID(objectID) {
			if err := c.verifyLabels(candidate.Labels, labels[labelAllocation], labels[labelSpec]); err != nil {
				return leases.Lease{}, fmt.Errorf("allocation lease %q labels do not match: %w", candidate.ID, err)
			}
			return candidate, nil
		}
	}
	return leases.Lease{}, fmt.Errorf("allocation lease %q already exists but cannot be inspected: %w", leaseID(objectID), orchruntime.ErrOwnershipMismatch)
}

func (c *runtimeClient) deleteOwnedLease(ctx context.Context, objectID string) error {
	id := leaseID(objectID)
	manager := c.client.LeasesService()
	existing, err := manager.List(ctx, "id=="+id)
	if err != nil {
		return fmt.Errorf("inspect allocation lease %q: %w", id, err)
	}
	for _, candidate := range existing {
		if candidate.ID != id {
			continue
		}
		if err := c.verifyObjectLabels(objectID, candidate.Labels); err != nil {
			return fmt.Errorf("refuse allocation lease %q removal: %w", id, err)
		}
		if err := manager.Delete(ctx, candidate, leases.SynchronousDelete); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("remove allocation lease %q: %w", id, err)
		}
		return nil
	}
	return nil
}

func leaseID(objectID string) string {
	return objectID + "-lease"
}

func mapNotFound(err error) error {
	if errdefs.IsNotFound(err) {
		return orchruntime.ErrNotFound
	}
	return err
}

func randomExecID() (string, error) {
	value := make([]byte, 12)
	if _, err := cryptorand.Read(value); err != nil {
		return "", err
	}
	return "exec_" + hex.EncodeToString(value), nil
}
