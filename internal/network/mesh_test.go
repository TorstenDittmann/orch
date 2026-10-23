package network

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/TorstenDittmann/orch/internal/model"
)

type meshTestLocal struct {
	Manager
	prepared int
}

func (*meshTestLocal) Validate(context.Context, model.NetworkConfig) error { return nil }

func (l *meshTestLocal) Prepare(context.Context, model.NetworkConfig) error {
	l.prepared++
	return nil
}

type meshTestWireGuard struct {
	WireGuardManager
	ownershipErr error
	reconciled   int
}

func (w *meshTestWireGuard) EnsureOwnership(context.Context) error { return w.ownershipErr }

func (w *meshTestWireGuard) Reconcile(context.Context, model.WireGuardConfig) error {
	w.reconciled++
	return nil
}

func TestMeshForwardsNodeLocalFirewallChanges(t *testing.T) {
	local := NewFake()
	mesh, err := NewMesh(local, &meshTestWireGuard{}, "10.0.0.11", model.DefaultWireGuardListenPort)
	if err != nil {
		t.Fatal(err)
	}
	// The agent hands the mesh to the supervisor as its node-port publisher.
	var allower IngressPortAllower = mesh
	if err := allower.AllowIngressPorts(context.Background(), []uint16{9418}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(local.IngressPorts, []uint16{9418}) {
		t.Fatalf("ingress ports = %v", local.IngressPorts)
	}
}

func TestMeshRunsSingleNodeWithoutKernelWireGuard(t *testing.T) {
	unsupported := fmt.Errorf("create WireGuard interface: %w", ErrWireGuardUnavailable)
	peer := testWireGuardConfiguration(t, "node-two", "10.88.1.0/24", "10.0.0.12:51820", 1).Peers[0]
	configuration := model.NetworkConfig{
		Generation: 1, ClusterCIDR: model.DefaultWorkloadCIDR, NodeSubnet: "10.88.0.0/24", Gateway: "10.88.0.1",
		WireGuardListenPort: model.DefaultWireGuardListenPort,
	}

	local, wireguard := &meshTestLocal{}, &meshTestWireGuard{ownershipErr: unsupported}
	mesh, err := NewMesh(local, wireguard, "10.0.0.11", model.DefaultWireGuardListenPort)
	if err != nil {
		t.Fatal(err)
	}
	if err := mesh.Prepare(context.Background(), configuration); err != nil {
		t.Fatalf("single node without WireGuard: %v", err)
	}
	if local.prepared != 1 || wireguard.reconciled != 0 {
		t.Fatalf("prepared %d, reconciled %d", local.prepared, wireguard.reconciled)
	}

	configuration.Peers = []model.WireGuardPeer{peer}
	if err := mesh.Prepare(context.Background(), configuration); !errors.Is(err, ErrWireGuardUnavailable) {
		t.Fatalf("node with peers and no WireGuard = %v", err)
	}
}
