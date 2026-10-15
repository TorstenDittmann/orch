package network

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/TorstenDittmann/orch/internal/model"
)

// Mesh wraps the node-local CNI manager with the WireGuard transport used to
// route remote node /24s. Attachments remain entirely owned by the CNI manager.
type Mesh struct {
	local     Manager
	wireguard WireGuardManager
	advertise netip.Addr
	port      uint16
}

func NewMesh(local Manager, wireguard WireGuardManager, advertiseAddress string, listenPort uint16) (*Mesh, error) {
	if local == nil || wireguard == nil {
		return nil, fmt.Errorf("local and WireGuard network managers are required")
	}
	address, err := netip.ParseAddr(advertiseAddress)
	if err != nil || !model.IsMeshEndpointAddress(address) || address.String() != advertiseAddress {
		return nil, fmt.Errorf("advertise address must be a canonical unicast IPv4 address")
	}
	if listenPort == 0 {
		return nil, fmt.Errorf("WireGuard listen port must be positive")
	}
	return &Mesh{local: local, wireguard: wireguard, advertise: address, port: listenPort}, nil
}

func (m *Mesh) Preflight(ctx context.Context) (Capabilities, error) {
	capabilities, err := m.local.Preflight(ctx)
	if err != nil {
		return Capabilities{}, err
	}
	capabilities.AdvertiseAddress = m.advertise.String()
	capabilities.WireGuardPublicKey = m.wireguard.PublicKey()
	capabilities.WireGuardListenPort = uint32(m.port)
	if err := m.wireguard.Preflight(ctx); err != nil {
		return capabilities, fmt.Errorf("%w: %v", ErrWireGuardUnavailable, err)
	}
	return capabilities, nil
}

func (m *Mesh) Prepare(ctx context.Context, configuration model.NetworkConfig) error {
	if err := m.Validate(ctx, configuration); err != nil {
		return err
	}
	listenPort := configuration.WireGuardListenPort
	if listenPort == 0 {
		// The controller has not observed this node's hello yet. Still validate
		// ownership and converge an empty peer set before the local firewall may
		// trust the deterministic interface name.
		listenPort = m.port
	}
	wireguard := model.WireGuardConfig{
		Generation: configuration.Generation, ClusterCIDR: configuration.ClusterCIDR,
		NodeSubnet: configuration.NodeSubnet, ListenPort: listenPort,
		Peers: append([]model.WireGuardPeer(nil), configuration.Peers...),
	}
	if err := m.wireguard.EnsureOwnership(ctx); err != nil {
		return fmt.Errorf("verify WireGuard ownership: %w", err)
	}
	if err := m.local.Prepare(ctx, configuration); err != nil {
		return err
	}
	if err := m.wireguard.Reconcile(ctx, wireguard); err != nil {
		return fmt.Errorf("reconcile WireGuard mesh: %w", err)
	}
	return nil
}

func (m *Mesh) Validate(ctx context.Context, configuration model.NetworkConfig) error {
	if err := m.local.Validate(ctx, configuration); err != nil {
		return err
	}
	listenPort := configuration.WireGuardListenPort
	if listenPort == 0 {
		listenPort = m.port
	}
	return model.ValidateWireGuardConfig(model.WireGuardConfig{
		Generation: configuration.Generation, ClusterCIDR: configuration.ClusterCIDR,
		NodeSubnet: configuration.NodeSubnet, ListenPort: listenPort, Peers: configuration.Peers,
	})
}

func (m *Mesh) Attach(ctx context.Context, request AttachmentRequest) (Attachment, error) {
	return m.local.Attach(ctx, request)
}

func (m *Mesh) Inspect(ctx context.Context, request AttachmentRequest) (Attachment, error) {
	return m.local.Inspect(ctx, request)
}

func (m *Mesh) Check(ctx context.Context, request AttachmentRequest) error {
	return m.local.Check(ctx, request)
}

func (m *Mesh) GC(ctx context.Context, valid []AttachmentRequest) error {
	return m.local.GC(ctx, valid)
}

func (m *Mesh) Detach(ctx context.Context, request AttachmentRequest) error {
	return m.local.Detach(ctx, request)
}

func (m *Mesh) Close() error {
	wireGuardErr := m.wireguard.Close()
	localErr := m.local.Close()
	if wireGuardErr != nil {
		return wireGuardErr
	}
	return localErr
}

func (m *Mesh) PublishNodePorts(ctx context.Context, ports []NodePort) ([]error, error) {
	publisher, ok := m.local.(NodePortPublisher)
	if !ok {
		return nil, ErrUnsupported
	}
	return publisher.PublishNodePorts(ctx, ports)
}
