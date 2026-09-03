package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
)

const (
	DefaultWorkloadCIDR = "10.88.0.0/16"
	ServiceDNSZone      = "svc.internal."
)

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
	prefix, err := ParseWorkloadCIDR(clusterCIDR)
	if err != nil {
		return NetworkConfig{}, err
	}
	if generation == 0 {
		return NetworkConfig{}, fmt.Errorf("network generation must be positive")
	}
	node := netip.PrefixFrom(prefix.Addr(), 24).Masked()
	gateway := node.Addr().Next()
	if !node.Contains(gateway) {
		return NetworkConfig{}, fmt.Errorf("node subnet has no usable gateway")
	}
	return NetworkConfig{
		Generation: generation, ClusterCIDR: prefix.String(), NodeSubnet: node.String(),
		Gateway: gateway.String(), DNSZone: ServiceDNSZone,
	}, nil
}

type assignmentDocument struct {
	Network     NetworkConfig       `json:"network"`
	Allocations []AllocationDesired `json:"allocations"`
}

func AssignmentPayload(network NetworkConfig, allocations []AllocationDesired) ([]byte, string, error) {
	var encoded []byte
	var err error
	if network.Generation == 0 {
		// Preserve the Plan #4 payload shape long enough to read and test
		// pre-network assignments. Every new reconciler snapshot supplies a
		// positive network generation and uses the document form below.
		encoded, err = json.Marshal(allocations)
	} else {
		encoded, err = json.Marshal(assignmentDocument{Network: network, Allocations: allocations})
	}
	if err != nil {
		return nil, "", fmt.Errorf("encode assignment: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return encoded, "sha256:" + hex.EncodeToString(digest[:]), nil
}

func DecodeAssignmentPayload(payload []byte) (NetworkConfig, []AllocationDesired, error) {
	var document assignmentDocument
	if err := json.Unmarshal(payload, &document); err == nil && document.Network.Generation != 0 {
		return document.Network, document.Allocations, nil
	}
	// Plan #4 cached/controller assignments were a bare allocation array. Keep
	// them readable so the matching new controller can replace them safely.
	var allocations []AllocationDesired
	if err := json.Unmarshal(payload, &allocations); err != nil {
		return NetworkConfig{}, nil, fmt.Errorf("decode assignment: %w", err)
	}
	return NetworkConfig{}, allocations, nil
}
