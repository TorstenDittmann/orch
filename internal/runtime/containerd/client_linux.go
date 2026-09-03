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
	"strings"
	"syscall"
	"time"

	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"

	"change.codes/torsten/better-swarm/internal/model"
	orchruntime "change.codes/torsten/better-swarm/internal/runtime"
)

const (
	labelManaged    = "io.orch.managed"
	labelCluster    = "io.orch.cluster"
	labelNode       = "io.orch.node"
	labelAllocation = "io.orch.allocation"
	labelSpec       = "io.orch.spec-digest"
)

type runtimeClient struct {
	client  *containerdclient.Client
	options orchruntime.Options
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
	return orchruntime.Capabilities{
		OperatingSystem: "linux", Architecture: runtime.GOARCH,
		RuntimeVersion: "containerd " + version.Version, Snapshotter: c.options.Snapshotter, CgroupsV2: true,
	}, nil
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
		if task, err := existing.Task(ctx, nil); err == nil {
			_, _ = task.Delete(ctx)
		} else if !errdefs.IsNotFound(err) {
			return orchruntime.Object{}, err
		}
		return c.startTask(ctx, existing, allocation)
	} else if !errdefs.IsNotFound(err) {
		return orchruntime.Object{}, err
	}

	labels := c.labels(allocation.ID, allocation.SpecDigest)
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
	image, err := c.resolveImage(leaseContext, allocation.Image)
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
		if errdefs.IsAlreadyExists(err) {
			return orchruntime.Object{}, fmt.Errorf("runtime id %q already exists: %w", id, orchruntime.ErrOwnershipMismatch)
		}
		return orchruntime.Object{}, fmt.Errorf("create container %q: %w", id, err)
	}
	object, err := c.startTask(leaseContext, container, allocation)
	if err != nil {
		_ = container.Delete(context.Background(), containerdclient.WithSnapshotCleanup)
		return orchruntime.Object{}, err
	}
	keepLease = true
	return object, nil
}

func (c *runtimeClient) resolveImage(ctx context.Context, reference string) (containerdclient.Image, error) {
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
	image, err = c.client.Pull(ctx, reference,
		containerdclient.WithPullUnpack,
		containerdclient.WithPullSnapshotter(c.options.Snapshotter),
		containerdclient.WithPlatform(platforms.DefaultString()),
	)
	if err != nil {
		return nil, fmt.Errorf("pull public digest image %q: %w", reference, err)
	}
	if !imageMatchesDigest(image, reference) {
		return nil, fmt.Errorf("pulled image %q target does not match its digest", reference)
	}
	return image, nil
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
	if err := task.Start(ctx); err != nil {
		_, _ = task.Delete(context.Background(), containerdclient.WithProcessKill)
		return orchruntime.Object{}, fmt.Errorf("start task %q: %w", container.ID(), err)
	}
	return orchruntime.Object{
		ID: container.ID(), AllocationID: allocation.ID, SpecDigest: allocation.SpecDigest,
		Running: true, StartedAt: time.Now().UTC(),
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
			if err := c.deleteOwnedLease(ctx, id); err != nil {
				return err
			}
			return removeObjectLogs(c.options.LogsDir, id)
		}
		return err
	}
	if task, err := container.Task(ctx, nil); err == nil {
		status, statusErr := task.Status(ctx)
		if statusErr != nil {
			return statusErr
		}
		if status.Status != containerdclient.Stopped {
			return fmt.Errorf("task %q is still running", id)
		}
		if _, err := task.Delete(ctx); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	} else if !errdefs.IsNotFound(err) {
		return err
	}
	if err := container.Delete(ctx, containerdclient.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	if err := c.deleteOwnedLease(ctx, id); err != nil {
		return err
	}
	return removeObjectLogs(c.options.LogsDir, id)
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

func (c *runtimeClient) Close() error { return c.client.Close() }

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
	task, err := container.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return object, nil
		}
		return orchruntime.Object{}, err
	}
	status, err := task.Status(ctx)
	if err != nil {
		return orchruntime.Object{}, err
	}
	object.Running = status.Status == containerdclient.Running
	if status.Status == containerdclient.Stopped {
		code := int32(status.ExitStatus)
		object.ExitCode = &code
	}
	return object, nil
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
