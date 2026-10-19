package control

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentctl "github.com/TorstenDittmann/orch/internal/agent"
	agentstate "github.com/TorstenDittmann/orch/internal/agent/state"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/discovery"
	"github.com/TorstenDittmann/orch/internal/health"
	"github.com/TorstenDittmann/orch/internal/ingress"
	"github.com/TorstenDittmann/orch/internal/model"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

// Exercise actual supervisor output against the controller's report validator:
// a process may run before readiness passes, without losing the agent stream.
func TestControllerAcceptsInitialSupervisorReadinessReport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state, reconciler, now := readyDistributedCluster(t, map[string]map[string]string{
		"node-one": {"web": "yes"}, "node-two": {},
	})
	manifest := newTestManifest("warming", map[string]config.Service{
		"web": {Image: executableImage, Constraints: map[string]string{"web": "yes"},
			HTTP: &config.HTTP{Port: 8080, Domains: []string{"app.example.com"}, HealthPath: "/health", HTTPS: true}},
	})
	applyManifest(t, state, manifest, "revision-one", "application-key-one", *now)
	*now = now.Add(time.Second)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assignment, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.AcknowledgeAssignment(ctx, "node-one", assignment.Generation, assignment.Digest); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	cache, err := agentstate.Open(ctx, filepath.Join(directory, "agent.db"), "cluster-one", "node-one")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	runtime := orchruntime.NewFake()
	runtime.Now = func() time.Time { return *now }
	supervisor := agentctl.NewSupervisorWithServices(cache, runtime, func() time.Time { return *now }, agentctl.SupervisorServices{
		Prober: &health.Fake{}, Discovery: discovery.NewFake(), Ingress: &ingress.Fake{},
	})

	for sequence := uint64(2); sequence <= 3; sequence++ {
		statuses, err := supervisor.Reconcile(ctx, assignment)
		if err != nil || len(statuses) != 1 {
			t.Fatalf("supervisor statuses = %#v, %v", statuses, err)
		}
		routes, network := supervisor.Observations()
		recorded, err := state.RecordAgentReport(ctx, model.AgentReport{
			NodeID: "node-one", BootID: "boot-node-one", Sequence: sequence,
			AssignmentGeneration: assignment.Generation, AssignmentDigest: assignment.Digest,
			Allocations: statuses, Routes: routes, Network: network, ReportedAt: *now,
		})
		if err != nil || !recorded {
			t.Fatalf("controller rejected supervisor report %d: recorded=%t error=%v", sequence, recorded, err)
		}
		application, err := state.Application(ctx, "warming")
		if err != nil {
			t.Fatal(err)
		}
		if sequence == 2 {
			if statuses[0].HealthStatus != model.HealthChecking || application.Execution.Status == model.ExecutionHealthy ||
				len(routes) != 1 || routes[0].State != model.RoutePending {
				t.Fatalf("warming service was prematurely published: status=%#v execution=%#v routes=%#v", statuses[0], application.Execution, routes)
			}
		} else if statuses[0].HealthStatus != model.HealthReady || application.Execution.Status != model.ExecutionHealthy {
			t.Fatalf("second successful probe did not activate service: status=%#v execution=%#v", statuses[0], application.Execution)
		}
		*now = now.Add(health.ProbeInterval)
	}
}
