package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"change.codes/torsten/better-swarm/internal/config"
	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/store"
	"github.com/distribution/reference"
)

// Reconciler turns durable application intent into one immutable full-node
// snapshot. It deliberately contains no placement algorithm: this release has
// at most one node.
type Reconciler struct {
	state       *store.Store
	cluster     model.Cluster
	now         func() time.Time
	onPublished func(model.Assignment)
}

func NewReconciler(state *store.Store, cluster model.Cluster, now func() time.Time, onPublished func(model.Assignment)) *Reconciler {
	if now == nil {
		now = time.Now
	}
	return &Reconciler{state: state, cluster: cluster, now: now, onPublished: onPublished}
}

// Reconcile is safe to call after every desired-state, enrollment, capability,
// or connection change. Identical allocation sets do not advance generation.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	applications, err := r.state.Applications(ctx, true)
	if err != nil {
		return fmt.Errorf("list desired applications: %w", err)
	}
	nodes, err := r.state.Nodes(ctx)
	if err != nil {
		return fmt.Errorf("list execution nodes: %w", err)
	}
	now := r.now().UTC()
	if len(nodes) == 0 {
		states := make(map[string]model.ApplicationExecution)
		for _, application := range applications {
			if application.DeletedAt != nil {
				continue
			}
			states[application.Name] = executionState(model.ExecutionPending, 0,
				"agent_unavailable", "no agent is enrolled or connected", now)
		}
		return r.state.UpdateApplicationExecutions(ctx, states)
	}

	node := nodes[0]
	allocations := make([]model.AllocationDesired, 0)
	states := make(map[string]model.ApplicationExecution, len(applications))
	for _, application := range applications {
		if application.DeletedAt != nil {
			if application.Execution.Status != model.ExecutionRemoved {
				states[application.Name] = executionState(model.ExecutionStopping, 0,
					"removal_pending", "waiting for the agent to remove runtime objects", now)
			}
			continue
		}
		planned, blocked, err := planApplication(r.cluster.ID, node, application)
		if err != nil {
			return fmt.Errorf("plan application %q: %w", application.Name, err)
		}
		if blocked != nil {
			status := blocked.status
			if status == "" {
				status = model.ExecutionBlocked
			}
			states[application.Name] = executionState(status, application.Generation,
				blocked.reason, blocked.message, now)
			continue
		}
		allocations = append(allocations, planned...)
		if application.Execution.ObservedGeneration != application.Generation ||
			application.Execution.Status == model.ExecutionBlocked || application.Execution.Status == model.ExecutionRemoved {
			states[application.Name] = executionState(model.ExecutionPending, 0,
				"assignment_pending", "desired state is waiting for durable agent receipt", now)
		}
	}
	sort.Slice(allocations, func(i, j int) bool { return allocations[i].ID < allocations[j].ID })
	payload, err := json.Marshal(allocations)
	if err != nil {
		return fmt.Errorf("encode assignment snapshot: %w", err)
	}
	digest := sha256.Sum256(payload)
	assignment, changed, err := r.state.PublishAssignment(ctx, model.PublishAssignment{
		NodeID: node.ID, Digest: "sha256:" + hex.EncodeToString(digest[:]), Payload: payload,
		Allocations: allocations, ApplicationStates: states, CreatedAt: now,
	})
	if err != nil {
		return err
	}
	if changed && r.onPublished != nil {
		r.onPublished(assignment)
	}
	return nil
}

type blockReason struct {
	status  string
	reason  string
	message string
}

func planApplication(clusterID string, node model.Node, application model.Application) ([]model.AllocationDesired, *blockReason, error) {
	var manifest config.Manifest
	if err := json.Unmarshal(application.CanonicalManifest, &manifest); err != nil {
		return nil, nil, fmt.Errorf("decode stored manifest: %w", err)
	}
	if err := config.Validate(&manifest); err != nil {
		return nil, nil, fmt.Errorf("validate stored manifest: %w", err)
	}
	if blocked := nodeBlocker(node); blocked != nil {
		return nil, blocked, nil
	}

	serviceNames := make([]string, 0, len(manifest.Services))
	for name := range manifest.Services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)
	for _, name := range serviceNames {
		service := manifest.Services[name]
		base := "services." + name
		parsed, err := reference.ParseNormalizedNamed(service.Image)
		if err != nil {
			return nil, nil, fmt.Errorf("parse stored image reference: %w", err)
		}
		if _, ok := parsed.(reference.Digested); !ok {
			return nil, &blockReason{
				reason:  "unsupported_image_tag",
				message: base + ".image must use a public digest-qualified OCI reference",
			}, nil
		}
		domain := reference.Domain(parsed)
		if domain == "localhost" || strings.HasPrefix(domain, "localhost:") {
			return nil, &blockReason{
				reason:  "private_registry_unsupported",
				message: base + ".image requires a private/local registry; registry credentials are not available in this release",
			}, nil
		}
		if len(service.SecretEnv) != 0 || len(service.SecretFiles) != 0 {
			return nil, &blockReason{
				reason:  "secrets_unsupported",
				message: base + " contains secret references; secrets are not executable in this release",
			}, nil
		}
		if len(service.Constraints) != 0 {
			return nil, &blockReason{
				reason:  "constraints_unsupported",
				message: base + ".constraints requires a later scheduling release",
			}, nil
		}
		if service.HTTP != nil {
			return nil, &blockReason{
				reason:  "http_unsupported",
				message: base + ".http requires networking and health gates that are not available in this release",
			}, nil
		}
	}

	allocations := make([]model.AllocationDesired, 0)
	for _, serviceName := range serviceNames {
		service := manifest.Services[serviceName]
		cpuMillis, err := parseCPUMillis(service.Resources.CPU)
		if err != nil {
			return nil, nil, err
		}
		memoryBytes, err := parseMemoryBytes(service.Resources.Memory)
		if err != nil {
			return nil, nil, err
		}
		startDeadline, err := time.ParseDuration(manifest.Deploy.HealthDeadline)
		if err != nil {
			return nil, nil, fmt.Errorf("parse stored process-start deadline: %w", err)
		}
		mounts := make([]model.VolumeMount, 0, len(service.Mounts))
		for _, mount := range service.Mounts {
			mounts = append(mounts, model.VolumeMount{Name: mount.Source, Target: mount.Target, ReadOnly: mount.ReadOnly})
		}
		sort.Slice(mounts, func(i, j int) bool {
			if mounts[i].Target == mounts[j].Target {
				return mounts[i].Name < mounts[j].Name
			}
			return mounts[i].Target < mounts[j].Target
		})
		for replica := 1; replica <= service.Replicas; replica++ {
			allocation := model.AllocationDesired{
				ID:          allocationID(clusterID, application.Name, serviceName, uint32(replica)),
				Application: application.Name, Service: serviceName, Replica: uint32(replica),
				ApplicationGeneration: application.Generation, RevisionID: application.RevisionID,
				Image: service.Image, Command: append([]string(nil), service.Command...),
				Args: append([]string(nil), service.Args...), Environment: cloneEnvironment(service.Env),
				RestartPolicy: service.Restart, CPUMillis: cpuMillis, MemoryBytes: memoryBytes,
				Mounts:               append([]model.VolumeMount(nil), mounts...),
				StartDeadlineSeconds: uint32(startDeadline / time.Second),
				AutoRollback:         manifest.Deploy.AutoRollback,
				StartBeforeStop:      len(mounts) == 0 && manifest.Deploy.MaxSurge > 0,
			}
			allocation.SpecDigest, err = model.AllocationSpecDigest(allocation)
			if err != nil {
				return nil, nil, err
			}
			allocations = append(allocations, allocation)
		}
	}
	return allocations, nil, nil
}

func nodeBlocker(node model.Node) *blockReason {
	if node.OperatingSystem == "" {
		return &blockReason{status: model.ExecutionPending, reason: "agent_capabilities_pending", message: "waiting for the enrolled agent to report runtime capabilities"}
	}
	if node.OperatingSystem != "linux" || (node.Architecture != "amd64" && node.Architecture != "arm64") {
		return &blockReason{reason: "unsupported_platform", message: "this release requires a Linux amd64 or arm64 agent"}
	}
	if !node.CgroupsV2 {
		return &blockReason{reason: "cgroups_v2_required", message: "the enrolled agent does not report cgroups v2 support"}
	}
	if strings.TrimSpace(node.RuntimeVersion) == "" {
		return &blockReason{reason: "containerd_unavailable", message: "the enrolled agent did not report a compatible containerd runtime"}
	}
	if node.RuntimeVersion != "containerd 2.3.4" {
		return &blockReason{reason: "unsupported_runtime_version", message: "this release requires containerd 2.3.4"}
	}
	if strings.TrimSpace(node.Snapshotter) == "" {
		return &blockReason{reason: "snapshotter_unavailable", message: "the enrolled agent did not report a configured containerd snapshotter"}
	}
	return nil
}

func allocationID(clusterID, application, service string, replica uint32) string {
	digest := sha256.Sum256([]byte(clusterID + "\x00" + application + "\x00" + service + "\x00" + strconv.FormatUint(uint64(replica), 10)))
	return "alloc_" + hex.EncodeToString(digest[:16])
}

func parseCPUMillis(value string) (uint64, error) {
	if strings.HasSuffix(value, "m") {
		amount, err := strconv.ParseUint(strings.TrimSuffix(value, "m"), 10, 64)
		if err != nil || amount == 0 {
			return 0, errors.New("stored CPU limit is invalid")
		}
		return amount, nil
	}
	amount, err := strconv.ParseUint(value, 10, 64)
	if err != nil || amount == 0 || amount > ^uint64(0)/1000 {
		return 0, errors.New("stored CPU limit is invalid")
	}
	return amount * 1000, nil
}

func parseMemoryBytes(value string) (uint64, error) {
	units := map[string]uint64{"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40}
	for suffix, multiplier := range units {
		if strings.HasSuffix(value, suffix) {
			amount, err := strconv.ParseUint(strings.TrimSuffix(value, suffix), 10, 64)
			if err != nil || amount == 0 || amount > uint64(1<<63-1)/multiplier {
				return 0, errors.New("stored memory limit is invalid")
			}
			return amount * multiplier, nil
		}
	}
	return 0, errors.New("stored memory limit is invalid")
}

func cloneEnvironment(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func executionState(status string, generation uint64, reason, message string, now time.Time) model.ApplicationExecution {
	return model.ApplicationExecution{
		Status: status, ObservedGeneration: generation, Reason: reason, Message: message, UpdatedAt: now,
	}
}
