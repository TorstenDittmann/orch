package network

import (
	"net"
	"net/netip"
	"testing"

	types100 "github.com/containernetworking/cni/pkg/types/100"
)

func TestAllocationAddressFromResultFailsClosed(t *testing.T) {
	subnet := netip.MustParsePrefix("10.88.0.0/24")
	gateway := netip.MustParseAddr("10.88.0.1")
	ip := func(value string) *types100.IPConfig {
		parsed := net.ParseIP(value)
		mask := net.CIDRMask(24, 32)
		if parsed.To4() == nil {
			mask = net.CIDRMask(64, 128)
		}
		return &types100.IPConfig{Address: net.IPNet{IP: parsed, Mask: mask}}
	}
	valid, err := allocationAddressFromResult([]*types100.IPConfig{ip("10.88.0.2")}, subnet, gateway)
	if err != nil || valid.String() != "10.88.0.2" {
		t.Fatalf("valid address = %s, %v", valid, err)
	}
	for name, values := range map[string][]*types100.IPConfig{
		"zero":         nil,
		"nil":          {nil},
		"multiple":     {ip("10.88.0.2"), ip("10.88.0.3")},
		"mixed family": {ip("10.88.0.2"), ip("fd00::2")},
		"IPv6 only":    {ip("fd00::2")},
		"foreign":      {ip("10.99.0.2")},
		"gateway":      {ip("10.88.0.1")},
		"network":      {ip("10.88.0.0")},
		"broadcast":    {ip("10.88.0.255")},
	} {
		t.Run(name, func(t *testing.T) {
			if address, err := allocationAddressFromResult(values, subnet, gateway); err == nil {
				t.Fatalf("accepted address %s", address)
			}
		})
	}
}
