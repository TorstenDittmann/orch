package scheduler

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

func TestScheduleRetainsFeasibleCurrentPlacement(t *testing.T) {
	input := Input{
		Nodes: []Node{
			testNode("node-a", "a", 1000, 1024),
			testNode("node-b", "b", 4000, 4096),
		},
		Workloads: []Workload{testWorkload("web-0", "web", 0, 500, 512)},
	}
	input.Workloads[0].CurrentNodeID = "node-a"
	got := scheduleOK(t, input)
	if got[0].NodeID != "node-a" || got[0].Reason != ReasonRetainedCurrent {
		t.Fatalf("decision = %#v", got[0])
	}
}

func TestScheduleSpreadsThenBalancesReplicas(t *testing.T) {
	input := Input{
		Nodes: []Node{
			testNode("node-a", "a", 2000, 2000),
			testNode("node-b", "b", 2000, 2000),
			testNode("node-c", "c", 2000, 2000),
		},
		Workloads: []Workload{
			testWorkload("web-2", "web", 2, 100, 100),
			testWorkload("web-0", "web", 0, 100, 100),
			testWorkload("web-1", "web", 1, 100, 100),
			testWorkload("web-3", "web", 3, 100, 100),
		},
	}
	input.Nodes[0].Reserved = Resources{CPUMillis: 1000, MemoryBytes: 1000}
	got := scheduleOK(t, input)
	wantNodes := []string{"node-b", "node-c", "node-a", "node-b"}
	for index, decision := range got {
		if decision.NodeID != wantNodes[index] {
			t.Fatalf("decision %d = %#v, want node %s", index, decision, wantNodes[index])
		}
	}
	if got[0].Reason != ReasonDeterministicTieBreak || got[1].Reason != ReasonResourceBalance ||
		got[2].Reason != ReasonReplicaSpread || got[3].Reason != ReasonDeterministicTieBreak {
		t.Fatalf("reasons = %#v", got)
	}
}

func TestScheduleReservesAllCurrentPlacementsBeforeNewWork(t *testing.T) {
	nodeA := testNode("node-a", "a", 1000, 1000)
	nodeB := testNode("node-b", "b", 1000, 1000)
	current := testWorkload("z-current", "api", 0, 800, 800)
	current.CurrentNodeID = "node-a"
	newWork := testWorkload("a-new", "worker", 0, 400, 400)
	got := scheduleOK(t, Input{Nodes: []Node{nodeA, nodeB}, Workloads: []Workload{newWork, current}})
	byID := decisionsByID(got)
	if byID["z-current"].NodeID != "node-a" || byID["z-current"].Reason != ReasonRetainedCurrent {
		t.Fatalf("current = %#v", byID["z-current"])
	}
	if byID["a-new"].NodeID != "node-b" {
		t.Fatalf("new = %#v", byID["a-new"])
	}
}

func TestScheduleHonorsExactLabelsAndVolumeClaim(t *testing.T) {
	nodeA := testNode("node-a", "a", 1000, 1000)
	nodeA.Labels = map[string]string{"region": "eu", "storage": "fast"}
	nodeB := testNode("node-b", "b", 1000, 1000)
	nodeB.Labels = map[string]string{"region": "us", "storage": "fast"}
	workload := testWorkload("db-0", "db", 0, 100, 100)
	workload.Constraints = map[string]string{"region": "eu", "storage": "fast"}
	workload.VolumeNodeID = "node-a"
	got := scheduleOK(t, Input{Nodes: []Node{nodeB, nodeA}, Workloads: []Workload{workload}})
	if got[0].NodeID != "node-a" || got[0].Reason != ReasonOnlyFeasibleNode {
		t.Fatalf("decision = %#v", got[0])
	}
}

func TestScheduleCordonedNodeRetainsButTakesNoNewWork(t *testing.T) {
	node := testNode("node-a", "a", 1000, 1000)
	node.Lifecycle = LifecycleCordoned
	current := testWorkload("web-0", "web", 0, 100, 100)
	current.CurrentNodeID = node.ID
	newWork := testWorkload("api-0", "api", 0, 100, 100)
	got := decisionsByID(scheduleOK(t, Input{Nodes: []Node{node}, Workloads: []Workload{newWork, current}}))
	if got[current.ID].NodeID != node.ID || got[current.ID].Reason != ReasonRetainedCurrent {
		t.Fatalf("current = %#v", got[current.ID])
	}
	if got[newWork.ID].Reason != ReasonUnschedulable || !hasBlocker(got[newWork.ID], BlockerCordoned) {
		t.Fatalf("new = %#v", got[newWork.ID])
	}
}

func TestScheduleDrainingNodeEvacuatesStatelessService(t *testing.T) {
	draining := testNode("node-a", "a", 1000, 1000)
	draining.Lifecycle = LifecycleDraining
	active := testNode("node-b", "b", 1000, 1000)
	current := testWorkload("web-0", "web", 0, 100, 100)
	current.CurrentNodeID = draining.ID
	decision := scheduleOK(t, Input{Nodes: []Node{draining, active}, Workloads: []Workload{current}})[0]
	if decision.NodeID != active.ID || decision.Reason != ReasonOnlyFeasibleNode {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestScheduleDrainingNodeRetainsClaimedVolumeAndCronOwnership(t *testing.T) {
	draining := testNode("node-a", "a", 1000, 1000)
	draining.Lifecycle = LifecycleDraining
	active := testNode("node-b", "b", 1000, 1000)
	volume := testWorkload("db-0", "db", 0, 100, 100)
	volume.CurrentNodeID = draining.ID
	volume.VolumeNodeID = draining.ID
	cron := testWorkload("backup", "backup", 0, 100, 100)
	cron.Kind = KindCronJob
	cron.CurrentNodeID = draining.ID
	got := decisionsByID(scheduleOK(t, Input{Nodes: []Node{draining, active}, Workloads: []Workload{volume, cron}}))
	if got[volume.ID].NodeID != draining.ID || got[volume.ID].Reason != ReasonLocalVolumePinned {
		t.Fatalf("volume = %#v", got[volume.ID])
	}
	if got[cron.ID].NodeID != draining.ID || got[cron.ID].Reason != ReasonRetainedCurrent {
		t.Fatalf("cron = %#v", got[cron.ID])
	}
}

func TestScheduleDoesNotReplaceUnavailableCronOwnerWithoutFencing(t *testing.T) {
	owner := testNode("node-a", "a", 1000, 1000)
	owner.Reachable = false
	replacement := testNode("node-b", "b", 1000, 1000)
	cron := testWorkload("backup", "backup", 0, 100, 100)
	cron.Kind = KindCronJob
	cron.CurrentNodeID = owner.ID
	decision := scheduleOK(t, Input{Nodes: []Node{owner, replacement}, Workloads: []Workload{cron}})[0]
	if decision.NodeID != "" || decision.Reason != ReasonUnschedulable ||
		!hasBlocker(decision, BlockerNotReachable) || !hasBlocker(decision, BlockerCronOwnerUnavailable) {
		t.Fatalf("decision = %#v", decision)
	}

	// Clearing durable ownership after a confirmed fence permits reassignment.
	cron.CurrentNodeID = ""
	decision = scheduleOK(t, Input{Nodes: []Node{owner, replacement}, Workloads: []Workload{cron}})[0]
	if decision.NodeID != replacement.ID || decision.Reason != ReasonOnlyFeasibleNode {
		t.Fatalf("reassigned decision = %#v", decision)
	}
}

func TestScheduleReportsAllStableFeasibilityBlockers(t *testing.T) {
	node := testNode("node-a", "a", 50, 50)
	node.Lifecycle = LifecycleRemoved
	node.Reachable = false
	node.Ready = false
	node.RuntimeCompatible = false
	node.NetworkCompatible = false
	node.Labels = map[string]string{"region": "us"}
	workload := testWorkload("web-0", "web", 0, 100, 100)
	workload.Constraints = map[string]string{"region": "eu", "storage": "fast"}
	workload.VolumeNodeID = "node-b"
	decision := scheduleOK(t, Input{Nodes: []Node{node}, Workloads: []Workload{workload}})[0]
	want := []BlockerCode{
		BlockerRemoved, BlockerNotReachable, BlockerNotReady, BlockerRuntimeIncompatible,
		BlockerNetworkIncompatible, BlockerConstraintMismatch, BlockerConstraintMismatch,
		BlockerLocalVolumePinned, BlockerInsufficientCPU, BlockerInsufficientMemory, BlockerVolumeNodeLost,
	}
	got := make([]BlockerCode, len(decision.Blockers))
	for index, blocker := range decision.Blockers {
		got[index] = blocker.Code
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("blockers = %#v, want %#v", got, want)
	}
}

func TestScheduleReportsNoNodes(t *testing.T) {
	decision := scheduleOK(t, Input{Workloads: []Workload{testWorkload("web-0", "web", 0, 100, 100)}})[0]
	if decision.Reason != ReasonUnschedulable || !hasBlocker(decision, BlockerNoNodes) {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestScheduleDeterministicTieBreakUsesNameThenID(t *testing.T) {
	nodeA := testNode("id-z", "a", 1000, 1000)
	nodeB := testNode("id-a", "b", 1000, 1000)
	decision := scheduleOK(t, Input{Nodes: []Node{nodeB, nodeA}, Workloads: []Workload{testWorkload("web-0", "web", 0, 100, 100)}})[0]
	if decision.NodeID != "id-z" || decision.Reason != ReasonDeterministicTieBreak {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestScheduleRejectsMalformedInput(t *testing.T) {
	tests := map[string]Input{
		"duplicate node":     {Nodes: []Node{testNode("node-a", "a", 1, 1), testNode("node-a", "b", 1, 1)}},
		"invalid lifecycle":  {Nodes: []Node{{ID: "node-a", Name: "a", Lifecycle: "BROKEN"}}},
		"duplicate workload": {Workloads: []Workload{testWorkload("same", "web", 0, 1, 1), testWorkload("same", "web", 1, 1, 1)}},
		"zero request":       {Workloads: []Workload{{ID: "web-0", Kind: KindService, Application: "app", Service: "web"}}},
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Schedule(input); err == nil {
				t.Fatal("Schedule() error = nil")
			}
		})
	}
}

func TestScheduleRandomizedInvariantsAndInputOrderIndependence(t *testing.T) {
	random := rand.New(rand.NewSource(20260904))
	for iteration := 0; iteration < 500; iteration++ {
		input := randomInput(random)
		first := scheduleOK(t, input)
		shuffled := cloneInput(input)
		random.Shuffle(len(shuffled.Nodes), func(i, j int) { shuffled.Nodes[i], shuffled.Nodes[j] = shuffled.Nodes[j], shuffled.Nodes[i] })
		random.Shuffle(len(shuffled.Workloads), func(i, j int) {
			shuffled.Workloads[i], shuffled.Workloads[j] = shuffled.Workloads[j], shuffled.Workloads[i]
		})
		second := scheduleOK(t, shuffled)
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("iteration %d changed with input order:\nfirst=%#v\nsecond=%#v", iteration, first, second)
		}
		assertInvariants(t, input, first)
	}
}

func assertInvariants(t *testing.T, input Input, decisions []Decision) {
	t.Helper()
	nodes := make(map[string]Node, len(input.Nodes))
	used := make(map[string]Resources, len(input.Nodes))
	for _, node := range input.Nodes {
		nodes[node.ID] = node
		used[node.ID] = node.Reserved
	}
	workloads := make(map[string]Workload, len(input.Workloads))
	for _, workload := range input.Workloads {
		workloads[workload.ID] = workload
	}
	for _, decision := range decisions {
		if decision.NodeID == "" {
			continue
		}
		node, exists := nodes[decision.NodeID]
		if !exists {
			t.Fatalf("decision references unknown node: %#v", decision)
		}
		workload := workloads[decision.WorkloadID]
		if node.Lifecycle == LifecycleRemoved || !node.Reachable || !node.Ready || !node.RuntimeCompatible || !node.NetworkCompatible {
			t.Fatalf("workload placed on infeasible node: %#v / %#v", workload, node)
		}
		if decision.Reason != ReasonRetainedCurrent && decision.Reason != ReasonLocalVolumePinned && node.Lifecycle != LifecycleActive {
			t.Fatalf("new workload placed on non-active node: %#v / %#v", decision, node)
		}
		for key, want := range workload.Constraints {
			if node.Labels[key] != want {
				t.Fatalf("constraint violated: %#v / %#v", workload, node)
			}
		}
		if workload.VolumeNodeID != "" && workload.VolumeNodeID != node.ID {
			t.Fatalf("volume claim violated: %#v / %#v", workload, node)
		}
		resources := used[node.ID]
		resources = addResources(resources, workload.Resources)
		used[node.ID] = resources
		if resources.CPUMillis > node.Capacity.CPUMillis || resources.MemoryBytes > node.Capacity.MemoryBytes {
			t.Fatalf("capacity exceeded on %s: %#v > %#v", node.ID, resources, node.Capacity)
		}
	}
}

func randomInput(random *rand.Rand) Input {
	nodeCount := 1 + random.Intn(8)
	nodes := make([]Node, nodeCount)
	for index := range nodes {
		cpu := uint64(500 + random.Intn(3501))
		memory := uint64(512 + random.Intn(3585))
		nodes[index] = testNode(nodeID(index), nodeID(index), cpu, memory)
		nodes[index].Labels = map[string]string{"region": []string{"eu", "us"}[random.Intn(2)]}
		if random.Intn(10) == 0 {
			nodes[index].Lifecycle = LifecycleCordoned
		}
		if random.Intn(12) == 0 {
			nodes[index].Reachable = false
		}
	}
	workloadCount := 1 + random.Intn(30)
	workloads := make([]Workload, workloadCount)
	for index := range workloads {
		service := "service-" + string(rune('a'+random.Intn(4)))
		workloads[index] = testWorkload(workloadID(index), service, uint32(index), uint64(50+random.Intn(451)), uint64(64+random.Intn(449)))
		if random.Intn(3) == 0 {
			workloads[index].Constraints = map[string]string{"region": []string{"eu", "us"}[random.Intn(2)]}
		}
		if random.Intn(3) == 0 {
			workloads[index].CurrentNodeID = nodes[random.Intn(len(nodes))].ID
		}
		if random.Intn(12) == 0 {
			workloads[index].VolumeNodeID = nodes[random.Intn(len(nodes))].ID
		}
	}
	return Input{Nodes: nodes, Workloads: workloads}
}

func cloneInput(input Input) Input {
	result := Input{Nodes: append([]Node(nil), input.Nodes...), Workloads: append([]Workload(nil), input.Workloads...)}
	return result
}

func nodeID(index int) string { return "node-" + string(rune('a'+index)) }

func workloadID(index int) string {
	return fmt.Sprintf("workload-%03d", index)
}

func testNode(id, name string, cpu, memory uint64) Node {
	return Node{
		ID: id, Name: name, Lifecycle: LifecycleActive, Reachable: true, Ready: true,
		RuntimeCompatible: true, NetworkCompatible: true,
		Capacity: Resources{CPUMillis: cpu, MemoryBytes: memory},
	}
}

func testWorkload(id, service string, replica uint32, cpu, memory uint64) Workload {
	return Workload{
		ID: id, Kind: KindService, Application: "app", Service: service, Replica: replica,
		Resources: Resources{CPUMillis: cpu, MemoryBytes: memory},
	}
}

func scheduleOK(t *testing.T, input Input) []Decision {
	t.Helper()
	got, err := Schedule(input)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func decisionsByID(decisions []Decision) map[string]Decision {
	result := make(map[string]Decision, len(decisions))
	for _, decision := range decisions {
		result[decision.WorkloadID] = decision
	}
	return result
}

func hasBlocker(decision Decision, code BlockerCode) bool {
	for _, blocker := range decision.Blockers {
		if blocker.Code == code {
			return true
		}
	}
	return false
}

func BenchmarkScheduleTwentyNodesTwoHundredAllocations(b *testing.B) {
	input := Input{Nodes: make([]Node, 20), Workloads: make([]Workload, 200)}
	for index := range input.Nodes {
		input.Nodes[index] = testNode(nodeID(index), nodeID(index), 64_000, 256<<30)
		input.Nodes[index].Labels = map[string]string{"region": []string{"eu", "us"}[index%2]}
	}
	for index := range input.Workloads {
		input.Workloads[index] = testWorkload(workloadID(index), "service-"+string(rune('a'+index%10)), uint32(index/10), 100, 64<<20)
		if index%3 == 0 {
			input.Workloads[index].Constraints = map[string]string{"region": "eu"}
		}
	}
	b.ResetTimer()
	for range b.N {
		if _, err := Schedule(input); err != nil {
			b.Fatal(err)
		}
	}
}

func TestScheduleTreatsNodePortsAsNodeResources(t *testing.T) {
	nodeA := testNode("node-a", "a", 4000, 4096)
	nodeB := testNode("node-b", "b", 4000, 4096)
	nodeC := testNode("node-c", "c", 4000, 4096)
	nodeC.UsedNodePorts = []string{"udp/8790"}
	workloads := make([]Workload, 0, 3)
	for replica := uint32(0); replica < 3; replica++ {
		workload := testWorkload(fmt.Sprintf("game-%d", replica), "game", replica, 100, 100)
		workload.NodePorts = []string{"tcp/8787", "udp/8790"}
		workloads = append(workloads, workload)
	}
	got := decisionsByID(scheduleOK(t, Input{Nodes: []Node{nodeA, nodeB, nodeC}, Workloads: workloads}))
	if got["game-0"].NodeID != "node-a" || got["game-1"].NodeID != "node-b" {
		t.Fatalf("decisions = %#v", got)
	}
	blocked := got["game-2"]
	if blocked.NodeID != "" || blocked.Reason != ReasonUnschedulable {
		t.Fatalf("third replica = %#v", blocked)
	}
	for _, nodeID := range []string{"node-a", "node-b", "node-c"} {
		found := false
		for _, blocker := range blocked.Blockers {
			found = found || blocker.NodeID == nodeID && blocker.Code == BlockerNodePortInUse
		}
		if !found {
			t.Fatalf("missing node_port_in_use blocker for %s: %#v", nodeID, blocked.Blockers)
		}
	}
	other := testWorkload("other", "other", 0, 100, 100)
	other.NodePorts = []string{"udp/8787"}
	if decision := scheduleOK(t, Input{Nodes: []Node{nodeA}, Workloads: append(workloads[:1:1], other)}); decision[1].NodeID != "node-a" {
		t.Fatalf("different protocol on the same port was rejected: %#v", decision)
	}
}
