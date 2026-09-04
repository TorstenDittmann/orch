//go:build linux

package network

import (
	"bytes"
	"testing"
)

func TestBridgeHardwareAddressIsStableLocalAndScoped(t *testing.T) {
	t.Parallel()
	first := bridgeHardwareAddress("orch-managed-cluster-one-node-one")
	repeat := bridgeHardwareAddress("orch-managed-cluster-one-node-one")
	other := bridgeHardwareAddress("orch-managed-cluster-one-node-two")
	if len(first) != 6 || !bytes.Equal(first, repeat) || bytes.Equal(first, other) {
		t.Fatalf("bridge addresses = %v, %v, %v", first, repeat, other)
	}
	if first[0]&0x01 != 0 || first[0]&0x02 == 0 {
		t.Fatalf("bridge address %s is not locally administered unicast", first)
	}
}
