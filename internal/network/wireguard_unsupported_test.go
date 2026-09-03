//go:build !linux

package network

import (
	"errors"
	"testing"
)

func TestNewWireGuardIsExplicitlyUnsupportedOffLinux(t *testing.T) {
	manager, err := NewWireGuard(WireGuardOptions{ClusterID: "cluster-one", NodeID: "node-one", StateDir: t.TempDir()})
	if manager != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("NewWireGuard() = %#v, %v", manager, err)
	}
}
