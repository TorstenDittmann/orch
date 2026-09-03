package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
)

func TestConfigureNetworkIsReplayableAndFreezesAfterReady(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	cluster := testCluster("cluster-one")
	if err := state.InitializeCluster(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	updatedAt := testCreatedAt.Add(time.Minute)
	input := model.ConfigureNetwork{
		CIDR: "10.96.0.0/16", IdempotencyKey: "network-key-one-1", RequestHash: strings.Repeat("a", 64), UpdatedAt: updatedAt,
	}
	first, err := state.ConfigureNetwork(ctx, input)
	if err != nil || !first.Changed || first.Generation != 2 || first.CIDR != input.CIDR || first.EventSequence != 2 {
		t.Fatalf("ConfigureNetwork() = %#v, %v", first, err)
	}
	replay, err := state.ConfigureNetwork(ctx, input)
	if err != nil || replay != first {
		t.Fatalf("ConfigureNetwork() replay = %#v, %v", replay, err)
	}
	conflict := input
	conflict.RequestHash = strings.Repeat("b", 64)
	if _, err := state.ConfigureNetwork(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting replay = %v", err)
	}
	ready, _ := model.FirstNodeNetwork(input.CIDR, first.Generation)
	frozen, err := state.FreezeNetwork(ctx, "node-one", model.NetworkStatus{
		Generation: ready.Generation, State: model.NetworkReady, ClusterCIDR: ready.ClusterCIDR,
		NodeSubnet: ready.NodeSubnet, Gateway: ready.Gateway,
	}, updatedAt.Add(time.Minute))
	if err != nil || !frozen {
		t.Fatalf("FreezeNetwork() = %v, %v", frozen, err)
	}
	if _, err := state.ConfigureNetwork(ctx, model.ConfigureNetwork{
		CIDR: "10.112.0.0/16", IdempotencyKey: "network-key-two-2", RequestHash: strings.Repeat("c", 64), UpdatedAt: updatedAt.Add(2 * time.Minute),
	}); !errors.Is(err, ErrNetworkFrozen) {
		t.Fatalf("configure frozen network = %v", err)
	}
}

func TestValidateNetworkObservationRequiresKnownSubsystemStates(t *testing.T) {
	t.Parallel()
	desired, err := model.FirstNodeNetwork("10.88.0.0/16", 1)
	if err != nil {
		t.Fatal(err)
	}
	valid := model.NetworkStatus{
		Generation: desired.Generation, State: model.NetworkReady, ClusterCIDR: desired.ClusterCIDR,
		NodeSubnet: desired.NodeSubnet, Gateway: desired.Gateway,
		DNSState: model.SubsystemReady, IngressState: model.SubsystemReady,
	}
	if err := validateNetworkObservation(valid, desired); err != nil {
		t.Fatalf("valid observation: %v", err)
	}
	computeOnly := desired
	computeOnly.IngressEnabled = false
	computeObservation := valid
	computeObservation.IngressState = model.SubsystemNotConfigured
	if err := validateNetworkObservation(computeObservation, computeOnly); err != nil {
		t.Fatalf("valid compute-only observation: %v", err)
	}

	for name, mutate := range map[string]func(*model.NetworkStatus){
		"unknown DNS state":     func(status *model.NetworkStatus) { status.DNSState = "unknown" },
		"unknown ingress state": func(status *model.NetworkStatus) { status.IngressState = "unknown" },
		"ready without DNS": func(status *model.NetworkStatus) {
			status.DNSState = model.SubsystemPending
		},
		"ready without ingress": func(status *model.NetworkStatus) {
			status.IngressState = model.SubsystemNotConfigured
		},
	} {
		t.Run(name, func(t *testing.T) {
			observation := valid
			mutate(&observation)
			if err := validateNetworkObservation(observation, desired); err == nil {
				t.Fatal("expected validation failure")
			}
		})
	}
}

func TestValidateNetworkedRunningObservationRequiresUsableHostAddress(t *testing.T) {
	t.Parallel()
	network, err := model.FirstNodeNetwork("10.88.0.0/16", 1)
	if err != nil {
		t.Fatal(err)
	}
	desired := model.AllocationDesired{
		ID: "alloc-one", Application: "hello", Service: "worker", Replica: 1,
		ApplicationGeneration: 1, RevisionID: "revision-one", SpecDigest: "sha256:" + strings.Repeat("a", 64),
		NetworkGeneration: 1,
	}
	observation := model.AllocationStatus{
		ID: desired.ID, Phase: model.AllocationRunning, HealthStatus: model.HealthNotConfigured,
		ActiveSpecDigest: desired.SpecDigest, ActiveGeneration: desired.ApplicationGeneration,
		ActiveRevisionID: desired.RevisionID,
	}
	for _, address := range []string{"", "10.88.0.0", "10.88.0.1", "10.88.0.255"} {
		observation.Address = address
		if err := validateObservationAgainstDesired(observation, desired, network); err == nil {
			t.Fatalf("accepted unusable address %q", address)
		}
	}
	observation.Address = "10.88.0.2"
	if err := validateObservationAgainstDesired(observation, desired, network); err != nil {
		t.Fatalf("valid host address: %v", err)
	}
}

func TestValidateObservationUsesAuthorizedRollbackHealthContract(t *testing.T) {
	t.Parallel()
	network, err := model.FirstNodeNetwork("10.88.0.0/16", 1)
	if err != nil {
		t.Fatal(err)
	}
	desired := model.AllocationDesired{
		ID: "alloc-one", ApplicationGeneration: 2, RevisionID: "revision-two",
		SpecDigest: "sha256:" + strings.Repeat("b", 64), NetworkGeneration: 1,
		Rollback: &model.RollbackSpec{
			ApplicationGeneration: 1, RevisionID: "revision-one",
			SpecDigest: "sha256:" + strings.Repeat("a", 64), NetworkGeneration: 1,
			HTTP: &model.HTTPDesired{Port: 8080, HealthPath: "/healthz"},
		},
	}
	readyAt := time.Now().UTC()
	observation := model.AllocationStatus{
		ID: desired.ID, Phase: model.AllocationRunning, HealthStatus: model.HealthReady,
		Address: "10.88.0.2", ActiveSpecDigest: desired.Rollback.SpecDigest,
		ActiveGeneration: desired.Rollback.ApplicationGeneration, ActiveRevisionID: desired.Rollback.RevisionID,
		ReadyAt: &readyAt,
	}
	if err := validateObservationAgainstDesired(observation, desired, network); err != nil {
		t.Fatalf("authorized HTTP rollback observation: %v", err)
	}

	desired.HTTP = &model.HTTPDesired{Port: 9090, HealthPath: "/ready"}
	desired.Rollback.HTTP = nil
	observation.HealthStatus = model.HealthNotConfigured
	observation.ReadyAt = nil
	observation.DeclaredPort = desired.HTTP.Port
	if err := validateObservationAgainstDesired(observation, desired, network); err != nil {
		t.Fatalf("authorized non-HTTP rollback observation: %v", err)
	}
}
