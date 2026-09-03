package model

import (
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
	if err != nil || decoded != configuration || len(allocations) != 0 {
		t.Fatalf("network decode = %#v, %#v, %v", decoded, allocations, err)
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
