package ingress

import (
	"encoding/binary"
	"testing"
)

func TestCaddyFileCapabilitiesAllowOnlyLowPortBinding(t *testing.T) {
	t.Parallel()
	exact := capabilityPayload(fileCapabilityRevision2, 20)
	binary.LittleEndian.PutUint32(exact[4:8], netBindServiceCapability)
	if !hasOnlyNetBindServiceFileCapability(exact) {
		t.Fatal("exact cap_net_bind_service payload was rejected")
	}

	tests := []struct {
		name string
		edit func([]byte)
	}{
		{name: "effective flag missing", edit: func(value []byte) {
			binary.LittleEndian.PutUint32(value[:4], fileCapabilityRevision2)
		}},
		{name: "extra permitted capability", edit: func(value []byte) {
			binary.LittleEndian.PutUint32(value[4:8], netBindServiceCapability|1)
		}},
		{name: "inheritable capability", edit: func(value []byte) {
			binary.LittleEndian.PutUint32(value[8:12], netBindServiceCapability)
		}},
		{name: "upper capability", edit: func(value []byte) {
			binary.LittleEndian.PutUint32(value[12:16], 1)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := append([]byte(nil), exact...)
			test.edit(candidate)
			if hasOnlyNetBindServiceFileCapability(candidate) {
				t.Fatal("unsafe capability payload was accepted")
			}
		})
	}

	version1 := capabilityPayload(fileCapabilityRevision1, 12)
	binary.LittleEndian.PutUint32(version1[4:8], netBindServiceCapability)
	if !hasOnlyNetBindServiceFileCapability(version1) {
		t.Fatal("exact revision-1 payload was rejected")
	}
	version3 := capabilityPayload(fileCapabilityRevision3, 24)
	binary.LittleEndian.PutUint32(version3[4:8], netBindServiceCapability)
	if !hasOnlyNetBindServiceFileCapability(version3) {
		t.Fatal("exact initial-namespace revision-3 payload was rejected")
	}
	binary.LittleEndian.PutUint32(version3[20:24], 1)
	if hasOnlyNetBindServiceFileCapability(version3) {
		t.Fatal("foreign user-namespace capability payload was accepted")
	}
	if hasOnlyNetBindServiceFileCapability(exact[:len(exact)-1]) {
		t.Fatal("truncated capability payload was accepted")
	}
}

func capabilityPayload(revision uint32, size int) []byte {
	value := make([]byte, size)
	binary.LittleEndian.PutUint32(value[:4], revision|fileCapabilityEffective)
	return value
}
