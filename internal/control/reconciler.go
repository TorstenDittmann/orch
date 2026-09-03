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
	"change.codes/torsten/better-swarm/internal/ingress"
	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/network"
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
	cluster, err := r.state.Cluster(ctx)
	if err != nil {
		return fmt.Errorf("read cluster network configuration: %w", err)
	}
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
	networkConfig, err := model.FirstNodeNetwork(cluster.WorkloadCIDR, cluster.NetworkGeneration)
	if err != nil {
		return fmt.Errorf("derive first-node network: %w", err)
	}
	domainOwners, err := desiredDomainOwners(applications)
	if err != nil {
		return err
	}
	portOwners, err := desiredTCPPortOwners(applications)
	if err != nil {
		return err
	}
	previousByID := make(map[string]model.AllocationDesired)
	previousCronByApplication := make(map[string][]model.CronJobDesired)
	if previous, previousErr := r.state.CurrentAssignment(ctx, node.ID); previousErr == nil {
		for _, allocation := range previous.Allocations {
			previousByID[allocation.ID] = allocation
		}
		for _, job := range previous.CronJobs {
			previousCronByApplication[job.Application] = append(previousCronByApplication[job.Application], job)
		}
	} else if !errors.Is(previousErr, store.ErrAssignmentNotFound) {
		return fmt.Errorf("read previous assignment: %w", previousErr)
	}
	allocations := make([]model.AllocationDesired, 0)
	cronJobs := make([]model.CronJobDesired, 0)
	states := make(map[string]model.ApplicationExecution, len(applications))
	for _, application := range applications {
		if application.DeletedAt != nil {
			if application.Execution.Status != model.ExecutionRemoved {
				states[application.Name] = executionState(model.ExecutionStopping, 0,
					"removal_pending", "waiting for the agent to remove runtime objects", now)
			}
			continue
		}
		planned, blocked, err := planApplicationBundleForNetwork(cluster.ID, node, application,
			networkConfig, cluster.NetworkFrozenAt != nil, domainOwners, portOwners, previousByID)
		if err != nil {
			return fmt.Errorf("plan application %q: %w", application.Name, err)
		}
		if blocked != nil {
			if blocked.preservePrevious {
				allocations = append(allocations, previousApplicationAllocations(previousByID, application.Name)...)
				cronJobs = append(cronJobs, previousCronByApplication[application.Name]...)
			}
			status := blocked.status
			if status == "" {
				status = model.ExecutionBlocked
			}
			states[application.Name] = executionState(status, application.Generation,
				blocked.reason, blocked.message, now)
			continue
		}
		allocations = append(allocations, planned.allocations...)
		cronJobs = append(cronJobs, planned.cronJobs...)
		if application.Execution.ObservedGeneration != application.Generation ||
			application.Execution.Status == model.ExecutionBlocked || application.Execution.Status == model.ExecutionRemoved {
			states[application.Name] = executionState(model.ExecutionPending, 0,
				"assignment_pending", "desired state is waiting for durable agent receipt", now)
		}
	}
	sort.Slice(allocations, func(i, j int) bool { return allocations[i].ID < allocations[j].ID })
	sort.Slice(cronJobs, func(i, j int) bool {
		if cronJobs[i].Application == cronJobs[j].Application {
			return cronJobs[i].Job < cronJobs[j].Job
		}
		return cronJobs[i].Application < cronJobs[j].Application
	})
	payload, digest, err := model.AssignmentPayloadWithJobs(networkConfig, allocations, cronJobs)
	if err != nil {
		return err
	}
	assignment, changed, err := r.state.PublishAssignment(ctx, model.PublishAssignment{
		NodeID: node.ID, Digest: digest, Payload: payload, Network: networkConfig,
		Allocations: allocations, CronJobs: cronJobs, ApplicationStates: states, CreatedAt: now,
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
	status           string
	reason           string
	message          string
	preservePrevious bool
}

type plannedApplication struct {
	allocations []model.AllocationDesired
	cronJobs    []model.CronJobDesired
}

func planApplication(clusterID string, node model.Node, application model.Application) ([]model.AllocationDesired, *blockReason, error) {
	networkConfig, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		return nil, nil, err
	}
	return planApplicationForNetwork(clusterID, node, application, networkConfig, true, nil, nil)
}

func planApplicationForNetwork(clusterID string, node model.Node, application model.Application,
	networkConfig model.NetworkConfig, networkFrozen bool, domainOwners map[string]string,
	previousByID map[string]model.AllocationDesired,
) ([]model.AllocationDesired, *blockReason, error) {
	planned, blocked, err := planApplicationBundleForNetwork(clusterID, node, application, networkConfig,
		networkFrozen, domainOwners, nil, previousByID)
	return planned.allocations, blocked, err
}

func planApplicationBundleForNetwork(clusterID string, node model.Node, application model.Application,
	networkConfig model.NetworkConfig, networkFrozen bool, domainOwners map[string]string,
	portOwners map[uint32]string, previousByID map[string]model.AllocationDesired,
) (plannedApplication, *blockReason, error) {
	var manifest config.Manifest
	if err := json.Unmarshal(application.CanonicalManifest, &manifest); err != nil {
		return plannedApplication{}, nil, fmt.Errorf("decode stored manifest: %w", err)
	}
	if err := config.Validate(&manifest); err != nil {
		return plannedApplication{}, nil, fmt.Errorf("validate stored manifest: %w", err)
	}
	if blocked := nodeBlocker(node); blocked != nil {
		return plannedApplication{}, blocked, nil
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
			return plannedApplication{}, nil, fmt.Errorf("parse stored image reference: %w", err)
		}
		if _, ok := parsed.(reference.Digested); !ok {
			return plannedApplication{}, &blockReason{
				reason:  "unsupported_image_tag",
				message: base + ".image must use a public digest-qualified OCI reference",
			}, nil
		}
		domain := reference.Domain(parsed)
		if domain == "localhost" || strings.HasPrefix(domain, "localhost:") {
			return plannedApplication{}, &blockReason{
				reason:  "private_registry_unsupported",
				message: base + ".image requires a private/local registry; registry credentials are not available in this release",
			}, nil
		}
		if len(service.SecretEnv) != 0 || len(service.SecretFiles) != 0 {
			return plannedApplication{}, &blockReason{
				reason:  "secrets_unsupported",
				message: base + " contains secret references; secrets are not executable in this release",
			}, nil
		}
		if len(service.Constraints) != 0 {
			return plannedApplication{}, &blockReason{
				reason:  "constraints_unsupported",
				message: base + ".constraints requires a later scheduling release",
			}, nil
		}
		if service.HTTP != nil && len(service.HTTP.Domains) != 0 {
			for _, domain := range service.HTTP.Domains {
				owner := application.Name + "/" + name
				if domainOwners != nil && domainOwners[domain] != owner {
					return plannedApplication{}, &blockReason{reason: "domain_conflict", message: base + ".http domain " + domain + " is already owned by an earlier application"}, nil
				}
			}
		}
		if service.TCP != nil && service.TCP.PublicPort != nil {
			port := uint32(*service.TCP.PublicPort)
			owner := application.Name + "/" + name
			if portOwners != nil && portOwners[port] != owner {
				return plannedApplication{}, &blockReason{reason: "tcp_port_conflict", message: fmt.Sprintf("%s.tcp public port %d is already owned by an earlier application", base, port)}, nil
			}
		}
	}
	for name, job := range manifest.CronJobs {
		parsed, err := reference.ParseNormalizedNamed(job.Image)
		if err != nil {
			return plannedApplication{}, nil, fmt.Errorf("parse stored cron image reference: %w", err)
		}
		if _, ok := parsed.(reference.Digested); !ok {
			return plannedApplication{}, &blockReason{reason: "unsupported_image_tag", message: "cronJobs." + name + ".image must use a public digest-qualified OCI reference"}, nil
		}
		domain := reference.Domain(parsed)
		if domain == "localhost" || strings.HasPrefix(domain, "localhost:") {
			return plannedApplication{}, &blockReason{reason: "private_registry_unsupported", message: "cronJobs." + name + ".image requires a private/local registry"}, nil
		}
	}
	if !networkFrozen || node.NetworkGeneration != networkConfig.Generation ||
		node.NodeSubnet != networkConfig.NodeSubnet || node.NetworkState != model.NetworkReady ||
		node.DNSState != model.SubsystemReady {
		return plannedApplication{}, &blockReason{
			status:           model.ExecutionPending,
			reason:           "network_initializing",
			message:          "waiting for the agent to validate and freeze the workload network",
			preservePrevious: true,
		}, nil
	}

	allocations := make([]model.AllocationDesired, 0)
	for _, serviceName := range serviceNames {
		service := manifest.Services[serviceName]
		cpuMillis, err := parseCPUMillis(service.Resources.CPU)
		if err != nil {
			return plannedApplication{}, nil, err
		}
		memoryBytes, err := parseMemoryBytes(service.Resources.Memory)
		if err != nil {
			return plannedApplication{}, nil, err
		}
		startDeadline, err := time.ParseDuration(manifest.Deploy.HealthDeadline)
		if err != nil {
			return plannedApplication{}, nil, fmt.Errorf("parse stored process-start deadline: %w", err)
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
				NetworkGeneration:    networkConfig.Generation,
				MaxSurge:             uint32(manifest.Deploy.MaxSurge),
				MaxUnavailable:       uint32(manifest.Deploy.MaxUnavailable),
			}
			if service.HTTP != nil {
				allocation.HTTP = &model.HTTPDesired{
					Port: uint32(service.HTTP.Port), Domains: append([]string(nil), service.HTTP.Domains...),
					HealthPath: service.HTTP.HealthPath, HTTPS: service.HTTP.HTTPS,
				}
			}
			if service.TCP != nil {
				allocation.TCP = &model.TCPDesired{Port: uint32(service.TCP.Port)}
				if service.TCP.PublicPort != nil {
					allocation.TCP.PublicPort = uint32(*service.TCP.PublicPort)
				}
			}
			allocation.DependsOn = append([]string(nil), service.DependsOn...)
			allocation.SpecDigest, err = model.AllocationSpecDigest(allocation)
			if err != nil {
				return plannedApplication{}, nil, err
			}
			if previous := previousByID[allocation.ID]; previous.SpecDigest != "" {
				sameDesiredRevision := previous.ApplicationGeneration == allocation.ApplicationGeneration &&
					previous.RevisionID == allocation.RevisionID && previous.SpecDigest == allocation.SpecDigest
				if sameDesiredRevision {
					allocation.Rollback = cloneRollbackSpec(previous.Rollback)
				} else if previous.Rollback != nil &&
					previous.Rollback.ApplicationGeneration == application.Execution.ActiveGeneration &&
					previous.Rollback.RevisionID == application.Execution.ActiveRevisionID {
					allocation.Rollback = cloneRollbackSpec(previous.Rollback)
				} else {
					// Every allocation carries application-level rollback authority,
					// including services whose immutable runtime spec did not change.
					// That lets one failed service restore coherent revision metadata
					// for the whole application without restarting equivalent tasks.
					allocation.Rollback = rollbackSpec(previous)
				}
			}
			allocations = append(allocations, allocation)
		}
	}
	jobNames := make([]string, 0, len(manifest.CronJobs))
	for name := range manifest.CronJobs {
		jobNames = append(jobNames, name)
	}
	sort.Strings(jobNames)
	cronJobs := make([]model.CronJobDesired, 0, len(jobNames))
	for _, name := range jobNames {
		job := manifest.CronJobs[name]
		cpuMillis, err := parseCPUMillis(job.Resources.CPU)
		if err != nil {
			return plannedApplication{}, nil, err
		}
		memoryBytes, err := parseMemoryBytes(job.Resources.Memory)
		if err != nil {
			return plannedApplication{}, nil, err
		}
		startDeadline, err := time.ParseDuration(job.StartDeadline)
		if err != nil {
			return plannedApplication{}, nil, fmt.Errorf("parse stored cron start deadline: %w", err)
		}
		timeout, err := time.ParseDuration(job.Timeout)
		if err != nil {
			return plannedApplication{}, nil, fmt.Errorf("parse stored cron timeout: %w", err)
		}
		desired := model.CronJobDesired{
			Application: application.Name, Job: name, ApplicationGeneration: application.Generation,
			RevisionID: application.RevisionID, Image: job.Image, Schedule: job.Schedule,
			Command: append([]string(nil), job.Command...), Args: append([]string(nil), job.Args...),
			Environment: cloneEnvironment(job.Env), CPUMillis: cpuMillis, MemoryBytes: memoryBytes,
			DependsOn: append([]string(nil), job.DependsOn...), StartDeadlineSeconds: uint32(startDeadline / time.Second),
			TimeoutSeconds: uint32(timeout / time.Second), NetworkGeneration: networkConfig.Generation,
		}
		desired.SpecDigest, err = model.CronJobSpecDigest(desired)
		if err != nil {
			return plannedApplication{}, nil, err
		}
		cronJobs = append(cronJobs, desired)
	}
	return plannedApplication{allocations: allocations, cronJobs: cronJobs}, nil, nil
}

func previousApplicationAllocations(previous map[string]model.AllocationDesired, application string) []model.AllocationDesired {
	result := make([]model.AllocationDesired, 0)
	for _, allocation := range previous {
		if allocation.Application == application {
			result = append(result, allocation)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func nodeBlocker(node model.Node) *blockReason {
	if node.OperatingSystem == "" {
		return nodeBlock(model.ExecutionPending, "agent_capabilities_pending", "waiting for the enrolled agent to report runtime capabilities")
	}
	if node.OperatingSystem != "linux" || (node.Architecture != "amd64" && node.Architecture != "arm64") {
		return nodeBlock("", "unsupported_platform", "this release requires a Linux amd64 or arm64 agent")
	}
	if !node.CgroupsV2 {
		return nodeBlock("", "cgroups_v2_required", "the enrolled agent does not report cgroups v2 support")
	}
	if strings.TrimSpace(node.RuntimeVersion) == "" {
		return nodeBlock("", "containerd_unavailable", "the enrolled agent did not report a compatible containerd runtime")
	}
	if node.RuntimeVersion != "containerd 2.3.4" {
		return nodeBlock("", "unsupported_runtime_version", "this release requires containerd 2.3.4")
	}
	if strings.TrimSpace(node.Snapshotter) == "" {
		return nodeBlock("", "snapshotter_unavailable", "the enrolled agent did not report a configured containerd snapshotter")
	}
	if node.CNIVersion != network.CNISpecVersion {
		return nodeBlock("", "cni_unavailable", "the enrolled agent does not report CNI 1.1.0 support")
	}
	if node.CNIPluginsVersion != network.CNIPluginsVersion {
		return nodeBlock("", "cni_plugins_unavailable", "the enrolled agent does not report the official CNI plugins v1.9.1 bundle")
	}
	if !node.Nftables {
		return nodeBlock("", "nftables_required", "the enrolled agent does not report nftables support")
	}
	if !node.DNSAvailable {
		return nodeBlock("", "dns_unavailable", "the enrolled agent does not report an available workload DNS service")
	}
	if node.CaddyVersion != ingress.RequiredCaddyVersion {
		return nodeBlock("", "caddy_unavailable", "the enrolled agent does not report Caddy "+ingress.RequiredCaddyVersion)
	}
	if node.NetworkState == model.NetworkFailed {
		reason := node.NetworkReason
		if reason == "" {
			reason = "network_preflight_failed"
		}
		message := node.NetworkMessage
		if message == "" {
			message = "the enrolled agent could not prepare the workload network"
		}
		return nodeBlock("", reason, message)
	}
	return nil
}

func nodeBlock(status, reason, message string) *blockReason {
	return &blockReason{status: status, reason: reason, message: message, preservePrevious: true}
}

func rollbackSpec(allocation model.AllocationDesired) *model.RollbackSpec {
	return &model.RollbackSpec{
		ApplicationGeneration: allocation.ApplicationGeneration, RevisionID: allocation.RevisionID,
		SpecDigest: allocation.SpecDigest, Image: allocation.Image,
		Command: append([]string(nil), allocation.Command...), Args: append([]string(nil), allocation.Args...),
		Environment: cloneEnvironment(allocation.Environment), RestartPolicy: allocation.RestartPolicy,
		CPUMillis: allocation.CPUMillis, MemoryBytes: allocation.MemoryBytes,
		Mounts:               append([]model.VolumeMount(nil), allocation.Mounts...),
		StartDeadlineSeconds: allocation.StartDeadlineSeconds,
		HTTP:                 allocation.HTTP, TCP: allocation.TCP,
		DependsOn: append([]string(nil), allocation.DependsOn...), NetworkGeneration: allocation.NetworkGeneration,
		AutoRollback: allocation.AutoRollback, StartBeforeStop: allocation.StartBeforeStop,
	}
}

func cloneRollbackSpec(input *model.RollbackSpec) *model.RollbackSpec {
	if input == nil {
		return nil
	}
	result := *input
	result.Command = append([]string(nil), input.Command...)
	result.Args = append([]string(nil), input.Args...)
	result.Environment = cloneEnvironment(input.Environment)
	result.Mounts = append([]model.VolumeMount(nil), input.Mounts...)
	if input.HTTP != nil {
		http := *input.HTTP
		http.Domains = append([]string(nil), input.HTTP.Domains...)
		result.HTTP = &http
	}
	if input.TCP != nil {
		tcp := *input.TCP
		result.TCP = &tcp
	}
	result.DependsOn = append([]string(nil), input.DependsOn...)
	return &result
}

func desiredDomainOwners(applications []model.Application) (map[string]string, error) {
	type candidate struct {
		created time.Time
		app     string
		service string
		domain  string
	}
	var candidates []candidate
	for _, application := range applications {
		if application.DeletedAt != nil {
			continue
		}
		var manifest config.Manifest
		if err := json.Unmarshal(application.CanonicalManifest, &manifest); err != nil {
			return nil, fmt.Errorf("decode stored manifest for domain ownership: %w", err)
		}
		for serviceName, service := range manifest.Services {
			if service.HTTP == nil {
				continue
			}
			for _, domain := range service.HTTP.Domains {
				candidates = append(candidates, candidate{application.CreatedAt, application.Name, serviceName, domain})
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].created.Equal(candidates[j].created) {
			return candidates[i].created.Before(candidates[j].created)
		}
		if candidates[i].app != candidates[j].app {
			return candidates[i].app < candidates[j].app
		}
		return candidates[i].service < candidates[j].service
	})
	owners := make(map[string]string)
	for _, candidate := range candidates {
		if _, exists := owners[candidate.domain]; !exists {
			owners[candidate.domain] = candidate.app + "/" + candidate.service
		}
	}
	return owners, nil
}

func desiredTCPPortOwners(applications []model.Application) (map[uint32]string, error) {
	type candidate struct {
		created time.Time
		app     string
		service string
		port    uint32
	}
	var candidates []candidate
	for _, application := range applications {
		if application.DeletedAt != nil {
			continue
		}
		var manifest config.Manifest
		if err := json.Unmarshal(application.CanonicalManifest, &manifest); err != nil {
			return nil, fmt.Errorf("decode stored manifest for TCP port ownership: %w", err)
		}
		for serviceName, service := range manifest.Services {
			if service.TCP != nil && service.TCP.PublicPort != nil {
				candidates = append(candidates, candidate{application.CreatedAt, application.Name, serviceName, uint32(*service.TCP.PublicPort)})
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].created.Equal(candidates[j].created) {
			return candidates[i].created.Before(candidates[j].created)
		}
		if candidates[i].app != candidates[j].app {
			return candidates[i].app < candidates[j].app
		}
		return candidates[i].service < candidates[j].service
	})
	owners := make(map[uint32]string)
	for _, candidate := range candidates {
		if _, exists := owners[candidate.port]; !exists {
			owners[candidate.port] = candidate.app + "/" + candidate.service
		}
	}
	return owners, nil
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
