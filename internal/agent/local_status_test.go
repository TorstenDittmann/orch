package agent

import (
	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	"change.codes/torsten/better-swarm/internal/model"
	"testing"
)

func TestReadinessRequiresCurrentNetworkDNSAndAssignedIngress(t *testing.T) {
	assignment := model.Assignment{Generation: 4}
	assignment.Network.Generation = 2
	report := &orchv1alpha1.AllocationReport{AssignmentGeneration: 4, Network: &orchv1alpha1.NetworkStatus{Generation: 2, State: orchv1alpha1.NetworkState_NETWORK_STATE_READY, DnsState: model.SubsystemReady}}
	if !assignmentReady(assignment, report) {
		t.Fatal("converged assignment not ready")
	}
	assignment.Network.IngressEnabled = true
	if assignmentReady(assignment, report) {
		t.Fatal("ingress readiness ignored")
	}
	report.Network.IngressState = model.SubsystemReady
	if !assignmentReady(assignment, report) {
		t.Fatal("converged ingress not ready")
	}
	report.Network.DnsState = model.SubsystemFailed
	if assignmentReady(assignment, report) {
		t.Fatal("DNS readiness ignored")
	}
	report.Network.DnsState = model.SubsystemReady
	report.Network.Generation = 1
	if assignmentReady(assignment, report) {
		t.Fatal("old network generation accepted")
	}
	report.Network.Generation = 2
	report.AssignmentGeneration = 3
	if assignmentReady(assignment, report) {
		t.Fatal("old assignment accepted")
	}
	if assignmentReady(assignment, nil) {
		t.Fatal("nil report accepted")
	}
}
