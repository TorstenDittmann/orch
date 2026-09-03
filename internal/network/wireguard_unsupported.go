//go:build !linux

package network

func NewWireGuard(WireGuardOptions) (WireGuardManager, error) { return nil, ErrUnsupported }
