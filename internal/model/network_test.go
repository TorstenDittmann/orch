package model

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"
)

func TestFirstNodeNetworkIsDeterministic(t *testing.T) {
	configuration, err := FirstNodeNetwork("10.96.0.0/16", 7)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Generation != 7 || configuration.NodeSubnet != "10.96.0.0/24" ||
		configuration.Gateway != "10.96.0.1" || configuration.DNSZone != ServiceDNSZone {
		t.Fatalf("FirstNodeNetwork() = %#v", configuration)
	}
}

func TestValidateMeshIdentityRequiresCompleteCanonicalPrivateIdentity(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(slices.Repeat([]byte{1}, 32))
	if err := ValidateMeshIdentity("", "", 0); err != nil {
		t.Fatalf("legacy empty identity = %v", err)
	}
	for _, address := range []string{"10.0.0.1", "203.0.113.1", "100.64.0.7"} {
		if err := ValidateMeshIdentity(address, key, 51820); err != nil {
			t.Fatalf("valid identity %s = %v", address, err)
		}
	}
	cases := []struct {
		address string
		key     string
		port    uint32
	}{
		{"10.0.0.1", "", 51820},
		{"0.0.0.0", key, 51820},
		{"0.1.2.3", key, 51820},
		{"127.0.0.1", key, 51820},
		{"169.254.1.1", key, 51820},
		{"224.0.0.1", key, 51820},
		{"255.255.255.255", key, 51820},
		{"fd00::1", key, 51820},
		{"10.0.0.1", "not-a-key", 51820},
		{"10.0.0.1", base64.StdEncoding.EncodeToString(make([]byte, 32)), 51820},
		{"10.0.0.1", key, 0},
		{"10.0.0.1", key, 65536},
	}
	for _, candidate := range cases {
		if err := ValidateMeshIdentity(candidate.address, candidate.key, candidate.port); err == nil {
			t.Fatalf("ValidateMeshIdentity(%q, %q, %d) succeeded", candidate.address, candidate.key, candidate.port)
		}
	}
}

func TestNodeNetworkAllocatesStableDistinctSubnets(t *testing.T) {
	first, err := NodeNetwork("10.96.0.0/19", 0, 7)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NodeNetwork("10.96.0.0/19", 1, 7)
	if err != nil {
		t.Fatal(err)
	}
	last, err := NodeNetwork("10.96.0.0/19", 31, 7)
	if err != nil {
		t.Fatal(err)
	}
	if first.NodeSubnet != "10.96.0.0/24" || first.Gateway != "10.96.0.1" ||
		second.NodeSubnet != "10.96.1.0/24" || second.Gateway != "10.96.1.1" ||
		last.NodeSubnet != "10.96.31.0/24" || last.Gateway != "10.96.31.1" {
		t.Fatalf("node networks = %#v, %#v, %#v", first, second, last)
	}
	legacy, err := FirstNodeNetwork("10.96.0.0/19", 7)
	if err != nil || !NetworkConfigEqual(legacy, first) {
		t.Fatalf("FirstNodeNetwork() = %#v, %v; want %#v", legacy, err, first)
	}
	if _, err := NodeNetwork("10.96.0.0/19", 32, 7); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("NodeNetwork() exhaustion error = %v", err)
	}
	if _, err := NodeNetwork(DefaultWorkloadCIDR, 0, 0); err == nil {
		t.Fatal("NodeNetwork() accepted zero generation")
	}
}

func TestValidateNodeNetworkAcceptsAnyAssignedSlot(t *testing.T) {
	configuration, err := NodeNetwork(DefaultWorkloadCIDR, 17, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateNodeNetwork(configuration); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*NetworkConfig){
		"generation": func(value *NetworkConfig) { value.Generation = 0 },
		"cluster":    func(value *NetworkConfig) { value.ClusterCIDR = "10.88.1.0/16" },
		"subnet":     func(value *NetworkConfig) { value.NodeSubnet = "10.99.0.0/24" },
		"gateway":    func(value *NetworkConfig) { value.Gateway = "10.88.17.2" },
		"dns":        func(value *NetworkConfig) { value.DNSZone = "other.internal." },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := configuration
			edit(&candidate)
			if err := ValidateNodeNetwork(candidate); err == nil {
				t.Fatalf("ValidateNodeNetwork(%#v) succeeded", candidate)
			}
		})
	}
}

func TestWireGuardConfigurationCanonicalizationAndValidation(t *testing.T) {
	keyOne := base64.StdEncoding.EncodeToString(slices.Repeat([]byte{1}, 32))
	keyTwo := base64.StdEncoding.EncodeToString(slices.Repeat([]byte{2}, 32))
	configuration := WireGuardConfig{
		Generation: 3, ClusterCIDR: DefaultWorkloadCIDR, NodeSubnet: "10.88.0.0/24",
		ListenPort: DefaultWireGuardListenPort,
		Peers: []WireGuardPeer{
			{NodeID: "node-two", PublicKey: keyTwo, Endpoint: "10.0.0.2:51820", AllowedSubnet: "10.88.2.0/24"},
			{NodeID: "node-one", PublicKey: keyOne, Endpoint: "10.0.0.1:51820", AllowedSubnet: "10.88.1.0/24"},
		},
	}
	canonical, err := CanonicalWireGuardConfig(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if canonical.Peers[0].NodeID != "node-one" || configuration.Peers[0].NodeID != "node-two" {
		t.Fatalf("canonical peers = %#v; original = %#v", canonical.Peers, configuration.Peers)
	}
	if err := ValidateWireGuardConfig(configuration); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("unordered ValidateWireGuardConfig() error = %v", err)
	}
	if err := ValidateWireGuardConfig(canonical); err != nil {
		t.Fatal(err)
	}
}

func TestWireGuardConfigurationRejectsUnsafePeerIntent(t *testing.T) {
	keyOne := base64.StdEncoding.EncodeToString(slices.Repeat([]byte{1}, 32))
	keyTwo := base64.StdEncoding.EncodeToString(slices.Repeat([]byte{2}, 32))
	valid := WireGuardConfig{
		Generation: 1, ClusterCIDR: DefaultWorkloadCIDR, NodeSubnet: "10.88.0.0/24",
		ListenPort: DefaultWireGuardListenPort,
		Peers: []WireGuardPeer{{
			NodeID: "node-one", PublicKey: keyOne, Endpoint: "10.0.0.1:51820", AllowedSubnet: "10.88.1.0/24",
		}},
	}
	cases := map[string]func(*WireGuardConfig){
		"generation":   func(value *WireGuardConfig) { value.Generation = 0 },
		"cluster":      func(value *WireGuardConfig) { value.ClusterCIDR = "10.88.1.0/16" },
		"local subnet": func(value *WireGuardConfig) { value.NodeSubnet = "10.88.0.1/24" },
		"listen port":  func(value *WireGuardConfig) { value.ListenPort = 0 },
		"node id":      func(value *WireGuardConfig) { value.Peers[0].NodeID = " node-one" },
		"public key":   func(value *WireGuardConfig) { value.Peers[0].PublicKey = "not-a-key" },
		"zero public key": func(value *WireGuardConfig) {
			value.Peers[0].PublicKey = base64.StdEncoding.EncodeToString(make([]byte, 32))
		},
		"loopback endpoint": func(value *WireGuardConfig) { value.Peers[0].Endpoint = "127.0.0.1:51820" },
		"link-local":        func(value *WireGuardConfig) { value.Peers[0].Endpoint = "169.254.0.1:51820" },
		"endpoint spelling": func(value *WireGuardConfig) { value.Peers[0].Endpoint = "10.0.0.1:051820" },
		"local peer subnet": func(value *WireGuardConfig) { value.Peers[0].AllowedSubnet = value.NodeSubnet },
		"foreign subnet":    func(value *WireGuardConfig) { value.Peers[0].AllowedSubnet = "10.99.0.0/24" },
		"duplicate node": func(value *WireGuardConfig) {
			value.Peers = append(value.Peers, WireGuardPeer{NodeID: "node-one", PublicKey: keyTwo, Endpoint: "10.0.0.2:51820", AllowedSubnet: "10.88.2.0/24"})
		},
		"duplicate key": func(value *WireGuardConfig) {
			value.Peers = append(value.Peers, WireGuardPeer{NodeID: "node-two", PublicKey: keyOne, Endpoint: "10.0.0.2:51820", AllowedSubnet: "10.88.2.0/24"})
		},
		"duplicate subnet": func(value *WireGuardConfig) {
			value.Peers = append(value.Peers, WireGuardPeer{NodeID: "node-two", PublicKey: keyTwo, Endpoint: "10.0.0.2:51820", AllowedSubnet: "10.88.1.0/24"})
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.Peers = slices.Clone(valid.Peers)
			edit(&candidate)
			if _, err := CanonicalWireGuardConfig(candidate); err == nil {
				t.Fatalf("CanonicalWireGuardConfig(%#v) succeeded", candidate)
			}
		})
	}
}

func TestServiceEndpointsMustBelongToLocalOrNamedPeerSubnet(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(slices.Repeat([]byte{1}, 32))
	configuration, err := NodeNetwork(DefaultWorkloadCIDR, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	configuration.Peers = []WireGuardPeer{{
		NodeID: "node-two", PublicKey: key, Endpoint: "10.0.0.2:51820", AllowedSubnet: "10.88.1.0/24",
	}}
	configuration.Endpoints = []ServiceEndpoint{
		{AllocationID: "alloc-local", Application: "hello", Service: "web", Address: "10.88.0.2", Port: 8080, NodeID: "node-one"},
		{AllocationID: "alloc-remote", Application: "hello", Service: "web", Address: "10.88.1.2", Port: 8080, NodeID: "node-two"},
	}
	if err := ValidateServiceEndpoints(configuration); err != nil {
		t.Fatal(err)
	}
	configuration.Endpoints[1].Address = "10.88.2.2"
	if err := ValidateServiceEndpoints(configuration); err == nil || !strings.Contains(err.Error(), "authorized") {
		t.Fatalf("foreign endpoint error = %v", err)
	}
}

func TestParseWorkloadCIDRRejectsUnsafeOrNoncanonicalPrefixes(t *testing.T) {
	for _, value := range []string{"", "10.88.1.0/16", "10.0.0.0/8", "10.0.0.0/20", "192.0.2.0/16", "fd00::/64", " 10.88.0.0/16"} {
		if _, err := ParseWorkloadCIDR(value); err == nil {
			t.Fatalf("ParseWorkloadCIDR(%q) succeeded", value)
		}
	}
}

func TestAssignmentPayloadReadsLegacyAndNetworkDocuments(t *testing.T) {
	legacy, legacyDigest, err := AssignmentPayload(NetworkConfig{}, []AllocationDesired{})
	if err != nil || string(legacy) != "[]" || legacyDigest == "" {
		t.Fatalf("legacy payload = %q, %q, %v", legacy, legacyDigest, err)
	}
	legacyNetwork, legacyAllocations, err := DecodeAssignmentPayload(legacy)
	if err != nil || legacyNetwork.Generation != 0 || len(legacyAllocations) != 0 {
		t.Fatalf("legacy decode = %#v, %#v, %v", legacyNetwork, legacyAllocations, err)
	}
	configuration, _ := FirstNodeNetwork(DefaultWorkloadCIDR, 1)
	payload, _, err := AssignmentPayload(configuration, []AllocationDesired{})
	if err != nil {
		t.Fatal(err)
	}
	decoded, allocations, err := DecodeAssignmentPayload(payload)
	if err != nil || !NetworkConfigEqual(decoded, configuration) || len(allocations) != 0 {
		t.Fatalf("network decode = %#v, %#v, %v", decoded, allocations, err)
	}
}

func TestAssignmentPayloadCanonicalizesEmptyAllocations(t *testing.T) {
	configuration, err := FirstNodeNetwork(DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	nilPayload, nilDigest, err := AssignmentPayloadWithJobs(configuration, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	emptyPayload, emptyDigest, err := AssignmentPayloadWithJobs(configuration, []AllocationDesired{}, []CronJobDesired{})
	if err != nil {
		t.Fatal(err)
	}
	if string(nilPayload) != string(emptyPayload) || nilDigest != emptyDigest {
		t.Fatalf("nil assignment = %q, %q; empty assignment = %q, %q", nilPayload, nilDigest, emptyPayload, emptyDigest)
	}
	if !strings.Contains(string(nilPayload), `"allocations":[]`) {
		t.Fatalf("canonical empty assignment = %q, want empty allocation array", nilPayload)
	}
}

func TestPlan4AllocationDigestIsAcceptedOnlyForUnextendedIntent(t *testing.T) {
	allocation := AllocationDesired{
		ID: "alloc-one", Application: "hello", Service: "worker", Replica: 1,
		ApplicationGeneration: 1, RevisionID: "revision-one",
		Image:   "docker.io/library/busybox@sha256:" + strings.Repeat("a", 64),
		Command: []string{"/bin/sleep"}, Args: []string{"3600"}, RestartPolicy: "always",
		CPUMillis: 100, MemoryBytes: 128 << 20, StartDeadlineSeconds: 30,
		AutoRollback: true, StartBeforeStop: true,
	}
	legacy, err := Plan4AllocationSpecDigest(allocation)
	if err != nil {
		t.Fatal(err)
	}
	current, err := AllocationSpecDigest(allocation)
	if err != nil || current == legacy {
		t.Fatalf("current digest = %q, legacy = %q, err = %v", current, legacy, err)
	}
	allocation.SpecDigest = legacy
	if matches, err := AllocationSpecDigestMatches(allocation); err != nil || !matches {
		t.Fatalf("legacy digest match = %t, %v", matches, err)
	}
	if legacyAllocation, err := IsPlan4Allocation(allocation); err != nil || !legacyAllocation {
		t.Fatalf("IsPlan4Allocation() = %t, %v", legacyAllocation, err)
	}
	allocation.MaxSurge = 1
	if matches, err := AllocationSpecDigestMatches(allocation); err != nil || matches {
		t.Fatalf("extended legacy digest match = %t, %v", matches, err)
	}
	allocation.MaxSurge = 0
	allocation.SpecDigest = current
	if legacyAllocation, err := IsPlan4Allocation(allocation); err != nil || legacyAllocation {
		t.Fatalf("current allocation reported as legacy = %t, %v", legacyAllocation, err)
	}
}

func TestWireGuardConfigurationAcceptsPublicPeerEndpoint(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(slices.Repeat([]byte{1}, 32))
	configuration := WireGuardConfig{
		Generation: 1, ClusterCIDR: DefaultWorkloadCIDR, NodeSubnet: "10.88.0.0/24",
		ListenPort: DefaultWireGuardListenPort,
		Peers: []WireGuardPeer{{
			NodeID: "game-us", PublicKey: key, Endpoint: "203.0.113.1:51820", AllowedSubnet: "10.88.1.0/24",
		}},
	}
	if err := ValidateWireGuardConfig(configuration); err != nil {
		t.Fatalf("public peer endpoint rejected: %v", err)
	}
}
