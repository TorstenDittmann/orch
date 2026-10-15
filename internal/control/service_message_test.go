package control

import (
	"strings"
	"testing"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/model"
)

func TestAllocationStatusMessageIncludesDeclaredPort(t *testing.T) {
	t.Parallel()
	message := allocationStatusMessage(model.AllocationStatus{
		ID: "allocation-one", Application: "hello", Service: "web", Replica: 1,
		UpdatedAt: time.Now().UTC(), DeclaredPort: 8080,
	})
	if message.GetDeclaredPort() != 8080 {
		t.Fatalf("declared port = %d, want 8080", message.GetDeclaredPort())
	}
}

func TestServiceRuntimeSummaryJoinsDesiredAndObservedAllowlist(t *testing.T) {
	t.Parallel()
	ready := time.Now().UTC()
	exit := int32(17)
	placement := model.AllocationPlacement{
		Allocation: model.AllocationDesired{ID: "allocation-one", Application: "hello", Service: "web", Replica: 2, ApplicationGeneration: 4, RevisionID: "desired-revision", SpecDigest: "desired-spec", CPUMillis: 250, MemoryBytes: 96 << 20, Environment: map[string]string{"password": "must-not-be-in-summary"}},
		Node:       model.Node{ID: "node-one", Name: "node-1", ConnectionStatus: model.NodeDisconnected},
	}
	message := serviceRuntimeSummary(placement, &model.AllocationStatus{Phase: model.AllocationRunning, HealthStatus: model.HealthReady, ActiveSpecDigest: "active-spec", ActiveRevisionID: "active-revision", RestartCount: 3, ExitCode: &exit, ReadyAt: &ready, UpdatedAt: ready})
	if message.GetId() != "allocation-one" || message.GetReplica() != 2 || message.GetNodeConnection() != orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_DISCONNECTED || message.GetAllocationPhase() != orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_RUNNING || message.GetSpecDigest() != "active-spec" || message.GetRevisionId() != "active-revision" || message.GetCpuMillis() != 250 || !message.GetHasExitCode() {
		t.Fatalf("runtime summary = %#v", message)
	}
	if strings.Contains(message.String(), "must-not-be-in-summary") || strings.Contains(message.String(), "password") {
		t.Fatalf("runtime summary leaked desired environment: %s", message)
	}
}
