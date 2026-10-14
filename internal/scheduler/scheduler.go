// Package scheduler makes deterministic, side-effect-free workload placement
// decisions. Persistence, grace periods, rollout admission, and assignment
// publication remain controller responsibilities.
package scheduler

import (
	"fmt"
	"math/bits"
	"sort"
)

// Lifecycle is the durable administrative state of a node.
type Lifecycle string

const (
	LifecycleActive   Lifecycle = "ACTIVE"
	LifecycleCordoned Lifecycle = "CORDONED"
	LifecycleDraining Lifecycle = "DRAINING"
	LifecycleRemoved  Lifecycle = "REMOVED"
)

// Kind identifies the scheduling behavior of a workload.
type Kind string

const (
	KindService Kind = "service"
	KindCronJob Kind = "cron_job"
)

// Resources uses the same units as assignments and node capability reports.
type Resources struct {
	CPUMillis   uint64
	MemoryBytes uint64
}

// Node is the scheduler's immutable view of one node. Reserved contains all
// resource reservations which are not represented by Input.Workloads, such as
// authorized rollback allocations and admitted surge candidates.
//
// ServicePlacements uses ServiceKey(application, service) and similarly counts
// placements outside Input.Workloads that should influence replica spreading.
//
// UsedNodePorts lists node-local published ports, such as "udp/8790", held by
// placements outside Input.Workloads.
type Node struct {
	ID                string
	Name              string
	Lifecycle         Lifecycle
	Reachable         bool
	Ready             bool
	RuntimeCompatible bool
	NetworkCompatible bool
	Labels            map[string]string
	Capacity          Resources
	Reserved          Resources
	ServicePlacements map[string]uint32
	UsedNodePorts     []string
}

// Workload is one logical service replica or cron template. CurrentNodeID makes
// a feasible placement sticky. VolumeNodeID is an already-persisted local
// volume claim and is a hard placement constraint. NodePorts are node-local
// published ports such as "tcp/8787"; a node can hold each one only once.
type Workload struct {
	ID            string
	Kind          Kind
	Application   string
	Service       string
	Replica       uint32
	Resources     Resources
	Constraints   map[string]string
	CurrentNodeID string
	VolumeNodeID  string
	NodePorts     []string
}

type Input struct {
	Nodes     []Node
	Workloads []Workload
}

type Reason string

const (
	ReasonRetainedCurrent       Reason = "retained_current"
	ReasonLocalVolumePinned     Reason = "local_volume_pinned"
	ReasonOnlyFeasibleNode      Reason = "only_feasible_node"
	ReasonReplicaSpread         Reason = "replica_spread"
	ReasonResourceBalance       Reason = "resource_balance"
	ReasonDeterministicTieBreak Reason = "deterministic_tie_break"
	ReasonUnschedulable         Reason = "unschedulable"
)

type BlockerCode string

const (
	BlockerNoNodes              BlockerCode = "no_nodes"
	BlockerRemoved              BlockerCode = "node_removed"
	BlockerNotReachable         BlockerCode = "node_unreachable"
	BlockerNotReady             BlockerCode = "node_not_ready"
	BlockerRuntimeIncompatible  BlockerCode = "runtime_incompatible"
	BlockerNetworkIncompatible  BlockerCode = "network_incompatible"
	BlockerCordoned             BlockerCode = "node_cordoned"
	BlockerDraining             BlockerCode = "node_draining"
	BlockerConstraintMismatch   BlockerCode = "constraint_mismatch"
	BlockerLocalVolumePinned    BlockerCode = "local_volume_pinned"
	BlockerVolumeNodeLost       BlockerCode = "volume_node_lost"
	BlockerCronOwnerUnavailable BlockerCode = "cron_owner_unavailable"
	BlockerInsufficientCPU      BlockerCode = "insufficient_cpu"
	BlockerInsufficientMemory   BlockerCode = "insufficient_memory"
	BlockerNodePortInUse        BlockerCode = "node_port_in_use"
)

// Blocker is one stable explanation for rejecting a node. Node fields are
// empty only for a cluster-wide blocker such as no_nodes.
type Blocker struct {
	NodeID   string
	NodeName string
	Code     BlockerCode
	Detail   string
}

type Decision struct {
	WorkloadID string
	NodeID     string
	NodeName   string
	Reason     Reason
	Blockers   []Blocker
}

type nodeState struct {
	node     Node
	used     Resources
	services map[string]uint64
	ports    map[string]struct{}
}

type candidate struct {
	state  *nodeState
	spread uint64
	load   fraction
}

type fraction struct {
	numerator   uint64
	denominator uint64
}

// ServiceKey returns the canonical key used for replica-spread accounting.
func ServiceKey(application, service string) string {
	return application + "/" + service
}

// Schedule deterministically places every workload it can and returns one
// decision per input workload. Input order and map iteration order do not
// affect the result.
func Schedule(input Input) ([]Decision, error) {
	nodes, err := prepareNodes(input.Nodes)
	if err != nil {
		return nil, err
	}
	workloads := append([]Workload(nil), input.Workloads...)
	sort.Slice(workloads, func(i, j int) bool { return workloadLess(workloads[i], workloads[j]) })
	if err := validateWorkloads(workloads); err != nil {
		return nil, err
	}

	decisions := make(map[string]Decision, len(workloads))

	// Reserve all feasible current placements first. This makes retention take
	// precedence over the placement of a new replica, independent of workload
	// input order.
	for _, workload := range workloads {
		if workload.CurrentNodeID == "" {
			continue
		}
		state := nodes[workload.CurrentNodeID]
		if state == nil || len(feasibilityBlockers(state, workload, true)) != 0 {
			continue
		}
		place(state, workload)
		reason := ReasonRetainedCurrent
		if state.node.Lifecycle == LifecycleDraining && workload.VolumeNodeID == state.node.ID {
			reason = ReasonLocalVolumePinned
		}
		decisions[workload.ID] = placedDecision(workload.ID, state, reason)
	}

	orderedNodes := sortedNodeStates(nodes)
	for _, workload := range workloads {
		if _, retained := decisions[workload.ID]; retained {
			continue
		}
		// An unavailable cron owner cannot be fenced by observation alone. The
		// controller clears CurrentNodeID only after a graceful handoff or an
		// explicitly confirmed fenced removal; until then no replacement is safe.
		if workload.Kind == KindCronJob && workload.CurrentNodeID != "" {
			blockers := []Blocker{{
				Code:   BlockerCronOwnerUnavailable,
				Detail: fmt.Sprintf("cron owner %q must be safely handed off or confirmed fenced", workload.CurrentNodeID),
			}}
			if current := nodes[workload.CurrentNodeID]; current != nil {
				blockers = append(feasibilityBlockers(current, workload, true), blockers...)
			}
			decisions[workload.ID] = Decision{WorkloadID: workload.ID, Reason: ReasonUnschedulable, Blockers: blockers}
			continue
		}
		candidates := make([]candidate, 0, len(orderedNodes))
		blockers := make([]Blocker, 0)
		for _, state := range orderedNodes {
			reasons := feasibilityBlockers(state, workload, false)
			if len(reasons) != 0 {
				blockers = append(blockers, reasons...)
				continue
			}
			key := ServiceKey(workload.Application, workload.Service)
			post := addResources(state.used, workload.Resources)
			candidates = append(candidates, candidate{
				state: state, spread: state.services[key], load: dominantUtilization(post, state.node.Capacity),
			})
		}
		if len(candidates) == 0 {
			if len(orderedNodes) == 0 {
				blockers = []Blocker{{Code: BlockerNoNodes, Detail: "the cluster has no nodes"}}
			}
			if workload.VolumeNodeID != "" {
				claim := nodes[workload.VolumeNodeID]
				if claim == nil || claim.node.Lifecycle == LifecycleRemoved || !claim.node.Reachable {
					blockers = append(blockers, Blocker{Code: BlockerVolumeNodeLost, Detail: fmt.Sprintf("local volume node %q is unavailable", workload.VolumeNodeID)})
				}
			}
			decisions[workload.ID] = Decision{WorkloadID: workload.ID, Reason: ReasonUnschedulable, Blockers: blockers}
			continue
		}
		sort.Slice(candidates, func(i, j int) bool { return candidateLess(candidates[i], candidates[j]) })
		reason := selectedReason(candidates)
		winner := candidates[0].state
		place(winner, workload)
		decisions[workload.ID] = placedDecision(workload.ID, winner, reason)
	}

	result := make([]Decision, 0, len(workloads))
	for _, workload := range workloads {
		result = append(result, decisions[workload.ID])
	}
	return result, nil
}

func prepareNodes(input []Node) (map[string]*nodeState, error) {
	result := make(map[string]*nodeState, len(input))
	names := make(map[string]struct{}, len(input))
	for _, node := range input {
		if node.ID == "" || node.Name == "" {
			return nil, fmt.Errorf("scheduler: node ID and name are required")
		}
		if _, exists := result[node.ID]; exists {
			return nil, fmt.Errorf("scheduler: duplicate node ID %q", node.ID)
		}
		if _, exists := names[node.Name]; exists {
			return nil, fmt.Errorf("scheduler: duplicate node name %q", node.Name)
		}
		switch node.Lifecycle {
		case LifecycleActive, LifecycleCordoned, LifecycleDraining, LifecycleRemoved:
		default:
			return nil, fmt.Errorf("scheduler: node %q has invalid lifecycle %q", node.Name, node.Lifecycle)
		}
		services := make(map[string]uint64, len(node.ServicePlacements))
		for key, count := range node.ServicePlacements {
			services[key] = uint64(count)
		}
		ports := make(map[string]struct{}, len(node.UsedNodePorts))
		for _, port := range node.UsedNodePorts {
			ports[port] = struct{}{}
		}
		copy := node
		result[node.ID] = &nodeState{node: copy, used: node.Reserved, services: services, ports: ports}
		names[node.Name] = struct{}{}
	}
	return result, nil
}

func validateWorkloads(workloads []Workload) error {
	seen := make(map[string]struct{}, len(workloads))
	for _, workload := range workloads {
		if workload.ID == "" || workload.Application == "" || workload.Service == "" {
			return fmt.Errorf("scheduler: workload ID, application, and service are required")
		}
		if _, exists := seen[workload.ID]; exists {
			return fmt.Errorf("scheduler: duplicate workload ID %q", workload.ID)
		}
		seen[workload.ID] = struct{}{}
		switch workload.Kind {
		case KindService, KindCronJob:
		default:
			return fmt.Errorf("scheduler: workload %q has invalid kind %q", workload.ID, workload.Kind)
		}
		if workload.Resources.CPUMillis == 0 || workload.Resources.MemoryBytes == 0 {
			return fmt.Errorf("scheduler: workload %q must request positive CPU and memory", workload.ID)
		}
	}
	return nil
}

func sortedNodeStates(nodes map[string]*nodeState) []*nodeState {
	result := make([]*nodeState, 0, len(nodes))
	for _, state := range nodes {
		result = append(result, state)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].node.Name != result[j].node.Name {
			return result[i].node.Name < result[j].node.Name
		}
		return result[i].node.ID < result[j].node.ID
	})
	return result
}

func workloadLess(left, right Workload) bool {
	if left.Application != right.Application {
		return left.Application < right.Application
	}
	if left.Service != right.Service {
		return left.Service < right.Service
	}
	if left.Kind != right.Kind {
		return left.Kind < right.Kind
	}
	if left.Replica != right.Replica {
		return left.Replica < right.Replica
	}
	return left.ID < right.ID
}

func feasibilityBlockers(state *nodeState, workload Workload, retaining bool) []Blocker {
	node := state.node
	var result []Blocker
	add := func(code BlockerCode, detail string) {
		result = append(result, Blocker{NodeID: node.ID, NodeName: node.Name, Code: code, Detail: detail})
	}
	if node.Lifecycle == LifecycleRemoved {
		add(BlockerRemoved, "node has been removed")
	}
	if !node.Reachable {
		add(BlockerNotReachable, "node is beyond the scheduling reachability grace")
	}
	if !node.Ready {
		add(BlockerNotReady, "node has not reported ready")
	}
	if !node.RuntimeCompatible {
		add(BlockerRuntimeIncompatible, "node runtime is incompatible")
	}
	if !node.NetworkCompatible {
		add(BlockerNetworkIncompatible, "node network is incompatible")
	}
	switch node.Lifecycle {
	case LifecycleCordoned:
		if !retaining {
			add(BlockerCordoned, "node is cordoned and accepts no new placements")
		}
	case LifecycleDraining:
		// Stateless services evacuate from a draining node. Cron ownership stays
		// sticky until the controller has completed its safe handoff and clears
		// CurrentNodeID; claimed local-volume work can never evacuate.
		if !retaining || workload.Kind == KindService && workload.VolumeNodeID == "" {
			add(BlockerDraining, "node is draining and accepts no new placements")
		}
	}
	for _, key := range sortedMapKeys(workload.Constraints) {
		want := workload.Constraints[key]
		got, exists := node.Labels[key]
		if !exists {
			add(BlockerConstraintMismatch, fmt.Sprintf("requires label %s=%q, but the label is absent", key, want))
		} else if got != want {
			add(BlockerConstraintMismatch, fmt.Sprintf("requires label %s=%q, node has %q", key, want, got))
		}
	}
	if workload.VolumeNodeID != "" && workload.VolumeNodeID != node.ID {
		add(BlockerLocalVolumePinned, fmt.Sprintf("local volume is claimed by node %q", workload.VolumeNodeID))
	}
	for _, port := range workload.NodePorts {
		if _, used := state.ports[port]; used {
			add(BlockerNodePortInUse, fmt.Sprintf("node-local port %s is already published on this node", port))
		}
	}
	if !fits(state.used.CPUMillis, workload.Resources.CPUMillis, node.Capacity.CPUMillis) {
		add(BlockerInsufficientCPU, fmt.Sprintf("needs %dm CPU with %dm of %dm reserved", workload.Resources.CPUMillis, state.used.CPUMillis, node.Capacity.CPUMillis))
	}
	if !fits(state.used.MemoryBytes, workload.Resources.MemoryBytes, node.Capacity.MemoryBytes) {
		add(BlockerInsufficientMemory, fmt.Sprintf("needs %d bytes memory with %d of %d reserved", workload.Resources.MemoryBytes, state.used.MemoryBytes, node.Capacity.MemoryBytes))
	}
	return result
}

func fits(used, request, capacity uint64) bool {
	return used <= capacity && request <= capacity-used
}

func addResources(left, right Resources) Resources {
	return Resources{CPUMillis: left.CPUMillis + right.CPUMillis, MemoryBytes: left.MemoryBytes + right.MemoryBytes}
}

func place(state *nodeState, workload Workload) {
	state.used = addResources(state.used, workload.Resources)
	state.services[ServiceKey(workload.Application, workload.Service)]++
	for _, port := range workload.NodePorts {
		state.ports[port] = struct{}{}
	}
}

func placedDecision(workloadID string, state *nodeState, reason Reason) Decision {
	return Decision{WorkloadID: workloadID, NodeID: state.node.ID, NodeName: state.node.Name, Reason: reason}
}

func candidateLess(left, right candidate) bool {
	if left.spread != right.spread {
		return left.spread < right.spread
	}
	if comparison := compareFractions(left.load, right.load); comparison != 0 {
		return comparison < 0
	}
	if left.state.node.Name != right.state.node.Name {
		return left.state.node.Name < right.state.node.Name
	}
	return left.state.node.ID < right.state.node.ID
}

func selectedReason(candidates []candidate) Reason {
	if len(candidates) == 1 {
		return ReasonOnlyFeasibleNode
	}
	if candidates[0].spread != candidates[1].spread {
		return ReasonReplicaSpread
	}
	if compareFractions(candidates[0].load, candidates[1].load) != 0 {
		return ReasonResourceBalance
	}
	return ReasonDeterministicTieBreak
}

func dominantUtilization(used, capacity Resources) fraction {
	cpu := fraction{numerator: used.CPUMillis, denominator: capacity.CPUMillis}
	memory := fraction{numerator: used.MemoryBytes, denominator: capacity.MemoryBytes}
	if compareFractions(cpu, memory) >= 0 {
		return cpu
	}
	return memory
}

func compareFractions(left, right fraction) int {
	leftHigh, leftLow := bits.Mul64(left.numerator, right.denominator)
	rightHigh, rightLow := bits.Mul64(right.numerator, left.denominator)
	if leftHigh < rightHigh || leftHigh == rightHigh && leftLow < rightLow {
		return -1
	}
	if leftHigh > rightHigh || leftHigh == rightHigh && leftLow > rightLow {
		return 1
	}
	return 0
}

func sortedMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
