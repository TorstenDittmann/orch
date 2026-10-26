//go:build linux

package network

import (
	"net"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// TestWorkloadFirewallLimitsHostAndMetadataAccess runs against the real
// kernel. Run it as root in a throwaway network namespace:
//
//	ORCH_FIREWALL_E2E=1 unshare -n go test -run TestWorkloadFirewall ./internal/network
func TestWorkloadFirewallLimitsHostAndMetadataAccess(t *testing.T) {
	if os.Getenv("ORCH_FIREWALL_E2E") != "1" {
		t.Skip("set ORCH_FIREWALL_E2E=1 and run as root under unshare -n")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	host, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	lo, err := netlink.LinkByName("lo")
	must(err)
	must(netlink.LinkSetUp(lo))

	bridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "orchfwtest"}}
	must(netlink.LinkAdd(bridge))
	must(netlink.AddrAdd(bridge, mustAddr(t, "10.199.0.1/24")))
	must(netlink.LinkSetUp(bridge))

	// The container sits on the bridge; outside is a routed neighbour that
	// answers on a private address and on the cloud metadata address.
	container := newNamespace(t, host)
	outside := newNamespace(t, host)
	attach := func(name string, peerNS netns.NsHandle, master netlink.Link, hostAddrs, peerAddrs []string, gateway string) {
		veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: name}, PeerName: name + "p"}
		must(netlink.LinkAdd(veth))
		if master != nil {
			must(netlink.LinkSetMaster(veth, master))
		}
		for _, address := range hostAddrs {
			must(netlink.AddrAdd(veth, mustAddr(t, address)))
		}
		must(netlink.LinkSetUp(veth))
		peer, err := netlink.LinkByName(name + "p")
		must(err)
		must(netlink.LinkSetNsFd(peer, int(peerNS)))
		inNamespace(t, peerNS, host, func() {
			peer, err := netlink.LinkByName(name + "p")
			must(err)
			for _, address := range peerAddrs {
				must(netlink.AddrAdd(peer, mustAddr(t, address)))
			}
			must(netlink.LinkSetUp(peer))
			lo, err := netlink.LinkByName("lo")
			must(err)
			must(netlink.LinkSetUp(lo))
			if gateway != "" {
				must(netlink.RouteAdd(&netlink.Route{Gw: net.ParseIP(gateway)}))
			}
		})
	}
	attach("orchfwc", container, bridge, nil, []string{"10.199.0.2/24"}, "10.199.0.1")
	attach("orchfwo", outside, nil, []string{"10.200.0.1/24", "169.254.0.1/16", "198.51.100.1/24"},
		[]string{"10.200.0.2/24", "169.254.169.254/16", "198.51.100.2/24"}, "10.200.0.1")
	// remote stands in for a workload on another node: its traffic arrives on
	// the WireGuard interface from that node's subnet.
	remote := newNamespace(t, host)
	attach("orchfwwg", remote, nil, []string{"10.199.1.1/24"}, []string{"10.199.1.2/24"}, "10.199.1.1")

	for _, port := range []int{22, 80, 5432} {
		listen(t, "0.0.0.0:"+strconv.Itoa(port))
	}
	listen(t, "10.199.0.1:53")
	inNamespace(t, outside, host, func() { listen(t, "0.0.0.0:8080") })

	f := &firewall{stateDir: t.TempDir(), tableName: "orch_fwtest", bridge: bridge.Name, wireguard: "orchfwwg"}
	must(f.prepare(model.NetworkConfig{ClusterCIDR: "10.199.0.0/16", NodeSubnet: "10.199.0.0/24", Gateway: "10.199.0.1"}))

	expectFrom := func(source netns.NsHandle, address string, open bool) {
		t.Helper()
		var err error
		inNamespace(t, source, host, func() {
			var connection net.Conn
			connection, err = net.DialTimeout("tcp", address, 2*time.Second)
			if err == nil {
				_ = connection.Close()
			}
		})
		if open && err != nil {
			t.Fatalf("%s from a workload: %v, want reachable", address, err)
		}
		if !open && err == nil {
			t.Fatalf("%s from a workload is reachable, want blocked", address)
		}
	}
	expect := func(address string, open bool) {
		t.Helper()
		expectFrom(container, address, open)
	}
	expect("10.199.0.1:53", true)
	expect("10.199.0.1:80", true)
	expect("10.199.0.1:22", false)
	expect("10.199.0.1:5432", false)
	expect("198.51.100.2:8080", true)
	expect("10.200.0.2:8080", false)
	expect("169.254.169.254:8080", false)
	// Workloads on other nodes reach this node's workloads but not its host.
	inNamespace(t, container, host, func() { listen(t, "10.199.0.2:9000") })
	expectFrom(remote, "10.199.0.2:9000", true)
	expectFrom(remote, "10.199.0.1:22", false)
	expectFrom(remote, "10.199.0.1:5432", false)

	must(f.allowIngress([]uint16{5432}))
	expect("10.199.0.1:5432", true)
	expect("10.199.0.1:22", false)
	must(f.allowIngress(nil))
	expect("10.199.0.1:5432", false)

	// An application that sets allowPrivateNetwork reaches private networks,
	// also after the table is rebuilt, until its object is detached.
	must(f.allowPrivateEgress("object-1", netip.MustParseAddr("10.199.0.2")))
	expect("10.200.0.2:8080", true)
	expect("10.199.0.1:22", false)
	expect("169.254.169.254:8080", false)
	must(f.prepare(model.NetworkConfig{ClusterCIDR: "10.199.0.0/16", NodeSubnet: "10.199.0.0/24", Gateway: "10.199.0.1"}))
	expect("10.200.0.2:8080", true)
	must(f.revokePrivateEgress("object-1"))
	expect("10.200.0.2:8080", false)
	expect("198.51.100.2:8080", true)
}

func mustAddr(t *testing.T, value string) *netlink.Addr {
	t.Helper()
	address, err := netlink.ParseAddr(value)
	if err != nil {
		t.Fatal(err)
	}
	return address
}

func newNamespace(t *testing.T, host netns.NsHandle) netns.NsHandle {
	t.Helper()
	created, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := netns.Set(host); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = created.Close() })
	return created
}

func inNamespace(t *testing.T, target, host netns.NsHandle, run func()) {
	t.Helper()
	if err := netns.Set(target); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := netns.Set(host); err != nil {
			t.Fatal(err)
		}
	}()
	run()
}

func listen(t *testing.T, address string) {
	t.Helper()
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			_ = connection.Close()
		}
	}()
}
