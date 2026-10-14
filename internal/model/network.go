package model

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

const (
	DefaultWorkloadCIDR        = "10.88.0.0/16"
	ServiceDNSZone             = "svc.internal."
	DefaultWireGuardListenPort = 51820
	MaxWireGuardPeers          = 19
	MaxServiceEndpoints        = 200
)

// IsMeshEndpointAddress reports whether address may carry WireGuard underlay
// traffic between nodes. Private and public unicast IPv4 addresses are both
// accepted because the tunnel encrypts and authenticates every packet; the
// workload network, DNS, and allocation addresses remain RFC1918 regardless.
func IsMeshEndpointAddress(address netip.Addr) bool {
	// IsGlobalUnicast already rejects unspecified, broadcast, loopback,
	// multicast, and link-local addresses; 0.0.0.0/8 is not routable either.
	return address.Is4() && address.IsGlobalUnicast() && address.As4()[0] != 0
}

// ValidateMeshIdentity accepts either the empty legacy capability tuple or one
// complete, canonical node mesh identity.
func ValidateMeshIdentity(address, publicKey string, port uint32) error {
	if address == "" && publicKey == "" && port == 0 {
		return nil
	}
	parsed, err := netip.ParseAddr(address)
	if err != nil || !IsMeshEndpointAddress(parsed) || parsed.String() != address {
		return fmt.Errorf("advertise address must be a canonical unicast IPv4 address")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(publicKey)
	if err != nil || len(decoded) != 32 || base64.StdEncoding.EncodeToString(decoded) != publicKey {
		return fmt.Errorf("WireGuard public key must be canonical base64 for 32 bytes")
	}
	nonzero := false
	for _, value := range decoded {
		nonzero = nonzero || value != 0
	}
	if !nonzero {
		return fmt.Errorf("WireGuard public key must not be zero")
	}
	if port == 0 || port > 65535 {
		return fmt.Errorf("WireGuard listen port must be between 1 and 65535")
	}
	return nil
}

func ParseWorkloadCIDR(value string) (netip.Prefix, error) {
	if strings.TrimSpace(value) != value || value == "" {
		return netip.Prefix{}, fmt.Errorf("workload CIDR must be canonical")
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil || !prefix.Addr().Is4() || prefix.Masked() != prefix {
		return netip.Prefix{}, fmt.Errorf("workload CIDR must be a canonical IPv4 prefix")
	}
	if prefix.Bits() < 16 || prefix.Bits() > 19 || !prefix.Addr().IsPrivate() {
		return netip.Prefix{}, fmt.Errorf("workload CIDR must be an RFC1918 prefix between /16 and /19")
	}
	return prefix, nil
}

func FirstNodeNetwork(clusterCIDR string, generation uint64) (NetworkConfig, error) {
	return NodeNetwork(clusterCIDR, 0, generation)
}

// NodeNetwork deterministically returns the nodeSlot-th /24 in clusterCIDR.
// Slots are durable controller allocations rather than hashes of node names,
// so a node keeps its subnet across renames and the first node remains slot 0.
func NodeNetwork(clusterCIDR string, nodeSlot uint32, generation uint64) (NetworkConfig, error) {
	prefix, err := ParseWorkloadCIDR(clusterCIDR)
	if err != nil {
		return NetworkConfig{}, err
	}
	if generation == 0 {
		return NetworkConfig{}, fmt.Errorf("network generation must be positive")
	}
	subnetCount := uint32(1) << uint32(24-prefix.Bits())
	if nodeSlot >= subnetCount {
		return NetworkConfig{}, fmt.Errorf("node subnet slot %d exceeds the %d /24 subnets in %s", nodeSlot, subnetCount, prefix)
	}
	addressBytes := prefix.Addr().As4()
	address := binary.BigEndian.Uint32(addressBytes[:]) + nodeSlot*256
	var nodeBytes [4]byte
	binary.BigEndian.PutUint32(nodeBytes[:], address)
	node := netip.PrefixFrom(netip.AddrFrom4(nodeBytes), 24)
	gateway := node.Addr().Next()
	if !node.Contains(gateway) {
		return NetworkConfig{}, fmt.Errorf("node subnet has no usable gateway")
	}
	return NetworkConfig{
		Generation: generation, ClusterCIDR: prefix.String(), NodeSubnet: node.String(),
		Gateway: gateway.String(), DNSZone: ServiceDNSZone, IngressEnabled: true,
	}, nil
}

// ValidateNodeNetwork checks that a configuration describes one canonical
// /24 within the cluster prefix and uses its first usable address as gateway.
func ValidateNodeNetwork(configuration NetworkConfig) error {
	if configuration.Generation == 0 {
		return fmt.Errorf("network generation must be positive")
	}
	cluster, err := ParseWorkloadCIDR(configuration.ClusterCIDR)
	if err != nil {
		return err
	}
	node, err := netip.ParsePrefix(configuration.NodeSubnet)
	if err != nil || !node.Addr().Is4() || node.Bits() != 24 || node.Masked() != node ||
		configuration.NodeSubnet != node.String() || !cluster.Contains(node.Addr()) {
		return fmt.Errorf("node subnet must be a canonical /24 within the workload CIDR")
	}
	gateway, err := netip.ParseAddr(configuration.Gateway)
	if err != nil || !gateway.Is4() || configuration.Gateway != gateway.String() || gateway != node.Addr().Next() {
		return fmt.Errorf("node gateway must be the first usable address in the node subnet")
	}
	if configuration.DNSZone != ServiceDNSZone {
		return fmt.Errorf("network DNS zone must be %q", ServiceDNSZone)
	}
	return nil
}

// WireGuardPeer is the complete public configuration for one remote node.
// PublicKey is the canonical base64 representation of a 32-byte WireGuard key.
type WireGuardPeer struct {
	NodeID        string `json:"node_id"`
	PublicKey     string `json:"public_key"`
	Endpoint      string `json:"endpoint"`
	AllowedSubnet string `json:"allowed_subnet"`
}

// WireGuardConfig is the canonical mesh intent for one node. The local
// private key is deliberately absent and remains agent-local.
type WireGuardConfig struct {
	Generation  uint64          `json:"generation"`
	ClusterCIDR string          `json:"cluster_cidr"`
	NodeSubnet  string          `json:"node_subnet"`
	ListenPort  uint16          `json:"listen_port"`
	Peers       []WireGuardPeer `json:"peers,omitempty"`
}

// CanonicalWireGuardConfig validates scalar values and returns a copy whose
// peers are sorted by node ID. Non-canonical textual network values are
// rejected instead of silently rewritten.
func CanonicalWireGuardConfig(configuration WireGuardConfig) (WireGuardConfig, error) {
	if configuration.Generation == 0 {
		return WireGuardConfig{}, fmt.Errorf("WireGuard generation must be positive")
	}
	cluster, err := ParseWorkloadCIDR(configuration.ClusterCIDR)
	if err != nil {
		return WireGuardConfig{}, err
	}
	local, err := parseCanonicalNodeSubnet(configuration.NodeSubnet, cluster)
	if err != nil {
		return WireGuardConfig{}, fmt.Errorf("local WireGuard subnet: %w", err)
	}
	if configuration.ListenPort == 0 {
		return WireGuardConfig{}, fmt.Errorf("WireGuard listen port must be positive")
	}
	if len(configuration.Peers) > MaxWireGuardPeers {
		return WireGuardConfig{}, fmt.Errorf("WireGuard peer count exceeds %d", MaxWireGuardPeers)
	}

	canonical := configuration
	canonical.Peers = slices.Clone(configuration.Peers)
	seenNodes := make(map[string]struct{}, len(canonical.Peers))
	seenKeys := make(map[string]struct{}, len(canonical.Peers))
	seenSubnets := make(map[netip.Prefix]struct{}, len(canonical.Peers))
	for index, peer := range canonical.Peers {
		if peer.NodeID == "" || len(peer.NodeID) > 128 || strings.TrimSpace(peer.NodeID) != peer.NodeID || strings.ContainsRune(peer.NodeID, '\x00') {
			return WireGuardConfig{}, fmt.Errorf("WireGuard peer %d has an invalid node id", index)
		}
		if _, exists := seenNodes[peer.NodeID]; exists {
			return WireGuardConfig{}, fmt.Errorf("WireGuard peer node id %q is duplicated", peer.NodeID)
		}
		seenNodes[peer.NodeID] = struct{}{}

		decodedKey, err := base64.StdEncoding.Strict().DecodeString(peer.PublicKey)
		if err != nil || len(decodedKey) != 32 || base64.StdEncoding.EncodeToString(decodedKey) != peer.PublicKey || allZero(decodedKey) {
			return WireGuardConfig{}, fmt.Errorf("WireGuard peer %q has an invalid public key", peer.NodeID)
		}
		if _, exists := seenKeys[peer.PublicKey]; exists {
			return WireGuardConfig{}, fmt.Errorf("WireGuard public key is duplicated")
		}
		seenKeys[peer.PublicKey] = struct{}{}

		endpoint, err := netip.ParseAddrPort(peer.Endpoint)
		if err != nil || !IsMeshEndpointAddress(endpoint.Addr()) || endpoint.Port() == 0 || endpoint.String() != peer.Endpoint {
			return WireGuardConfig{}, fmt.Errorf("WireGuard peer %q endpoint must be a canonical unicast IPv4 address and port", peer.NodeID)
		}
		allowed, err := parseCanonicalNodeSubnet(peer.AllowedSubnet, cluster)
		if err != nil {
			return WireGuardConfig{}, fmt.Errorf("WireGuard peer %q subnet: %w", peer.NodeID, err)
		}
		if allowed == local {
			return WireGuardConfig{}, fmt.Errorf("WireGuard peer %q uses the local node subnet", peer.NodeID)
		}
		if _, exists := seenSubnets[allowed]; exists {
			return WireGuardConfig{}, fmt.Errorf("WireGuard peer subnet %q is duplicated", allowed)
		}
		seenSubnets[allowed] = struct{}{}
	}
	slices.SortFunc(canonical.Peers, func(left, right WireGuardPeer) int {
		return strings.Compare(left.NodeID, right.NodeID)
	})
	return canonical, nil
}

// ValidateWireGuardConfig additionally requires peer order to already be
// canonical, which keeps assignment bytes and reconciliation fingerprints
// deterministic.
func ValidateWireGuardConfig(configuration WireGuardConfig) error {
	canonical, err := CanonicalWireGuardConfig(configuration)
	if err != nil {
		return err
	}
	if !slices.Equal(configuration.Peers, canonical.Peers) {
		return fmt.Errorf("WireGuard peers must be in canonical node id order")
	}
	return nil
}

// ValidateServiceEndpoints ensures every catalog address is authorized by the
// local subnet or by the named WireGuard peer's allowed subnet.
func ValidateServiceEndpoints(configuration NetworkConfig) error {
	if len(configuration.Endpoints) > MaxServiceEndpoints {
		return fmt.Errorf("service endpoint count exceeds %d", MaxServiceEndpoints)
	}
	local, err := netip.ParsePrefix(configuration.NodeSubnet)
	if err != nil {
		return fmt.Errorf("service endpoint local subnet is invalid")
	}
	peerSubnets := make(map[string]netip.Prefix, len(configuration.Peers))
	for _, peer := range configuration.Peers {
		prefix, err := netip.ParsePrefix(peer.AllowedSubnet)
		if err != nil {
			return fmt.Errorf("service endpoint peer subnet is invalid")
		}
		peerSubnets[peer.NodeID] = prefix
	}
	previous := ""
	seen := make(map[string]struct{}, len(configuration.Endpoints))
	for _, endpoint := range configuration.Endpoints {
		if endpoint.AllocationID == "" || endpoint.Application == "" || endpoint.Service == "" || endpoint.NodeID == "" ||
			endpoint.Port > 65535 {
			return fmt.Errorf("service endpoint is incomplete")
		}
		if previous != "" && endpoint.AllocationID < previous {
			return fmt.Errorf("service endpoints must be in canonical allocation order")
		}
		if endpoint.HTTPPublic && endpoint.Port == 0 {
			return fmt.Errorf("service endpoint %q public HTTP route has no port", endpoint.AllocationID)
		}
		if _, exists := seen[endpoint.AllocationID]; exists {
			return fmt.Errorf("service endpoint allocation %q is duplicated", endpoint.AllocationID)
		}
		seen[endpoint.AllocationID] = struct{}{}
		previous = endpoint.AllocationID
		address, err := netip.ParseAddr(endpoint.Address)
		if err != nil || !address.Is4() || endpoint.Address != address.String() {
			return fmt.Errorf("service endpoint %q address is invalid", endpoint.AllocationID)
		}
		remote, hasPeer := peerSubnets[endpoint.NodeID]
		if !local.Contains(address) && (!hasPeer || !remote.Contains(address)) {
			return fmt.Errorf("service endpoint %q address is outside its authorized node subnet", endpoint.AllocationID)
		}
	}
	return nil
}

func parseCanonicalNodeSubnet(value string, cluster netip.Prefix) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(value)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 24 || prefix.Masked() != prefix || value != prefix.String() ||
		!cluster.Contains(prefix.Addr()) {
		return netip.Prefix{}, fmt.Errorf("must be a canonical /24 within %s", cluster)
	}
	return prefix, nil
}

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

type assignmentDocument struct {
	Network     NetworkConfig       `json:"network"`
	Allocations []AllocationDesired `json:"allocations"`
	CronJobs    []CronJobDesired    `json:"cron_jobs,omitempty"`
}

func AssignmentPayload(network NetworkConfig, allocations []AllocationDesired) ([]byte, string, error) {
	return AssignmentPayloadWithJobs(network, allocations, nil)
}

func AssignmentPayloadWithJobs(network NetworkConfig, allocations []AllocationDesired, jobs []CronJobDesired) ([]byte, string, error) {
	// Repeated protobuf fields cannot preserve the distinction between nil and
	// empty slices. Canonicalize the required collection before hashing so a
	// snapshot keeps the same digest after crossing the controller-agent wire.
	if allocations == nil {
		allocations = []AllocationDesired{}
	}
	var encoded []byte
	var err error
	if network.Generation == 0 && len(jobs) == 0 {
		// Preserve the Plan #4 payload shape long enough to read and test
		// pre-network assignments. Every new reconciler snapshot supplies a
		// positive network generation and uses the document form below.
		encoded, err = json.Marshal(allocations)
	} else {
		encoded, err = json.Marshal(assignmentDocument{Network: network, Allocations: allocations, CronJobs: jobs})
	}
	if err != nil {
		return nil, "", fmt.Errorf("encode assignment: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return encoded, "sha256:" + hex.EncodeToString(digest[:]), nil
}

func DecodeAssignmentPayload(payload []byte) (NetworkConfig, []AllocationDesired, error) {
	network, allocations, _, err := DecodeAssignmentDocument(payload)
	return network, allocations, err
}

func DecodeAssignmentDocument(payload []byte) (NetworkConfig, []AllocationDesired, []CronJobDesired, error) {
	var document assignmentDocument
	if err := json.Unmarshal(payload, &document); err == nil && document.Network.Generation != 0 {
		return document.Network, document.Allocations, document.CronJobs, nil
	}
	// Plan #4 cached/controller assignments were a bare allocation array. Keep
	// them readable so the matching new controller can replace them safely.
	var allocations []AllocationDesired
	if err := json.Unmarshal(payload, &allocations); err != nil {
		return NetworkConfig{}, nil, nil, fmt.Errorf("decode assignment: %w", err)
	}
	return NetworkConfig{}, allocations, nil, nil
}
