//go:build linux

package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

const orchWireGuardRouteProtocol = 99

type linuxWireGuardBackend struct {
	client *wgctrl.Client
}

// NewWireGuard constructs the Linux kernel WireGuard manager. Kernel support
// and permissions are checked by Preflight before reconciliation is allowed.
func NewWireGuard(options WireGuardOptions) (WireGuardManager, error) {
	if err := ValidateWireGuardOptions(options); err != nil {
		return nil, err
	}
	stateDir, err := filepath.Abs(options.StateDir)
	if err != nil {
		return nil, err
	}
	options.StateDir = stateDir
	if options.KeyPath != "" {
		keyPath, err := filepath.Abs(options.KeyPath)
		if err != nil {
			return nil, err
		}
		options.KeyPath = keyPath
	}
	if err := ensurePrivateDirectory(options.StateDir); err != nil {
		return nil, err
	}
	return newWireGuardManager(options, &linuxWireGuardBackend{}, nil)
}

func (b *linuxWireGuardBackend) Preflight(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.client != nil {
		return nil
	}
	client, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("open WireGuard control socket: %w", err)
	}
	if _, err := client.Devices(); err != nil {
		_ = client.Close()
		return fmt.Errorf("query kernel WireGuard support: %w", err)
	}
	b.client = client
	return nil
}

func (*linuxWireGuardBackend) Link(name string) (wireGuardLink, bool, error) {
	link, err := netlink.LinkByName(name)
	var notFound netlink.LinkNotFoundError
	if errors.As(err, &notFound) {
		return wireGuardLink{}, false, nil
	}
	if err != nil {
		return wireGuardLink{}, false, err
	}
	return wireGuardLink{Name: link.Attrs().Name, Alias: link.Attrs().Alias, Kind: link.Type()}, true, nil
}

func (*linuxWireGuardBackend) CreateLink(name, alias string) error {
	link := &netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Name: name, Alias: alias}, LinkType: "wireguard"}
	return netlink.LinkAdd(link)
}

func (*linuxWireGuardBackend) EnsureRoute(interfaceName string, prefix netip.Prefix, source netip.Addr) error {
	link, err := netlink.LinkByName(interfaceName)
	if err != nil {
		return err
	}
	destination := prefixIPNet(prefix)
	filter := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: &destination}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, filter, netlink.RT_FILTER_OIF|netlink.RT_FILTER_DST)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if route.LinkIndex == link.Attrs().Index && route.Dst != nil && route.Dst.String() == prefix.String() &&
			int(route.Protocol) == orchWireGuardRouteProtocol {
			if route.Src != nil && route.Src.String() == source.String() {
				return nil
			}
			desired := &netlink.Route{
				LinkIndex: link.Attrs().Index, Dst: &destination, Src: source.AsSlice(), Scope: netlink.SCOPE_LINK,
				Protocol: netlink.RouteProtocol(orchWireGuardRouteProtocol),
			}
			return netlink.RouteReplace(desired)
		}
		if route.Dst != nil && route.Dst.String() == prefix.String() {
			return fmt.Errorf("destination already has a foreign route: %w", ErrOwnershipMismatch)
		}
	}
	route := &netlink.Route{
		LinkIndex: link.Attrs().Index, Dst: &destination, Src: source.AsSlice(), Scope: netlink.SCOPE_LINK,
		Protocol: netlink.RouteProtocol(orchWireGuardRouteProtocol),
	}
	if err := netlink.RouteAdd(route); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("destination already has a route not owned by orch: %w", ErrOwnershipMismatch)
		}
		return err
	}
	return nil
}

func (b *linuxWireGuardBackend) Configure(interfaceName string, desired wireGuardDeviceConfig) error {
	if b.client == nil {
		return fmt.Errorf("WireGuard control socket is unavailable")
	}
	privateKey := desired.PrivateKey
	listenPort := desired.ListenPort
	configuration := wgtypes.Config{
		PrivateKey: &privateKey, ListenPort: &listenPort, ReplacePeers: true,
		Peers: make([]wgtypes.PeerConfig, 0, len(desired.Peers)),
	}
	for _, peer := range desired.Peers {
		endpoint := net.UDPAddrFromAddrPort(peer.Endpoint)
		allowed := prefixIPNet(peer.AllowedSubnet)
		configuration.Peers = append(configuration.Peers, wgtypes.PeerConfig{
			PublicKey: peer.PublicKey, Endpoint: endpoint, ReplaceAllowedIPs: true,
			AllowedIPs: []net.IPNet{allowed},
		})
	}
	return b.client.ConfigureDevice(interfaceName, configuration)
}

func (*linuxWireGuardBackend) SetLinkUp(interfaceName string) error {
	link, err := netlink.LinkByName(interfaceName)
	if err != nil {
		return err
	}
	return netlink.LinkSetUp(link)
}

func (*linuxWireGuardBackend) RemoveRoute(interfaceName string, prefix netip.Prefix) error {
	link, err := netlink.LinkByName(interfaceName)
	var notFound netlink.LinkNotFoundError
	if errors.As(err, &notFound) {
		return nil
	}
	if err != nil {
		return err
	}
	destination := prefixIPNet(prefix)
	filter := &netlink.Route{
		LinkIndex: link.Attrs().Index, Dst: &destination,
		Protocol: netlink.RouteProtocol(orchWireGuardRouteProtocol),
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, filter,
		netlink.RT_FILTER_OIF|netlink.RT_FILTER_DST|netlink.RT_FILTER_PROTOCOL)
	if err != nil {
		return err
	}
	for index := range routes {
		if routes[index].LinkIndex == link.Attrs().Index && routes[index].Dst != nil &&
			routes[index].Dst.String() == prefix.String() && int(routes[index].Protocol) == orchWireGuardRouteProtocol {
			if err := netlink.RouteDel(&routes[index]); err != nil && !errors.Is(err, unix.ESRCH) {
				return err
			}
		}
	}
	return nil
}

func (b *linuxWireGuardBackend) Close() error {
	if b.client == nil {
		return nil
	}
	err := b.client.Close()
	b.client = nil
	return err
}

func prefixIPNet(prefix netip.Prefix) net.IPNet {
	address := prefix.Addr().As4()
	return net.IPNet{IP: net.IPv4(address[0], address[1], address[2], address[3]), Mask: net.CIDRMask(prefix.Bits(), 32)}
}
