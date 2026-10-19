package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/scheduler"
	"github.com/TorstenDittmann/orch/internal/store"
)

const distributedLossGrace = 60 * time.Second

func applicationsUseConstraints(applications []model.Application) bool {
	for _, application := range applications {
		var manifest config.Manifest
		if json.Unmarshal(application.CanonicalManifest, &manifest) != nil {
			continue
		}
		for _, service := range manifest.Services {
			// Node-local ports are node resources only the scheduler counts.
			if len(service.Constraints) != 0 || len(service.NodePorts()) != 0 {
				return true
			}
		}
		for _, job := range manifest.CronJobs {
			if len(job.Constraints) != 0 {
				return true
			}
		}
	}
	return false
}

type distributedAllocation struct {
	allocation   model.AllocationDesired
	constraints  map[string]string
	volumeNames  []string
	volumeNodeID string
	nodePorts    []string
}

type distributedCron struct {
	job         model.CronJobDesired
	constraints map[string]string
	volumeNode  string
}

func (r *Reconciler) reconcileDistributed(ctx context.Context, cluster model.Cluster, applications []model.Application, nodes []model.Node, now time.Time) error {
	for index := range applications {
		if applications[index].DeletedAt != nil {
			continue
		}
		observations, err := r.state.ApplicationAllocationObservations(ctx, applications[index].Name)
		if err != nil {
			return fmt.Errorf("read rollout observations for application %q: %w", applications[index].Name, err)
		}
		applications[index].Execution.Allocations = observations
	}
	networks := make(map[string]model.NetworkConfig, len(nodes))
	for _, node := range nodes {
		configuration, err := model.NodeNetwork(cluster.WorkloadCIDR, node.Ordinal, cluster.NetworkGeneration)
		if err != nil {
			return fmt.Errorf("derive network for node %q: %w", node.Name, err)
		}
		networks[node.ID] = configuration
	}
	endpoints, err := r.state.ReadyServiceEndpoints(ctx)
	if err != nil {
		return err
	}
	if err := enrichDistributedNetworks(nodes, networks, nil); err != nil {
		return err
	}

	nodeByID := make(map[string]model.Node, len(nodes))
	for _, node := range nodes {
		nodeByID[node.ID] = node
	}
	currentByNode := make(map[string]model.Assignment, len(nodes))
	currentNodeByLogical := make(map[string]string)
	currentNodesByLogical := make(map[string][]string)
	currentAllocationByLogical := make(map[string]model.AllocationDesired)
	previousByBaseID := make(map[string]model.AllocationDesired)
	currentCronNode := make(map[string]string)
	previousCronByLogical := make(map[string]model.CronJobDesired)
	for _, node := range nodes {
		assignment, err := r.state.CurrentAssignment(ctx, node.ID)
		if errors.Is(err, store.ErrAssignmentNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read current assignment for node %q: %w", node.Name, err)
		}
		currentByNode[node.ID] = assignment
		for _, allocation := range assignment.Allocations {
			logical := allocationLogicalKey(allocation.Application, allocation.Service, allocation.Replica)
			currentNodesByLogical[logical] = append(currentNodesByLogical[logical], node.ID)
			if preferCurrentNode(nodeByID, currentNodeByLogical[logical], node.ID) {
				currentNodeByLogical[logical] = node.ID
				currentAllocationByLogical[logical] = allocation
			}
			baseID := allocationID(cluster.ID, allocation.Application, allocation.Service, allocation.Replica)
			copy := allocation
			copy.ID = baseID
			if previousByBaseID[baseID].ID == "" || currentNodeByLogical[logical] == node.ID {
				previousByBaseID[baseID] = copy
			}
		}
		for _, job := range assignment.CronJobs {
			logical := cronLogicalKey(job.Application, job.Job)
			if preferCurrentNode(nodeByID, currentCronNode[logical], node.ID) {
				currentCronNode[logical] = node.ID
				previousCronByLogical[logical] = job
			}
		}
	}

	planningNode, ok := r.firstPlanningNode(nodes, networks, now)
	if !ok {
		states := make(map[string]model.ApplicationExecution)
		retainedAllocations := make(map[string][]model.AllocationDesired, len(currentByNode))
		retainedCron := make(map[string][]model.CronJobDesired, len(currentByNode))
		for _, application := range applications {
			if application.DeletedAt == nil {
				states[application.Name] = executionState(model.ExecutionPending, application.Generation,
					"node_unavailable", "no connected compatible node has a ready workload network", now)
			}
		}
		for nodeID, assignment := range currentByNode {
			retainedAllocations[nodeID] = append([]model.AllocationDesired(nil), assignment.Allocations...)
			retainedCron[nodeID] = append([]model.CronJobDesired(nil), assignment.CronJobs...)
		}
		// Still publish network-only assignments so newly enrolled nodes can
		// perform preflight and report READY, while keeping every previously
		// authorized runtime object until a safe placement decision is possible.
		return r.publishDistributed(ctx, nodes, networks, currentByNode, retainedAllocations, retainedCron, states, now)
	}

	domainOwners, err := desiredDomainOwners(applications)
	if err != nil {
		return err
	}
	portOwners, err := desiredTCPPortOwners(applications)
	if err != nil {
		return err
	}
	nodePortOwners, err := desiredNodePortOwners(applications)
	if err != nil {
		return err
	}
	plannedAllocations := make([]distributedAllocation, 0)
	plannedCron := make([]distributedCron, 0)
	preservedAllocations := make(map[string][]model.AllocationDesired)
	preservedCron := make(map[string][]model.CronJobDesired)
	states := make(map[string]model.ApplicationExecution, len(applications))
	for _, application := range applications {
		if application.DeletedAt != nil {
			if application.Execution.Status != model.ExecutionRemoved {
				states[application.Name] = executionState(model.ExecutionStopping, 0,
					"removal_pending", "waiting for every assigned agent to remove runtime objects", now)
			}
			continue
		}
		var manifest config.Manifest
		if err := json.Unmarshal(application.CanonicalManifest, &manifest); err != nil {
			blocked := invalidIntentBlock(fmt.Errorf("decode stored manifest: %w", err))
			preserveDistributedApplication(application.Name, currentByNode, preservedAllocations, preservedCron)
			states[application.Name] = executionState(blockedStatus(blocked), application.Generation,
				blocked.reason, blocked.message, now)
			continue
		}
		gate, err := r.hookGate(ctx, application, manifest, now)
		if err != nil {
			return err
		}
		if gate != nil {
			preserveDistributedApplication(application.Name, currentByNode, preservedAllocations, preservedCron)
			states[application.Name] = *gate
			continue
		}
		resolved, secretBlocked, err := r.resolveApplicationIntent(ctx, application)
		if err != nil {
			return fmt.Errorf("resolve application %q secrets: %w", application.Name, err)
		}
		if secretBlocked != nil {
			if secretBlocked.preservePrevious {
				preserveDistributedApplication(application.Name, currentByNode, preservedAllocations, preservedCron)
			}
			states[application.Name] = executionState(model.ExecutionBlocked, application.Generation,
				secretBlocked.reason, secretBlocked.message, now)
			continue
		}
		if blocked := nodePortBlocker(application.Name, manifest, nodePortOwners); blocked != nil {
			states[application.Name] = executionState(model.ExecutionBlocked, application.Generation,
				blocked.reason, blocked.message, now)
			continue
		}
		bundle, blocked, err := planApplicationBundleForNetwork(cluster.ID, planningNode, application,
			networks[planningNode.ID], cluster.NetworkFrozenAt != nil, domainOwners, portOwners, previousByBaseID, resolved)
		resolved.wipe()
		if err != nil {
			blocked = invalidIntentBlock(err)
		}
		if blocked != nil {
			if blocked.preservePrevious {
				preserveDistributedApplication(application.Name, currentByNode, preservedAllocations, preservedCron)
			}
			states[application.Name] = executionState(blockedStatus(blocked), application.Generation,
				blocked.reason, blocked.message, now)
			continue
		}
		bundle.allocations = gateDistributedRollout(application, bundle.allocations, previousByBaseID)
		applicationAllocations := make([]distributedAllocation, 0, len(bundle.allocations))
		volumeConflict := false
		for _, allocation := range bundle.allocations {
			service := manifest.Services[allocation.Service]
			volumeNames := make([]string, 0, len(service.Mounts))
			volumeNode := ""
			for _, mount := range service.Mounts {
				volumeNames = append(volumeNames, mount.Source)
				claim, err := r.state.VolumeClaim(ctx, application.Name, mount.Source)
				if err != nil {
					return err
				}
				if claim != "" && volumeNode != "" && claim != volumeNode {
					volumeConflict = true
				}
				if claim != "" {
					volumeNode = claim
				}
			}
			applicationAllocations = append(applicationAllocations, distributedAllocation{
				allocation: allocation, constraints: service.Constraints, volumeNames: volumeNames, volumeNodeID: volumeNode,
				nodePorts: allocationNodePorts(allocation),
			})
		}
		if volumeConflict {
			preserveDistributedApplication(application.Name, currentByNode, preservedAllocations, preservedCron)
			states[application.Name] = executionState(model.ExecutionBlocked, application.Generation,
				"local_volume_conflict", "local volumes of this application are claimed by different nodes", now)
			continue
		}
		plannedAllocations = append(plannedAllocations, applicationAllocations...)
		for _, job := range bundle.cronJobs {
			plannedCron = append(plannedCron, distributedCron{job: job, constraints: manifest.CronJobs[job.Job].Constraints})
		}
		// Reports preserve controller-side blockers. Reopen execution derivation
		// after successful planning; unresolved placement blockers below override
		// this state again, including when the assignment payload is unchanged.
		if application.Execution.ObservedGeneration != application.Generation ||
			application.Execution.Status == model.ExecutionBlocked ||
			strings.HasPrefix(application.Execution.Reason, "pre_deploy_") {
			states[application.Name] = executionState(model.ExecutionPending, application.Generation,
				"placement_pending", "desired state is waiting for durable multi-node assignment receipt", now)
		}
	}
	if err := r.appendOneOffAssignments(ctx, preservedCron); err != nil {
		return err
	}
	cronOwnerships, err := r.state.CronOwnerships(ctx)
	if err != nil {
		return err
	}
	cronOwnershipByLogical := make(map[string][]store.CronOwnership)
	for _, ownership := range cronOwnerships {
		logical := cronLogicalKey(ownership.Application, ownership.Job)
		cronOwnershipByLogical[logical] = append(cronOwnershipByLogical[logical], ownership)
	}
	readyCron := plannedCron[:0]
	for _, item := range plannedCron {
		logical := cronLogicalKey(item.job.Application, item.job.Job)
		owner := currentCronNode[logical]
		observations := cronOwnershipByLogical[logical]
		if owner == "" {
			// The observation deliberately outlives the assignment that removed
			// the job. Do not grant a new lease until the former owner reports
			// against that empty replacement assignment.
			if len(observations) != 0 {
				states[item.job.Application] = executionState(model.ExecutionPending, item.job.ApplicationGeneration,
					"cron_handoff_releasing", "waiting for the previous cron owner to acknowledge release", now)
				continue
			}
			readyCron = append(readyCron, item)
			continue
		}
		ownerNode := nodeByID[owner]
		if ownerNode.Lifecycle != model.NodeLifecycleDraining {
			readyCron = append(readyCron, item)
			continue
		}
		previous := previousCronByLogical[logical]
		connected := r.sessions == nil || r.sessions.Connected(owner)
		if len(observations) != 1 || !cronReleaseReady(ownerNode, currentByNode[owner], observations[0], previous, connected) {
			if previous.Job != "" {
				preservedCron[owner] = append(preservedCron[owner], previous)
			}
			states[item.job.Application] = executionState(model.ExecutionPending, item.job.ApplicationGeneration,
				"cron_handoff_pending", "waiting for the draining cron owner to become idle and acknowledge its assignment", now)
			continue
		}
		states[item.job.Application] = executionState(model.ExecutionPending, item.job.ApplicationGeneration,
			"cron_handoff_releasing", "waiting for the draining cron owner to acknowledge release", now)
	}
	plannedCron = readyCron

	// A cron job that reads local volumes runs where the owning service's
	// durable claim put them. Until that service has been placed there is no
	// node to choose, so the job waits rather than claiming a volume itself.
	pinnedCron := plannedCron[:0]
	for _, item := range plannedCron {
		volumeNode, claimed := "", true
		for _, mount := range item.job.Mounts {
			claim, err := r.state.VolumeClaim(ctx, item.job.Application, mount.Name)
			if err != nil {
				return err
			}
			if claim == "" || volumeNode != "" && claim != volumeNode {
				claimed = false
				break
			}
			volumeNode = claim
		}
		if !claimed {
			if owner := currentCronNode[cronLogicalKey(item.job.Application, item.job.Job)]; owner != "" {
				if previous := previousCronByLogical[cronLogicalKey(item.job.Application, item.job.Job)]; previous.Job != "" {
					preservedCron[owner] = append(preservedCron[owner], previous)
				}
			}
			states[item.job.Application] = executionState(model.ExecutionPending, item.job.ApplicationGeneration,
				"cron_volume_pending", "waiting for the service that owns the cron job's volumes to be placed", now)
			continue
		}
		item.volumeNode = volumeNode
		pinnedCron = append(pinnedCron, item)
	}
	plannedCron = pinnedCron

	schedulerNodes := make([]scheduler.Node, 0, len(nodes))
	for _, node := range nodes {
		labels := cloneEnvironment(node.Labels)
		if labels == nil {
			labels = make(map[string]string)
		}
		labels["orch.os"] = node.OperatingSystem
		labels["orch.arch"] = node.Architecture
		labels["orch.node"] = node.Name
		cpu, memory := node.AllocatableCPUMillis, node.AllocatableMemoryBytes
		if cpu == 0 {
			cpu = node.TotalCPUMillis
		}
		if memory == 0 {
			memory = node.TotalMemoryBytes
		}
		// A pre-feature agent did not report capacity. Preserve the upgrade
		// path until its first workloads-4 hello replaces these sentinels.
		if cpu == 0 {
			cpu = 100_000
		}
		if memory == 0 {
			memory = 1 << 40
		}
		reachable := node.LastSeenAt != nil && now.Sub(*node.LastSeenAt) < distributedLossGrace
		connected := r.sessions == nil || r.sessions.Connected(node.ID)
		runtimeCompatible := node.OperatingSystem == "linux" && (node.Architecture == "amd64" || node.Architecture == "arm64") &&
			node.CgroupsV2 && node.RuntimeVersion == "containerd 2.3.4" && node.Snapshotter != ""
		networkCompatible := node.NetworkState == model.NetworkReady && node.DNSState == model.SubsystemReady &&
			node.NetworkGeneration == cluster.NetworkGeneration && node.NodeSubnet == networks[node.ID].NodeSubnet
		if len(nodes) > 1 {
			networkCompatible = networkCompatible && validMeshIdentity(node, networks[node.ID])
		}
		lifecycle := scheduler.Lifecycle(node.Lifecycle)
		if lifecycle == scheduler.LifecycleActive && reachable && !connected {
			lifecycle = scheduler.LifecycleCordoned
		}
		schedulerNodes = append(schedulerNodes, scheduler.Node{
			ID: node.ID, Name: node.Name, Lifecycle: lifecycle, Reachable: reachable,
			Ready: runtimeCompatible && networkCompatible, RuntimeCompatible: runtimeCompatible,
			NetworkCompatible: networkCompatible, Labels: labels,
			Capacity:          scheduler.Resources{CPUMillis: cpu, MemoryBytes: memory},
			Reserved:          assignmentResources(preservedAllocations[node.ID], preservedCron[node.ID]),
			ServicePlacements: servicePlacementCounts(preservedAllocations[node.ID]),
			UsedNodePorts:     usedNodePorts(preservedAllocations[node.ID]),
		})
	}

	workloads := make([]scheduler.Workload, 0, len(plannedAllocations)+len(plannedCron))
	allocationByLogical := make(map[string]distributedAllocation, len(plannedAllocations))
	for _, item := range plannedAllocations {
		logical := allocationLogicalKey(item.allocation.Application, item.allocation.Service, item.allocation.Replica)
		allocationByLogical[logical] = item
		workloads = append(workloads, scheduler.Workload{
			ID: logical, Kind: scheduler.KindService, Application: item.allocation.Application,
			Service: item.allocation.Service, Replica: item.allocation.Replica,
			Resources:   allocationReservation(item.allocation),
			Constraints: item.constraints, CurrentNodeID: currentNodeByLogical[logical], VolumeNodeID: item.volumeNodeID,
			NodePorts: item.nodePorts,
		})
	}
	cronByLogical := make(map[string]distributedCron, len(plannedCron))
	for _, item := range plannedCron {
		logical := cronLogicalKey(item.job.Application, item.job.Job)
		cronByLogical[logical] = item
		owner := currentCronNode[logical]
		workloads = append(workloads, scheduler.Workload{
			ID: logical, Kind: scheduler.KindCronJob, Application: item.job.Application, Service: item.job.Job, Replica: 1,
			Resources:   scheduler.Resources{CPUMillis: item.job.CPUMillis, MemoryBytes: item.job.MemoryBytes},
			Constraints: item.constraints, CurrentNodeID: owner, VolumeNodeID: item.volumeNode,
		})
	}
	decisions, err := scheduler.Schedule(scheduler.Input{Nodes: schedulerNodes, Workloads: workloads})
	if err != nil {
		return err
	}
	allocationsByNode := cloneAllocationMap(preservedAllocations)
	cronByNode := cloneCronMap(preservedCron)
	executionByApplication := make(map[string]model.ApplicationExecution, len(applications))
	for _, application := range applications {
		executionByApplication[application.Name] = application.Execution
	}
	surgesByApplication := make(map[string]uint32)
	unavailableByApplication := make(map[string]uint32)
	for logical, owners := range currentNodesByLogical {
		application := strings.SplitN(logical, "/", 2)[0]
		if len(owners) > 1 {
			surgesByApplication[application] += uint32(len(owners) - 1)
		}
		current := currentAllocationByLogical[logical]
		if current.ID != "" && !allocationExecutionReady(executionByApplication[application], current) {
			unavailableByApplication[application]++
		}
	}
	for _, decision := range decisions {
		if decision.NodeID == "" {
			application := strings.SplitN(decision.WorkloadID, "/", 2)[0]
			reason, message := decisionBlocker(decision)
			generation := states[application].ObservedGeneration
			if item, exists := allocationByLogical[decision.WorkloadID]; exists {
				generation = item.allocation.ApplicationGeneration
			}
			if item, exists := cronByLogical[decision.WorkloadID]; exists {
				generation = item.job.ApplicationGeneration
			}
			states[application] = executionState(model.ExecutionBlocked, generation, reason, message, now)
			if item, exists := allocationByLogical[decision.WorkloadID]; exists && len(item.volumeNames) != 0 {
				if owner := currentNodeByLogical[decision.WorkloadID]; owner != "" {
					allocation := currentAllocationByLogical[decision.WorkloadID]
					if allocation.ID == "" {
						continue
					}
					allocationsByNode[owner] = append(allocationsByNode[owner], allocation)
				}
			}
			if item, exists := cronByLogical[decision.WorkloadID]; exists {
				if owner := currentCronNode[decision.WorkloadID]; owner != "" {
					previous := previousCronByLogical[decision.WorkloadID]
					if previous.Job == "" {
						previous = item.job
					}
					cronByNode[owner] = append(cronByNode[owner], previous)
				}
			}
			continue
		}
		if item, exists := allocationByLogical[decision.WorkloadID]; exists {
			allocation := item.allocation
			sourceNodeID := currentNodeByLogical[decision.WorkloadID]
			cooperativeMove := sourceNodeID != "" && sourceNodeID != decision.NodeID &&
				nodeReachable(nodes, sourceNodeID, now) && (r.sessions == nil || r.sessions.Connected(sourceNodeID))
			if cooperativeMove && len(currentNodesByLogical[decision.WorkloadID]) == 1 {
				hold := false
				if allocation.MaxSurge > 0 {
					hold = surgesByApplication[allocation.Application] >= allocation.MaxSurge
					if !hold {
						surgesByApplication[allocation.Application]++
					}
				} else {
					hold = unavailableByApplication[allocation.Application] >= allocation.MaxUnavailable
					if !hold {
						unavailableByApplication[allocation.Application]++
					}
				}
				if hold {
					allocationsByNode[sourceNodeID] = append(allocationsByNode[sourceNodeID], currentAllocationByLogical[decision.WorkloadID])
					states[allocation.Application] = executionState(model.ExecutionPending, allocation.ApplicationGeneration,
						"placement_budget_wait", "waiting for an application-wide placement move budget", now)
					continue
				}
			}
			preferredID := ""
			if currentNodeByLogical[decision.WorkloadID] == decision.NodeID {
				preferredID = currentAllocationByLogical[decision.WorkloadID].ID
			} else if currentNodeByLogical[decision.WorkloadID] == "" && nodeByID[decision.NodeID].Ordinal == 0 {
				preferredID = allocation.ID
			}
			attemptID, err := r.state.EnsurePlacementAttempt(ctx, decision.WorkloadID, decision.NodeID,
				allocation.Application, allocation.Service, allocation.Replica, preferredID, string(decision.Reason), now)
			if err != nil {
				return err
			}
			allocation.ID = attemptID
			allocationsByNode[decision.NodeID] = append(allocationsByNode[decision.NodeID], allocation)
			for _, previousNodeID := range currentNodesByLogical[decision.WorkloadID] {
				cooperativeSource := nodeReachable(nodes, previousNodeID, now) &&
					(r.sessions == nil || r.sessions.Connected(previousNodeID))
				if previousNodeID == decision.NodeID || allocation.MaxSurge == 0 || !cooperativeSource ||
					drainReplacementReady(nodeByID[decision.NodeID], currentByNode[decision.NodeID], allocation,
						executionByApplication[allocation.Application]) {
					continue
				}
				previous, ok := currentAllocationForLogical(currentByNode[previousNodeID], decision.WorkloadID)
				if !ok {
					continue
				}
				allocationsByNode[previousNodeID] = append(allocationsByNode[previousNodeID], previous)
				states[allocation.Application] = executionState(model.ExecutionPending, allocation.ApplicationGeneration,
					"placement_replacement_pending", "waiting for the replacement allocation to be acknowledged and ready before stopping the previous placement", now)
			}
			if decision.Reason == scheduler.ReasonLocalVolumePinned {
				states[allocation.Application] = executionState(model.ExecutionBlocked, allocation.ApplicationGeneration,
					"local_volume_pinned", "a local volume keeps this allocation on the draining node", now)
			}
			for _, volume := range item.volumeNames {
				if err := r.state.ClaimVolume(ctx, allocation.Application, volume, decision.NodeID, now); err != nil {
					return err
				}
			}
			continue
		}
		if item, exists := cronByLogical[decision.WorkloadID]; exists {
			owner := currentCronNode[decision.WorkloadID]
			if owner != "" && owner != decision.NodeID {
				previous := previousCronByLogical[decision.WorkloadID]
				observations := cronOwnershipByLogical[decision.WorkloadID]
				connected := r.sessions == nil || r.sessions.Connected(owner)
				if len(observations) != 1 || !cronReleaseReady(nodeByID[owner], currentByNode[owner], observations[0], previous, connected) {
					if previous.Job != "" {
						cronByNode[owner] = append(cronByNode[owner], previous)
					}
					states[item.job.Application] = executionState(model.ExecutionPending, item.job.ApplicationGeneration,
						"cron_handoff_pending", "waiting for the current cron owner to become idle and acknowledge its assignment", now)
				} else {
					states[item.job.Application] = executionState(model.ExecutionPending, item.job.ApplicationGeneration,
						"cron_handoff_releasing", "waiting for the current cron owner to acknowledge release", now)
				}
				continue
			}
			cronByNode[decision.NodeID] = append(cronByNode[decision.NodeID], item.job)
			if nodeByID[decision.NodeID].Lifecycle == model.NodeLifecycleDraining {
				states[item.job.Application] = executionState(model.ExecutionPending, item.job.ApplicationGeneration,
					"cron_handoff_pending", "waiting for the draining node to finish and release cron ownership", now)
			}
		}
	}
	currentEndpoints := filterPlacedEndpoints(endpoints, allocationsByNode, nodes, now)
	for nodeID, configuration := range networks {
		configuration.Endpoints = reachableEndpoints(configuration, nodeID, currentEndpoints)
		networks[nodeID] = configuration
	}
	if err := r.publishDistributed(ctx, nodes, networks, currentByNode, allocationsByNode, cronByNode, states, now); err != nil {
		return err
	}
	activeAttempts := make([]string, 0, len(plannedAllocations)+len(preservedAllocations))
	for _, allocations := range allocationsByNode {
		for _, allocation := range allocations {
			activeAttempts = append(activeAttempts, allocation.ID)
		}
	}
	return r.state.RetireInactivePlacementAttempts(ctx, activeAttempts, now)
}

func reachableEndpoints(configuration model.NetworkConfig, localNodeID string, endpoints []model.ServiceEndpoint) []model.ServiceEndpoint {
	allowed := map[string]bool{localNodeID: true}
	for _, peer := range configuration.Peers {
		allowed[peer.NodeID] = true
	}
	result := make([]model.ServiceEndpoint, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if allowed[endpoint.NodeID] {
			result = append(result, endpoint)
		}
	}
	return result
}

func allocationReservation(allocation model.AllocationDesired) scheduler.Resources {
	result := scheduler.Resources{CPUMillis: allocation.CPUMillis, MemoryBytes: allocation.MemoryBytes}
	if allocation.StartBeforeStop && allocation.Rollback != nil {
		result.CPUMillis += allocation.Rollback.CPUMillis
		result.MemoryBytes += allocation.Rollback.MemoryBytes
	}
	return result
}

func preferCurrentNode(nodes map[string]model.Node, selected, candidate string) bool {
	if selected == "" {
		return true
	}
	return nodes[selected].Lifecycle == model.NodeLifecycleDraining &&
		nodes[candidate].Lifecycle != model.NodeLifecycleDraining
}

func currentAllocationForLogical(assignment model.Assignment, logical string) (model.AllocationDesired, bool) {
	for _, allocation := range assignment.Allocations {
		if allocationLogicalKey(allocation.Application, allocation.Service, allocation.Replica) == logical {
			return allocation, true
		}
	}
	return model.AllocationDesired{}, false
}

func drainReplacementReady(node model.Node, assignment model.Assignment, desired model.AllocationDesired,
	execution model.ApplicationExecution,
) bool {
	if assignment.Generation == 0 || node.AcknowledgedGeneration != assignment.Generation {
		return false
	}
	assigned := false
	for _, allocation := range assignment.Allocations {
		if allocation.ID == desired.ID && allocation.ApplicationGeneration == desired.ApplicationGeneration &&
			allocation.RevisionID == desired.RevisionID && allocation.SpecDigest == desired.SpecDigest {
			assigned = true
			break
		}
	}
	if !assigned {
		return false
	}
	for _, status := range execution.Allocations {
		if status.ID == desired.ID && status.Phase == model.AllocationRunning &&
			status.ActiveGeneration == desired.ApplicationGeneration && status.ActiveRevisionID == desired.RevisionID &&
			status.ActiveSpecDigest == desired.SpecDigest &&
			(status.HealthStatus == model.HealthReady || status.HealthStatus == model.HealthNotConfigured) {
			return true
		}
	}
	return false
}

func allocationExecutionReady(execution model.ApplicationExecution, desired model.AllocationDesired) bool {
	for _, status := range execution.Allocations {
		if status.ID == desired.ID && status.Phase == model.AllocationRunning &&
			status.ActiveGeneration == desired.ApplicationGeneration && status.ActiveRevisionID == desired.RevisionID &&
			status.ActiveSpecDigest == desired.SpecDigest &&
			(status.HealthStatus == model.HealthReady || status.HealthStatus == model.HealthNotConfigured) {
			return true
		}
	}
	return false
}

func cronReleaseReady(node model.Node, assignment model.Assignment, ownership store.CronOwnership,
	previous model.CronJobDesired, connected bool,
) bool {
	if !connected || previous.Job == "" || assignment.Generation == 0 || node.AcknowledgedGeneration != assignment.Generation ||
		ownership.NodeID != node.ID || ownership.Application != previous.Application || ownership.Job != previous.Job ||
		ownership.SpecDigest != previous.SpecDigest || ownership.ActiveRunID != "" {
		return false
	}
	for _, job := range assignment.CronJobs {
		if job.Application == previous.Application && job.Job == previous.Job && job.SpecDigest == previous.SpecDigest {
			return true
		}
	}
	return false
}

func preserveDistributedApplication(application string, current map[string]model.Assignment,
	allocations map[string][]model.AllocationDesired, cron map[string][]model.CronJobDesired,
) {
	for nodeID, assignment := range current {
		for _, allocation := range assignment.Allocations {
			if allocation.Application == application {
				allocations[nodeID] = append(allocations[nodeID], allocation)
			}
		}
		for _, job := range assignment.CronJobs {
			if job.Application == application && job.RunID == "" {
				cron[nodeID] = append(cron[nodeID], job)
			}
		}
	}
}

func assignmentResources(allocations []model.AllocationDesired, jobs []model.CronJobDesired) scheduler.Resources {
	var result scheduler.Resources
	for _, allocation := range allocations {
		reservation := allocationReservation(allocation)
		result.CPUMillis += reservation.CPUMillis
		result.MemoryBytes += reservation.MemoryBytes
	}
	for _, job := range jobs {
		result.CPUMillis += job.CPUMillis
		result.MemoryBytes += job.MemoryBytes
	}
	return result
}

func servicePlacementCounts(allocations []model.AllocationDesired) map[string]uint32 {
	result := make(map[string]uint32)
	for _, allocation := range allocations {
		result[scheduler.ServiceKey(allocation.Application, allocation.Service)]++
	}
	return result
}

func cloneAllocationMap(input map[string][]model.AllocationDesired) map[string][]model.AllocationDesired {
	result := make(map[string][]model.AllocationDesired, len(input))
	for nodeID, allocations := range input {
		result[nodeID] = append([]model.AllocationDesired(nil), allocations...)
	}
	return result
}

func cloneCronMap(input map[string][]model.CronJobDesired) map[string][]model.CronJobDesired {
	result := make(map[string][]model.CronJobDesired, len(input))
	for nodeID, jobs := range input {
		result[nodeID] = append([]model.CronJobDesired(nil), jobs...)
	}
	return result
}

// gateDistributedRollout admits at most one not-yet-ready replacement across
// the whole application. Agents can still use their local surge/rollback
// machinery, but never receive independent permission to advance every node
// at once.
func gateDistributedRollout(application model.Application, desired []model.AllocationDesired, previous map[string]model.AllocationDesired) []model.AllocationDesired {
	ready := make(map[string]bool, len(application.Execution.Allocations))
	for _, status := range application.Execution.Allocations {
		key := allocationLogicalKey(status.Application, status.Service, status.Replica)
		ready[key] = status.Phase == model.AllocationRunning && status.ActiveGeneration == application.Generation &&
			(status.HealthStatus == model.HealthReady || status.HealthStatus == model.HealthNotConfigured)
	}
	inProgress := false
	// The previous map describes current desired records. Detect an admitted
	// candidate independently from the old-generation branch above.
	for _, allocation := range desired {
		prior := previous[allocation.ID]
		if prior.ApplicationGeneration == application.Generation && prior.SpecDigest == allocation.SpecDigest &&
			!ready[allocationLogicalKey(allocation.Application, allocation.Service, allocation.Replica)] {
			inProgress = true
		}
	}
	admitted := inProgress
	result := append([]model.AllocationDesired(nil), desired...)
	for index, allocation := range result {
		prior := previous[allocation.ID]
		if prior.SpecDigest == "" || prior.ApplicationGeneration == application.Generation || ready[allocationLogicalKey(allocation.Application, allocation.Service, allocation.Replica)] {
			continue
		}
		if !admitted {
			admitted = true
			continue
		}
		result[index] = prior
	}
	return result
}

func (r *Reconciler) publishDistributed(ctx context.Context, nodes []model.Node, networks map[string]model.NetworkConfig,
	current map[string]model.Assignment, allocations map[string][]model.AllocationDesired,
	cron map[string][]model.CronJobDesired, states map[string]model.ApplicationExecution, now time.Time,
) error {
	if err := r.appendOneOffAssignments(ctx, cron); err != nil {
		return err
	}
	ordered := append([]model.Node(nil), nodes...)
	sort.Slice(ordered, func(i, j int) bool {
		leftRemoved := removedPlacementCount(current[ordered[i].ID], allocations[ordered[i].ID], cron[ordered[i].ID])
		rightRemoved := removedPlacementCount(current[ordered[j].ID], allocations[ordered[j].ID], cron[ordered[j].ID])
		if leftRemoved != rightRemoved {
			return leftRemoved > rightRemoved
		}
		return ordered[i].Ordinal < ordered[j].Ordinal
	})
	for _, node := range ordered {
		nodeAllocations := allocations[node.ID]
		nodeCron := cron[node.ID]
		sort.Slice(nodeAllocations, func(i, j int) bool { return nodeAllocations[i].ID < nodeAllocations[j].ID })
		sort.Slice(nodeCron, func(i, j int) bool {
			if nodeCron[i].Application == nodeCron[j].Application {
				return nodeCron[i].Job < nodeCron[j].Job
			}
			return nodeCron[i].Application < nodeCron[j].Application
		})
		payload, digest, err := model.AssignmentPayloadWithJobs(networks[node.ID], nodeAllocations, nodeCron)
		if err != nil {
			return err
		}
		assignment, changed, err := r.state.PublishAssignment(ctx, model.PublishAssignment{
			NodeID: node.ID, Digest: digest, Payload: payload, Network: networks[node.ID],
			Allocations: nodeAllocations, CronJobs: nodeCron, ApplicationStates: states, CreatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("publish assignment for node %q: %w", node.Name, err)
		}
		if changed && r.onPublished != nil {
			hydrated, err := r.HydrateAssignment(ctx, assignment)
			if err != nil {
				return fmt.Errorf("hydrate assignment for node %q: %w", node.Name, err)
			}
			r.onPublished(hydrated)
			wipeMaterials(hydrated.Materials)
		}
	}
	return nil
}

func (r *Reconciler) firstPlanningNode(nodes []model.Node, networks map[string]model.NetworkConfig, now time.Time) (model.Node, bool) {
	ordered := append([]model.Node(nil), nodes...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Ordinal < ordered[j].Ordinal })
	for _, node := range ordered {
		if node.Lifecycle == model.NodeLifecycleRemoved || node.LastSeenAt == nil || now.Sub(*node.LastSeenAt) >= distributedLossGrace {
			continue
		}
		if r.sessions != nil && !r.sessions.Connected(node.ID) {
			continue
		}
		if nodeBlocker(node) == nil && node.NetworkGeneration == networks[node.ID].Generation && node.NodeSubnet == networks[node.ID].NodeSubnet &&
			(len(nodes) == 1 || validMeshIdentity(node, networks[node.ID])) {
			return node, true
		}
	}
	return model.Node{}, false
}

func blockedStatus(blocked *blockReason) string {
	if blocked.status != "" {
		return blocked.status
	}
	return model.ExecutionBlocked
}

func allocationLogicalKey(application, service string, replica uint32) string {
	return fmt.Sprintf("%s/%s/%d", application, service, replica)
}

func cronLogicalKey(application, job string) string { return application + "/" + job }

func nodeReachable(nodes []model.Node, nodeID string, now time.Time) bool {
	for _, node := range nodes {
		if node.ID == nodeID {
			return node.LastSeenAt != nil && now.Sub(*node.LastSeenAt) < distributedLossGrace
		}
	}
	return false
}

func decisionBlocker(decision scheduler.Decision) (string, string) {
	if len(decision.Blockers) == 0 {
		return "unschedulable", "no node can satisfy this workload"
	}
	for _, preferred := range []scheduler.BlockerCode{
		scheduler.BlockerVolumeNodeLost, scheduler.BlockerLocalVolumePinned,
		scheduler.BlockerCronOwnerUnavailable, scheduler.BlockerNodePortInUse, scheduler.BlockerConstraintMismatch,
		scheduler.BlockerInsufficientCPU, scheduler.BlockerInsufficientMemory,
	} {
		for _, blocker := range decision.Blockers {
			if blocker.Code == preferred {
				return string(blocker.Code), blocker.Detail
			}
		}
	}
	return string(decision.Blockers[0].Code), decision.Blockers[0].Detail
}

func removedPlacementCount(current model.Assignment, desired []model.AllocationDesired, jobs []model.CronJobDesired) int {
	want := make(map[string]struct{}, len(desired)+len(jobs))
	for _, allocation := range desired {
		want[allocationLogicalKey(allocation.Application, allocation.Service, allocation.Replica)] = struct{}{}
	}
	for _, job := range jobs {
		want[cronLogicalKey(job.Application, job.Job)] = struct{}{}
	}
	removed := 0
	for _, allocation := range current.Allocations {
		if _, exists := want[allocationLogicalKey(allocation.Application, allocation.Service, allocation.Replica)]; !exists {
			removed++
		}
	}
	for _, job := range current.CronJobs {
		if _, exists := want[cronLogicalKey(job.Application, job.Job)]; !exists {
			removed++
		}
	}
	return removed
}

func enrichDistributedNetworks(nodes []model.Node, networks map[string]model.NetworkConfig, endpoints []model.ServiceEndpoint) error {
	meshReady := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		meshReady[node.ID] = validMeshIdentity(node, networks[node.ID])
	}
	for _, node := range nodes {
		configuration := networks[node.ID]
		configuration.IngressEnabled = node.Ingress
		configuration.Endpoints = append([]model.ServiceEndpoint(nil), endpoints...)
		if meshReady[node.ID] {
			configuration.WireGuardListenPort = uint16(node.WireGuardListenPort)
			for _, peer := range nodes {
				if peer.ID == node.ID || !meshReady[peer.ID] {
					continue
				}
				configuration.Peers = append(configuration.Peers, model.WireGuardPeer{
					NodeID: peer.ID, PublicKey: peer.WireGuardPublicKey,
					Endpoint:      net.JoinHostPort(peer.AdvertiseAddress, strconv.Itoa(int(peer.WireGuardListenPort))),
					AllowedSubnet: networks[peer.ID].NodeSubnet,
				})
			}
			wireguard := model.WireGuardConfig{
				Generation: configuration.Generation, ClusterCIDR: configuration.ClusterCIDR,
				NodeSubnet: configuration.NodeSubnet, ListenPort: configuration.WireGuardListenPort,
				Peers: configuration.Peers,
			}
			canonical, err := model.CanonicalWireGuardConfig(wireguard)
			if err != nil {
				return fmt.Errorf("build WireGuard mesh for node %q: %w", node.Name, err)
			}
			configuration.Peers = canonical.Peers
		}
		networks[node.ID] = configuration
	}
	return nil
}

func validMeshIdentity(node model.Node, network model.NetworkConfig) bool {
	return model.ValidateMeshIdentity(node.AdvertiseAddress, node.WireGuardPublicKey, node.WireGuardListenPort) == nil &&
		node.AdvertiseAddress != "" && model.ValidateWireGuardConfig(model.WireGuardConfig{
		Generation: network.Generation, ClusterCIDR: network.ClusterCIDR, NodeSubnet: network.NodeSubnet,
		ListenPort: uint16(node.WireGuardListenPort),
	}) == nil
}

func filterPlacedEndpoints(endpoints []model.ServiceEndpoint, allocations map[string][]model.AllocationDesired, nodes []model.Node, now time.Time) []model.ServiceEndpoint {
	current := make(map[string]string)
	for nodeID, desired := range allocations {
		for _, allocation := range desired {
			current[allocation.ID] = nodeID
		}
	}
	reachable := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		reachable[node.ID] = node.LastSeenAt != nil && now.Sub(*node.LastSeenAt) < distributedLossGrace
	}
	result := make([]model.ServiceEndpoint, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if current[endpoint.AllocationID] == endpoint.NodeID && reachable[endpoint.NodeID] {
			result = append(result, endpoint)
		}
	}
	return result
}

// allocationNodePorts returns the node-local ports an allocation publishes,
// including those of its authorized rollback revision, as scheduler keys.
func allocationNodePorts(allocation model.AllocationDesired) []string {
	seen := make(map[string]struct{})
	add := func(tcp *model.TCPDesired, udp *model.UDPDesired) {
		if tcp != nil && tcp.NodePort != 0 {
			seen[fmt.Sprintf("tcp/%d", tcp.NodePort)] = struct{}{}
		}
		if udp != nil {
			seen[fmt.Sprintf("udp/%d", udp.NodePort)] = struct{}{}
		}
	}
	add(allocation.TCP, allocation.UDP)
	if allocation.Rollback != nil {
		add(allocation.Rollback.TCP, allocation.Rollback.UDP)
	}
	result := make([]string, 0, len(seen))
	for port := range seen {
		result = append(result, port)
	}
	sort.Strings(result)
	return result
}

func usedNodePorts(allocations []model.AllocationDesired) []string {
	var result []string
	for _, allocation := range allocations {
		result = append(result, allocationNodePorts(allocation)...)
	}
	return result
}
