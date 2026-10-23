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
	"sync"
	"time"
	"unicode/utf8"

	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/ingress"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/network"
	"github.com/TorstenDittmann/orch/internal/security"
	"github.com/TorstenDittmann/orch/internal/store"
	"github.com/distribution/reference"
)

// Reconciler turns durable application intent into one immutable full-node
// snapshot. It deliberately contains no placement algorithm: this release has
// at most one node.
type Reconciler struct {
	mu          sync.Mutex
	state       *store.Store
	cluster     model.Cluster
	now         func() time.Time
	onPublished func(model.Assignment)
	secretBox   *security.SecretBox
	sessions    *sessionManager
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
	r.mu.Lock()
	defer r.mu.Unlock()
	cluster, err := r.state.Cluster(ctx)
	if err != nil {
		return fmt.Errorf("read cluster network configuration: %w", err)
	}
	applications, err := r.state.Applications(ctx, true)
	if err != nil {
		return fmt.Errorf("list desired applications: %w", err)
	}
	scales, err := r.state.ServiceScales(ctx)
	if err != nil {
		return err
	}
	if err := applyServiceScales(applications, scales); err != nil {
		return err
	}
	nodes, err := r.state.Nodes(ctx)
	if err != nil {
		return fmt.Errorf("list execution nodes: %w", err)
	}
	now := r.now().UTC()
	if err := r.state.CancelObsoleteJobs(ctx); err != nil {
		return err
	}
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
	jobs, err := r.state.Jobs(ctx, "", "", 100, true)
	if err != nil {
		return err
	}
	hasHooks := false
	for _, app := range applications {
		var manifest config.Manifest
		if json.Unmarshal(app.CanonicalManifest, &manifest) == nil && manifest.Deploy.PreDeploy != nil {
			hasHooks = true
		}
	}
	if len(jobs) > 0 || hasHooks || usesDistributedScheduler(nodes, applications) {
		return r.reconcileDistributed(ctx, cluster, applications, nodes, now)
	}
	return r.reconcileLegacy(ctx, cluster, applications, nodes, now)
}

func (r *Reconciler) reconcileLegacy(ctx context.Context, cluster model.Cluster, applications []model.Application, nodes []model.Node, now time.Time) error {
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
			if job.RunID != "" {
				continue
			}
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
		resolved, secretBlocked, err := r.resolveApplicationIntent(ctx, application)
		if err != nil {
			return fmt.Errorf("resolve application %q secrets: %w", application.Name, err)
		}
		if secretBlocked != nil {
			if secretBlocked.preservePrevious {
				allocations = append(allocations, previousApplicationAllocations(previousByID, application.Name)...)
				cronJobs = append(cronJobs, previousCronByApplication[application.Name]...)
			}
			states[application.Name] = executionState(model.ExecutionBlocked, application.Generation,
				secretBlocked.reason, secretBlocked.message, now)
			continue
		}
		planned, blocked, err := planApplicationBundleForNetwork(cluster.ID, node, application,
			networkConfig, cluster.NetworkFrozenAt != nil, domainOwners, portOwners, previousByID, resolved)
		resolved.wipe()
		if err != nil {
			blocked = invalidIntentBlock(err)
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
		hydrated, err := r.HydrateAssignment(ctx, assignment)
		if err != nil {
			return fmt.Errorf("hydrate assignment secret material: %w", err)
		}
		r.onPublished(hydrated)
		wipeMaterials(hydrated.Materials)
	}
	return nil
}

func usesDistributedScheduler(nodes []model.Node, applications []model.Application) bool {
	if len(nodes) > 1 || applicationsUseConstraints(applications) {
		return true
	}
	for _, node := range nodes {
		if node.Ordinal != 0 || node.Lifecycle != model.NodeLifecycleActive || node.TotalCPUMillis != 0 ||
			node.AllocatableCPUMillis != 0 || node.AdvertiseAddress != "" || node.WireGuardPublicKey != "" {
			return true
		}
	}
	return false
}

type blockReason struct {
	status           string
	reason           string
	message          string
	preservePrevious bool
}

// invalidIntentBlock confines a stored application that can no longer be
// planned, for example after an upgrade tightened validation, to that
// application. Its previously published placement keeps running so one bad
// application never stops the reconcile for the rest of the cluster.
func invalidIntentBlock(err error) *blockReason {
	return &blockReason{reason: "invalid_stored_intent",
		message: "stored application intent cannot be planned: " + err.Error(), preservePrevious: true}
}

type plannedApplication struct {
	allocations []model.AllocationDesired
	cronJobs    []model.CronJobDesired
}

type resolvedApplicationIntent struct {
	secrets    map[string]model.ResolvedSecret
	registries map[string]model.ResolvedRegistryCredential
}

func (r *resolvedApplicationIntent) wipe() {
	if r == nil {
		return
	}
	for name, item := range r.secrets {
		wipeBytes(item.Value)
		delete(r.secrets, name)
	}
	for host, item := range r.registries {
		wipeBytes(item.Secret)
		delete(r.registries, host)
	}
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
	portOwners map[uint32]string, previousByID map[string]model.AllocationDesired, resolvedValues ...*resolvedApplicationIntent,
) (plannedApplication, *blockReason, error) {
	var resolved *resolvedApplicationIntent
	if len(resolvedValues) != 0 {
		resolved = resolvedValues[0]
	}
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
		if (len(service.SecretEnv) != 0 || len(service.SecretFiles) != 0) && resolved == nil {
			return plannedApplication{}, &blockReason{
				reason:  "secrets_unsupported",
				message: base + " contains secret references but no secret resolver is configured",
			}, nil
		}
		if service.HTTP != nil && service.HTTP.BasicAuth != nil && resolved == nil {
			return plannedApplication{}, &blockReason{
				reason:  "secrets_unsupported",
				message: base + ".http.basicAuth references a secret but no secret resolver is configured",
			}, nil
		}
		if service.HTTP != nil && len(service.HTTP.Domains)+len(service.HTTP.Redirects) != 0 {
			for _, domain := range httpRouteDomains(service.HTTP) {
				owner := application.Name + "/" + name
				if domainOwners != nil && domainOwners[domain] != owner {
					return plannedApplication{}, &blockReason{reason: "domain_conflict", message: base + ".http domain " + domain + " is already owned by an earlier application"}, nil
				}
			}
		}
		if service.HTTP != nil && service.HTTP.Public {
			owner := application.Name + "/" + name
			if domainOwners != nil && domainOwners[model.PublicHTTPRoute] != owner {
				return plannedApplication{}, &blockReason{reason: "http_public_conflict", message: base + ".http public listener is already owned by an earlier application"}, nil
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
		if (len(job.SecretEnv) != 0 || len(job.SecretFiles) != 0) && resolved == nil {
			return plannedApplication{}, &blockReason{reason: "secrets_unsupported", message: "cronJobs." + name + " contains secret references but no secret resolver is configured"}, nil
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
		var stopGrace time.Duration
		if service.StopGrace != "" {
			if stopGrace, err = time.ParseDuration(service.StopGrace); err != nil {
				return plannedApplication{}, nil, fmt.Errorf("parse stored stop grace period: %w", err)
			}
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
		backups := volumeBackups(&manifest, mounts)
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
				StopGraceSeconds:     uint32(stopGrace / time.Second),
				Privileged:           service.Privileged,
				VolumeBackups:        append([]model.VolumeBackupDesired(nil), backups...),
			}
			if service.HTTP != nil {
				allocation.HTTP = &model.HTTPDesired{
					Port: uint32(service.HTTP.Port), Public: service.HTTP.Public, Domains: append([]string(nil), service.HTTP.Domains...),
					HealthPath: service.HTTP.HealthPath, HTTPS: service.HTTP.HTTPS, NodeLocal: service.HTTP.NodeLocal,
					Redirects: cloneEnvironment(service.HTTP.Redirects),
				}
				if auth := service.HTTP.BasicAuth; auth != nil {
					allocation.HTTP.BasicAuth = &model.HTTPBasicAuth{User: auth.User, PasswordHash: basicAuthHash(resolved.secrets[auth.PasswordSecret])}
				}
			}
			if service.TCP != nil {
				allocation.TCP = &model.TCPDesired{Port: uint32(service.TCP.Port)}
				if service.TCP.PublicPort != nil {
					allocation.TCP.PublicPort = uint32(*service.TCP.PublicPort)
				}
				if service.TCP.NodePort != nil {
					allocation.TCP.NodePort = uint32(*service.TCP.NodePort)
				}
			}
			if service.UDP != nil {
				allocation.UDP = &model.UDPDesired{Port: uint32(service.UDP.Port), NodePort: uint32(service.UDP.NodePort)}
			}
			allocation.DependsOn = append([]string(nil), service.DependsOn...)
			allocation.SecretEnvironment, allocation.SecretFiles = desiredSecretBindings(service.SecretEnv, service.SecretFiles, resolved)
			allocation.RegistryCredential = desiredRegistryCredential(service.Image, resolved)
			allocation.SpecDigest, err = model.AllocationSpecDigest(allocation)
			if err != nil {
				return plannedApplication{}, nil, err
			}
			if previous := previousByID[allocation.ID]; previous.SpecDigest != "" {
				// The same revision keeps its rollback authority even when a
				// secret rotation changes the spec: a rollback to the generation
				// the agent is already running is invalid and the agent rejects
				// the whole assignment.
				sameDesiredRevision := previous.ApplicationGeneration == allocation.ApplicationGeneration &&
					previous.RevisionID == allocation.RevisionID && previous.NetworkGeneration == allocation.NetworkGeneration
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
		for _, mount := range job.Mounts {
			desired.Mounts = append(desired.Mounts, model.VolumeMount{Name: mount.Source, Target: mount.Target, ReadOnly: true})
		}
		desired.SecretEnvironment, desired.SecretFiles = desiredSecretBindings(job.SecretEnv, job.SecretFiles, resolved)
		desired.RegistryCredential = desiredRegistryCredential(job.Image, resolved)
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
	if node.Ingress && node.CaddyVersion != ingress.RequiredCaddyVersion {
		return nodeBlock("", "caddy_unavailable", "the enrolled agent does not report Caddy "+ingress.RequiredCaddyVersion)
	}
	return nil
}

// volumeBackups returns the backup policies of the local volumes a service
// mounts, sorted by volume. Each volume has exactly one owning service.
func volumeBackups(manifest *config.Manifest, mounts []model.VolumeMount) []model.VolumeBackupDesired {
	var backups []model.VolumeBackupDesired
	for _, mount := range mounts {
		backup := manifest.Volumes[mount.Name].Backup
		if backup == nil {
			continue
		}
		backups = append(backups, model.VolumeBackupDesired{
			Volume: mount.Name, Schedule: backup.Schedule, Keep: uint32(backup.Keep),
			Before: append([]string(nil), backup.Before...),
		})
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].Volume < backups[j].Volume })
	return backups
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
		HTTP:                 allocation.HTTP, TCP: allocation.TCP, UDP: allocation.UDP,
		DependsOn: append([]string(nil), allocation.DependsOn...), NetworkGeneration: allocation.NetworkGeneration,
		SecretEnvironment:  cloneSecretEnvironment(allocation.SecretEnvironment),
		SecretFiles:        append([]model.SecretFileDesired(nil), allocation.SecretFiles...),
		RegistryCredential: cloneRegistryCredential(allocation.RegistryCredential),
		AutoRollback:       allocation.AutoRollback, StartBeforeStop: allocation.StartBeforeStop,
		StopGraceSeconds: allocation.StopGraceSeconds, Privileged: allocation.Privileged,
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
		http.Redirects = cloneEnvironment(input.HTTP.Redirects)
		if input.HTTP.BasicAuth != nil {
			auth := *input.HTTP.BasicAuth
			http.BasicAuth = &auth
		}
		result.HTTP = &http
	}
	if input.TCP != nil {
		tcp := *input.TCP
		result.TCP = &tcp
	}
	if input.UDP != nil {
		udp := *input.UDP
		result.UDP = &udp
	}
	result.DependsOn = append([]string(nil), input.DependsOn...)
	result.SecretEnvironment = cloneSecretEnvironment(input.SecretEnvironment)
	result.SecretFiles = append([]model.SecretFileDesired(nil), input.SecretFiles...)
	result.RegistryCredential = cloneRegistryCredential(input.RegistryCredential)
	return &result
}

func (r *Reconciler) resolveApplicationIntent(ctx context.Context, application model.Application) (*resolvedApplicationIntent, *blockReason, error) {
	var manifest config.Manifest
	if err := json.Unmarshal(application.CanonicalManifest, &manifest); err != nil {
		return nil, invalidIntentBlock(err), nil
	}
	result := &resolvedApplicationIntent{secrets: make(map[string]model.ResolvedSecret), registries: make(map[string]model.ResolvedRegistryCredential)}
	secretUses := make(map[string]bool)
	images := make([]string, 0, len(manifest.Services)+len(manifest.CronJobs))
	for _, service := range manifest.Services {
		images = append(images, service.Image)
		for _, name := range service.SecretEnv {
			secretUses[name] = true
		}
		for _, item := range service.SecretFiles {
			if _, exists := secretUses[item.Source]; !exists {
				secretUses[item.Source] = false
			}
		}
		if service.HTTP != nil && service.HTTP.BasicAuth != nil {
			if _, exists := secretUses[service.HTTP.BasicAuth.PasswordSecret]; !exists {
				secretUses[service.HTTP.BasicAuth.PasswordSecret] = false
			}
		}
	}
	for _, job := range manifest.CronJobs {
		images = append(images, job.Image)
		for _, name := range job.SecretEnv {
			secretUses[name] = true
		}
		for _, item := range job.SecretFiles {
			if _, exists := secretUses[item.Source]; !exists {
				secretUses[item.Source] = false
			}
		}
	}
	if len(secretUses) != 0 && r.secretBox == nil {
		return nil, &blockReason{reason: "secrets_unsupported", message: "controller secret key is unavailable", preservePrevious: true}, nil
	}
	names := make([]string, 0, len(secretUses))
	for name := range secretUses {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		resolved, err := r.state.ResolveApplicationSecret(ctx, r.secretBox, application.Name, name)
		if errors.Is(err, store.ErrSecretNotFound) {
			result.wipe()
			return nil, &blockReason{reason: "secret_missing", message: "application secret " + application.Name + "/" + name + " is not set", preservePrevious: true}, nil
		}
		if err != nil {
			result.wipe()
			return nil, nil, err
		}
		if secretUses[name] && (!utf8.Valid(resolved.Value) || strings.ContainsRune(string(resolved.Value), 0)) {
			wipeBytes(resolved.Value)
			result.wipe()
			return nil, &blockReason{reason: "secret_environment_invalid", message: "application secret " + application.Name + "/" + name + " cannot be projected into an environment variable", preservePrevious: true}, nil
		}
		result.secrets[name] = resolved
	}
	for _, image := range images {
		parsed, err := reference.ParseNormalizedNamed(image)
		if err != nil {
			result.wipe()
			return nil, invalidIntentBlock(err), nil
		}
		host := reference.Domain(parsed)
		if _, exists := result.registries[host]; exists || r.secretBox == nil {
			continue
		}
		credential, err := r.state.ResolveRegistryCredential(ctx, r.secretBox, host)
		if errors.Is(err, store.ErrRegistryCredentialNotFound) {
			continue
		}
		if err != nil {
			result.wipe()
			return nil, nil, err
		}
		result.registries[host] = credential
	}
	return result, nil, nil
}

func desiredSecretBindings(environment map[string]string, files []config.SecretFile, resolved *resolvedApplicationIntent) (map[string]model.SecretReference, []model.SecretFileDesired) {
	if resolved == nil {
		return nil, nil
	}
	env := make(map[string]model.SecretReference, len(environment))
	for key, name := range environment {
		item := resolved.secrets[name]
		env[key] = model.SecretReference{Name: name, VersionID: item.VersionID}
	}
	if len(env) == 0 {
		env = nil
	}
	secretFiles := make([]model.SecretFileDesired, 0, len(files))
	for _, file := range files {
		item := resolved.secrets[file.Source]
		secretFiles = append(secretFiles, model.SecretFileDesired{SecretReference: model.SecretReference{Name: file.Source, VersionID: item.VersionID}, Target: file.Target})
	}
	return env, secretFiles
}

func desiredRegistryCredential(image string, resolved *resolvedApplicationIntent) *model.RegistryCredentialReference {
	if resolved == nil {
		return nil
	}
	parsed, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return nil
	}
	item, exists := resolved.registries[reference.Domain(parsed)]
	if !exists {
		return nil
	}
	return &model.RegistryCredentialReference{Host: item.Host, VersionID: item.VersionID, Username: item.Username}
}

func cloneSecretEnvironment(input map[string]model.SecretReference) map[string]model.SecretReference {
	if len(input) == 0 {
		return nil
	}
	result := make(map[string]model.SecretReference, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func cloneRegistryCredential(input *model.RegistryCredentialReference) *model.RegistryCredentialReference {
	if input == nil {
		return nil
	}
	result := *input
	return &result
}

func (r *Reconciler) HydrateAssignment(ctx context.Context, assignment model.Assignment) (model.Assignment, error) {
	if r.secretBox == nil {
		return assignment, nil
	}
	applicationRefs := make(map[string]model.SecretReference)
	registryRefs := make(map[string]model.RegistryCredentialReference)
	collect := func(application string, environment map[string]model.SecretReference, files []model.SecretFileDesired, registry *model.RegistryCredentialReference) {
		for _, ref := range environment {
			applicationRefs[application+"\x00"+ref.Name+"\x00"+ref.VersionID] = ref
		}
		for _, file := range files {
			applicationRefs[application+"\x00"+file.Name+"\x00"+file.VersionID] = file.SecretReference
		}
		if registry != nil {
			registryRefs[registry.Host+"\x00"+registry.VersionID] = *registry
		}
	}
	for _, allocation := range assignment.Allocations {
		collect(allocation.Application, allocation.SecretEnvironment, allocation.SecretFiles, allocation.RegistryCredential)
		if allocation.Rollback != nil {
			collect(allocation.Application, allocation.Rollback.SecretEnvironment, allocation.Rollback.SecretFiles, allocation.Rollback.RegistryCredential)
		}
	}
	for _, job := range assignment.CronJobs {
		collect(job.Application, job.SecretEnvironment, job.SecretFiles, job.RegistryCredential)
	}
	keys := make([]string, 0, len(applicationRefs))
	for key := range applicationRefs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	totalBytes := 0
	for _, key := range keys {
		parts := strings.Split(key, "\x00")
		resolved, err := r.state.ResolveApplicationSecretVersion(ctx, r.secretBox, parts[0], parts[1], parts[2])
		if err != nil {
			wipeMaterials(assignment.Materials)
			return model.Assignment{}, err
		}
		assignment.Materials = append(assignment.Materials, model.SecretMaterial{Kind: model.SecretMaterialApplication,
			Application: resolved.Application, Name: resolved.Name, VersionID: resolved.VersionID, Value: resolved.Value})
		totalBytes += len(resolved.Value)
		if totalBytes > 16<<20 {
			wipeMaterials(assignment.Materials)
			return model.Assignment{}, fmt.Errorf("assignment secret material bundle exceeds 16 MiB")
		}
	}
	keys = keys[:0]
	for key := range registryRefs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		ref := registryRefs[key]
		resolved, err := r.state.ResolveRegistryCredentialVersion(ctx, r.secretBox, ref.Host, ref.VersionID)
		if err != nil {
			wipeMaterials(assignment.Materials)
			return model.Assignment{}, err
		}
		assignment.Materials = append(assignment.Materials, model.SecretMaterial{Kind: model.SecretMaterialRegistry,
			Host: resolved.Host, Username: resolved.Username, VersionID: resolved.VersionID, Value: resolved.Secret})
		totalBytes += len(resolved.Secret)
		if totalBytes > 16<<20 {
			wipeMaterials(assignment.Materials)
			return model.Assignment{}, fmt.Errorf("assignment secret material bundle exceeds 16 MiB")
		}
	}
	return assignment, nil
}

func wipeMaterials(materials []model.SecretMaterial) {
	for index := range materials {
		wipeBytes(materials[index].Value)
	}
}

func wipeBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
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
		if json.Unmarshal(application.CanonicalManifest, &manifest) != nil {
			// Planning blocks the undecodable application on its own.
			continue
		}
		for serviceName, service := range manifest.Services {
			if service.HTTP == nil {
				continue
			}
			if service.HTTP.Public {
				candidates = append(candidates, candidate{application.CreatedAt, application.Name, serviceName, model.PublicHTTPRoute})
			}
			for _, domain := range httpRouteDomains(service.HTTP) {
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
		if json.Unmarshal(application.CanonicalManifest, &manifest) != nil {
			// Planning blocks the undecodable application on its own.
			continue
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

// desiredNodePortOwners maps "tcp/PORT" and "udp/PORT" to the earliest
// application that publishes the port, either node-locally or (for TCP) on
// ingress nodes. Services of one application may share a node-local port; the
// scheduler keeps them on different nodes.
func desiredNodePortOwners(applications []model.Application) (map[string]string, error) {
	sorted := append([]model.Application(nil), applications...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
		}
		return sorted[i].Name < sorted[j].Name
	})
	owners := make(map[string]string)
	for _, application := range sorted {
		if application.DeletedAt != nil {
			continue
		}
		var manifest config.Manifest
		if json.Unmarshal(application.CanonicalManifest, &manifest) != nil {
			// Planning blocks the undecodable application on its own.
			continue
		}
		for _, port := range applicationPortKeys(manifest) {
			if _, exists := owners[port]; !exists {
				owners[port] = application.Name
			}
		}
	}
	return owners, nil
}

func applicationPortKeys(manifest config.Manifest) []string {
	var ports []string
	for _, name := range sortedServiceNames(manifest) {
		service := manifest.Services[name]
		ports = append(ports, service.NodePorts()...)
		if service.TCP != nil && service.TCP.PublicPort != nil {
			ports = append(ports, fmt.Sprintf("tcp/%d", *service.TCP.PublicPort))
		}
	}
	return ports
}

// nodePortBlocker reports a node-local port, or an ingress TCP port that
// collides with one, that an earlier application already owns.
func nodePortBlocker(application string, manifest config.Manifest, owners map[string]string) *blockReason {
	for _, name := range sortedServiceNames(manifest) {
		service := manifest.Services[name]
		for _, port := range service.NodePorts() {
			if owner := owners[port]; owner != application {
				return &blockReason{reason: "node_port_conflict", message: fmt.Sprintf(
					"services.%s node port %s is already owned by application %q", name, port, owner)}
			}
		}
		if service.TCP != nil && service.TCP.PublicPort != nil {
			port := fmt.Sprintf("tcp/%d", *service.TCP.PublicPort)
			if owner := owners[port]; owner != application {
				return &blockReason{reason: "tcp_port_conflict", message: fmt.Sprintf(
					"services.%s.tcp public port %d is already owned by application %q", name, *service.TCP.PublicPort, owner)}
			}
		}
	}
	return nil
}

func sortedServiceNames(manifest config.Manifest) []string {
	names := make([]string, 0, len(manifest.Services))
	for name := range manifest.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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

// httpRouteDomains returns every hostname a service claims on ingress: the
// domains it serves and the domains that redirect to them.
func httpRouteDomains(http *config.HTTP) []string {
	domains := append([]string(nil), http.Domains...)
	for from := range http.Redirects {
		domains = append(domains, from)
	}
	sort.Strings(domains)
	return domains
}
