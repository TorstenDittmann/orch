//go:build linux

package network

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/TorstenDittmann/orch/internal/hostports"
	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const (
	nodePortPreroutingChain = "prerouting"
	nodePortForwardChain    = "published"
)

// addNodePortChains creates the empty DNAT base chain and the regular chain
// that the forward chain jumps to for published destinations.
func (f *firewall) addNodePortChains(connection *nftables.Conn, table *nftables.Table) (*nftables.Chain, *nftables.Chain) {
	prerouting := connection.AddChain(&nftables.Chain{
		Name: nodePortPreroutingChain, Table: table, Type: nftables.ChainTypeNAT,
		Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityNATDest,
	})
	published := connection.AddChain(&nftables.Chain{Name: nodePortForwardChain, Table: table})
	return prerouting, published
}

// addNodePortRules translates each published port into
//
//	prerouting: iifname != BRIDGE fib daddr type local meta l4proto P th dport PORT dnat to ADDRESS:TARGET
//	published:  ip daddr ADDRESS meta l4proto P th dport TARGET accept
//
// Only packets addressed to this node from outside the workload bridge are
// translated, so client source addresses survive and replies leave through
// conntrack from the address the client used.
func (f *firewall) addNodePortRules(connection *nftables.Conn, table *nftables.Table, prerouting, published *nftables.Chain, ports []NodePort) {
	bridge := append([]byte(f.bridge), 0)
	for _, port := range ports {
		protocol := protocolNumber(port.Protocol)
		address := port.Address.As4()
		connection.AddRule(&nftables.Rule{Table: table, Chain: prerouting, Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: bridge},
			&expr.Fib{Register: 1, ResultADDRTYPE: true, FlagDADDR: true},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(unix.RTN_LOCAL)},
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{protocol}},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(port.Port)},
			&expr.Immediate{Register: 1, Data: address[:]},
			&expr.Immediate{Register: 2, Data: binaryutil.BigEndian.PutUint16(port.TargetPort)},
			&expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1, RegProtoMin: 2, Specified: true},
		}})
		connection.AddRule(&nftables.Rule{Table: table, Chain: published, Exprs: []expr.Any{
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: address[:]},
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{protocol}},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(port.TargetPort)},
			&expr.Verdict{Kind: expr.VerdictAccept},
		}})
	}
}

// publish replaces the published node-port set. A port that a host process
// already binds is refused rather than silently redirected, which protects
// SSH, the controller API, and WireGuard from being shadowed.
func (f *firewall) publish(ports []NodePort) ([]error, error) {
	results := make([]error, len(ports))
	accepted := make([]NodePort, 0, len(ports))
	if f.hostOwned == nil {
		f.hostOwned = map[string]bool{}
		for _, port := range hostports.SSH() {
			f.hostOwned["tcp/"+strconv.Itoa(int(port))] = true
		}
	}
	for index, port := range ports {
		if f.hostOwned[port.Key()] {
			results[index] = fmt.Errorf("%s port %d belongs to a service on this node", strings.ToUpper(port.Protocol), port.Port)
			continue
		}
		if err := hostPortFree(port); err != nil {
			// Only a listener makes the port the host's. A port that is busy
			// as some connection's source port, or fails for another reason
			// such as EMFILE, is retried on the next publish.
			if errors.Is(err, syscall.EADDRINUSE) && hostports.Listening(port.Protocol, port.Port) {
				f.hostOwned[port.Key()] = true
			}
			results[index] = err
			continue
		}
		accepted = append(accepted, port)
	}
	if !f.prepared {
		return results, errors.New("the workload firewall has not been prepared")
	}
	if slices.Equal(accepted, f.published) {
		return results, f.flushUnflushed()
	}
	connection, err := nftables.New()
	if err != nil {
		return results, fmt.Errorf("open nftables: %w", err)
	}
	table := &nftables.Table{Name: f.tableName, Family: nftables.TableFamilyIPv4}
	prerouting := &nftables.Chain{Name: nodePortPreroutingChain, Table: table}
	published := &nftables.Chain{Name: nodePortForwardChain, Table: table}
	connection.FlushChain(prerouting)
	connection.FlushChain(published)
	f.addNodePortRules(connection, table, prerouting, published, accepted)
	if err := connection.Flush(); err != nil {
		return results, fmt.Errorf("install node port rules: %w", err)
	}
	previous := f.published
	f.published = accepted
	// NAT is decided on a flow's first packet. Long-lived UDP flows would
	// otherwise keep reaching the old allocation, or the host itself, after
	// the mapping changes, so every added, changed, or removed port starts over.
	f.unflushed = append(f.unflushed, changedNodePorts(previous, accepted)...)
	return results, f.flushUnflushed()
}

// flushUnflushed deletes conntrack entries for every port still owed a flush.
// Failed ports stay queued so the next publish retries them even when the
// published set is unchanged.
func (f *firewall) flushUnflushed() error {
	var failed []NodePort
	var errs []error
	for _, port := range f.unflushed {
		if err := flushConntrack(port); err != nil {
			failed = append(failed, port)
			errs = append(errs, err)
		}
	}
	f.unflushed = failed
	return errors.Join(errs...)
}

func changedNodePorts(previous, current []NodePort) []NodePort {
	before := make(map[string]NodePort, len(previous))
	for _, port := range previous {
		before[port.Key()] = port
	}
	var changed []NodePort
	for _, port := range current {
		if old, exists := before[port.Key()]; !exists || old != port {
			changed = append(changed, port)
		}
		delete(before, port.Key())
	}
	for _, port := range previous {
		if _, removed := before[port.Key()]; removed {
			changed = append(changed, port)
		}
	}
	return changed
}

func hostPortFree(port NodePort) error {
	address := net.JoinHostPort("0.0.0.0", strconv.Itoa(int(port.Port)))
	var err error
	if port.Protocol == "udp" {
		var listener net.PacketConn
		if listener, err = net.ListenPacket("udp4", address); err == nil {
			return listener.Close()
		}
	} else {
		var listener net.Listener
		if listener, err = net.Listen("tcp4", address); err == nil {
			return listener.Close()
		}
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("%s port %d is already bound on this node: %w", strings.ToUpper(port.Protocol), port.Port, syscall.EADDRINUSE)
	}
	return fmt.Errorf("check %s port %d on this node: %w", strings.ToUpper(port.Protocol), port.Port, err)
}

func flushConntrack(port NodePort) error {
	filter := &netlink.ConntrackFilter{}
	if err := filter.AddProtocol(protocolNumber(port.Protocol)); err != nil {
		return err
	}
	if err := filter.AddPort(netlink.ConntrackOrigDstPort, port.Port); err != nil {
		return err
	}
	if _, err := netlink.ConntrackDeleteFilters(netlink.ConntrackTable, unix.AF_INET, filter); err != nil {
		return fmt.Errorf("flush conntrack entries for node port %s: %w", port.Key(), err)
	}
	return nil
}

func protocolNumber(protocol string) uint8 {
	if protocol == "udp" {
		return unix.IPPROTO_UDP
	}
	return unix.IPPROTO_TCP
}
