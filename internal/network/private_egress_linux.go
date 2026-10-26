package network

import (
	"encoding/binary"
	"fmt"
	"maps"
	"net"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
)

const privateEgressSet = "private_egress"

// privateRanges are the networks workloads cannot reach unless their
// application sets allowPrivateNetwork: the private and shared address space
// that VPCs, LANs and carrier networks use. The cluster's own workload
// network is always reachable.
var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
}

// addPrivateEgressRules refuses new connections from workloads to private
// networks, except from workloads whose address is in the private_egress set.
// They run before the rule that accepts everything else from the bridge.
func (f *firewall) addPrivateEgressRules(connection *nftables.Conn, table *nftables.Table, forward *nftables.Chain, cluster netip.Prefix) error {
	interfaceName := append([]byte(f.bridge), 0)
	fromBridge := []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: interfaceName},
	}
	rule := func(exprs ...expr.Any) {
		connection.AddRule(&nftables.Rule{Table: table, Chain: forward, Exprs: append(append([]expr.Any(nil), fromBridge...), exprs...)})
	}
	set := &nftables.Set{Table: table, Name: privateEgressSet, KeyType: nftables.TypeIPAddr}
	if err := connection.AddSet(set, privateEgressElements(f.privateEgress)); err != nil {
		return fmt.Errorf("add private egress set: %w", err)
	}
	// Replies, such as those to a LAN client of a node port, are not new
	// connections from the workload.
	states := expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED
	rule(
		&expr.Ct{Key: expr.CtKeySTATE, Register: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4,
			Mask: binaryutil.NativeEndian.PutUint32(states), Xor: binaryutil.NativeEndian.PutUint32(0)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0, 0, 0, 0}},
		&expr.Verdict{Kind: expr.VerdictAccept},
	)
	rule(append(destinationIn(cluster), &expr.Verdict{Kind: expr.VerdictAccept})...)
	rule(
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
		&expr.Lookup{SourceRegister: 1, SetName: set.Name, SetID: set.ID},
		&expr.Verdict{Kind: expr.VerdictAccept},
	)
	for _, prefix := range privateRanges {
		rule(append(destinationIn(prefix), rejectAdministratively())...)
	}
	return nil
}

func destinationIn(prefix netip.Prefix) []expr.Any {
	mask := binary.BigEndian.Uint32(net.CIDRMask(prefix.Bits(), 32))
	address := prefix.Masked().Addr().As4()
	maskBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(maskBytes, mask)
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: maskBytes, Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: address[:]},
	}
}

// allowPrivateEgress lets the workload at address, attached as objectID,
// reach private networks.
func (f *firewall) allowPrivateEgress(objectID string, address netip.Addr) error {
	if !address.Is4() {
		return fmt.Errorf("private network access needs an IPv4 workload address")
	}
	if current, ok := f.privateEgress[objectID]; ok && current == address {
		return nil
	}
	next := maps.Clone(f.privateEgress)
	if next == nil {
		next = map[string]netip.Addr{}
	}
	next[objectID] = address
	return f.syncPrivateEgress(next)
}

// revokePrivateEgress forgets objectID's private network access.
func (f *firewall) revokePrivateEgress(objectID string) error {
	if _, ok := f.privateEgress[objectID]; !ok {
		return nil
	}
	next := maps.Clone(f.privateEgress)
	delete(next, objectID)
	return f.syncPrivateEgress(next)
}

// retainPrivateEgress forgets every object not in live.
func (f *firewall) retainPrivateEgress(live map[string]struct{}) error {
	next := maps.Clone(f.privateEgress)
	maps.DeleteFunc(next, func(objectID string, _ netip.Addr) bool {
		_, ok := live[objectID]
		return !ok
	})
	if len(next) == len(f.privateEgress) {
		return nil
	}
	return f.syncPrivateEgress(next)
}

// syncPrivateEgress replaces the set's contents with next and records next
// only once the kernel has it, so a failed update is retried by the next
// attach, check or detach. Before the table exists, prepare fills the set
// instead.
func (f *firewall) syncPrivateEgress(next map[string]netip.Addr) error {
	if !f.prepared {
		f.privateEgress = next
		return nil
	}
	connection, err := nftables.New()
	if err != nil {
		return fmt.Errorf("open nftables: %w", err)
	}
	table := &nftables.Table{Name: f.tableName, Family: nftables.TableFamilyIPv4}
	set := &nftables.Set{Table: table, Name: privateEgressSet, KeyType: nftables.TypeIPAddr}
	connection.FlushSet(set)
	if elements := privateEgressElements(next); len(elements) != 0 {
		if err := connection.SetAddElements(set, elements); err != nil {
			return fmt.Errorf("update private egress set: %w", err)
		}
	}
	if err := connection.Flush(); err != nil {
		return fmt.Errorf("update private egress set: %w", err)
	}
	f.privateEgress = next
	return nil
}

func privateEgressElements(grants map[string]netip.Addr) []nftables.SetElement {
	elements := make([]nftables.SetElement, 0, len(grants))
	seen := map[netip.Addr]bool{}
	for _, address := range grants {
		if seen[address] {
			continue
		}
		seen[address] = true
		key := address.As4()
		elements = append(elements, nftables.SetElement{Key: key[:]})
	}
	return elements
}
