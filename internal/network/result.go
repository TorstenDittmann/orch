package network

import (
	"encoding/binary"
	"fmt"
	"net/netip"

	types100 "github.com/containernetworking/cni/pkg/types/100"
)

func allocationAddressFromResult(ips []*types100.IPConfig, subnet netip.Prefix, gateway netip.Addr) (netip.Addr, error) {
	if len(ips) != 1 || ips[0] == nil {
		return netip.Addr{}, fmt.Errorf("CNI result must contain exactly one allocation address")
	}
	address, ok := netip.AddrFromSlice(ips[0].Address.IP)
	if !ok {
		return netip.Addr{}, fmt.Errorf("CNI result contains an invalid allocation address")
	}
	address = address.Unmap()
	if !address.Is4() || !subnet.Contains(address) || address == gateway ||
		address == subnet.Masked().Addr() || address == finalIPv4Address(subnet) {
		return netip.Addr{}, fmt.Errorf("CNI result contains an unusable allocation IPv4 address")
	}
	return address, nil
}

func finalIPv4Address(prefix netip.Prefix) netip.Addr {
	masked := prefix.Masked()
	if !masked.Addr().Is4() {
		return netip.Addr{}
	}
	bytes := masked.Addr().As4()
	value := binary.BigEndian.Uint32(bytes[:])
	hostBits := uint(32 - masked.Bits())
	if hostBits == 32 {
		value = ^uint32(0)
	} else {
		value |= (uint32(1) << hostBits) - 1
	}
	binary.BigEndian.PutUint32(bytes[:], value)
	return netip.AddrFrom4(bytes)
}
