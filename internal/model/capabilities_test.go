package model

import "testing"

func TestCapabilitiesAffectDigestAndCannotUseLegacyAuthorization(t *testing.T) {
	base := AllocationDesired{Image: "example/image", CPUMillis: 100, MemoryBytes: 128 << 20}
	before, err := AllocationSpecDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Capabilities = []string{}
	if empty, err := AllocationSpecDigest(base); err != nil || empty != before {
		t.Fatalf("empty capabilities changed an existing digest: %s, %v", empty, err)
	}
	base.Capabilities = []string{"CAP_CHOWN"}
	after, err := AllocationSpecDigest(base)
	if err != nil || after == before {
		t.Fatalf("capabilities did not change digest: %s, %v", after, err)
	}
	base.SpecDigest = before
	if matches, err := AllocationSpecDigestMatches(base); err != nil || matches {
		t.Fatalf("old current digest authorized new capabilities: %v, %v", matches, err)
	}
	legacy, err := Plan4AllocationSpecDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	base.SpecDigest = legacy
	if matches, err := AllocationSpecDigestMatches(base); err != nil || matches {
		t.Fatalf("legacy digest authorized capabilities: %v, %v", matches, err)
	}
	base.Capabilities = nil
	base.Privileged = true
	if matches, err := AllocationSpecDigestMatches(base); err != nil || matches {
		t.Fatalf("legacy digest authorized privileged mode: %v, %v", matches, err)
	}
}
