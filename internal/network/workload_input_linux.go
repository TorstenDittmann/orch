//go:build linux

package network

import (
	"fmt"
	"net"
	"net/netip"
	"slices"

	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

const (
	workloadInputChain = "workload_input"
	ingressPortsChain  = "ingress_ports"
	// icmpAdminProhibited is ICMP destination unreachable code 13.
	icmpAdminProhibited = 13
)

// addWorkloadInputChains limits what workloads reach on the host itself:
//
//	input:          iifname BRIDGE jump workload_input
//	                iifname WIREGUARD ip saddr CLUSTER jump workload_input
//	workload_input: ct state established,related accept
//	                meta l4proto icmp accept
//	                ip daddr GATEWAY udp/tcp dport 53 accept
//	                tcp dport { 80, 443 } accept
//	                jump ingress_ports
//	                reject with icmp admin-prohibited
//
// Without it a container could reach SSH, the controller API, and any host
// service the provider firewall hides from the internet. Workloads on other
// nodes arrive over WireGuard and reach this node's gateway address the same
// way, so they get the same limits. HTTP and raw TCP
// ingress stay reachable so an application can call its own public domain.
// The returned chain holds one accept per raw TCP route port.
func (f *firewall) addWorkloadInputChains(connection *nftables.Conn, table *nftables.Table, config model.NetworkConfig) *nftables.Chain {
	input := connection.AddChain(&nftables.Chain{
		Name: "input", Table: table, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookInput, Priority: nftables.ChainPriorityFilter,
	})
	workload := connection.AddChain(&nftables.Chain{Name: workloadInputChain, Table: table})
	ingress := connection.AddChain(&nftables.Chain{Name: ingressPortsChain, Table: table})
	connection.AddRule(&nftables.Rule{Table: table, Chain: input, Exprs: []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: append([]byte(f.bridge), 0)},
		&expr.Verdict{Kind: expr.VerdictJump, Chain: workload.Name},
	}})
	cluster := netip.MustParsePrefix(config.ClusterCIDR).Masked()
	clusterAddress := cluster.Addr().As4()
	clusterMask := net.CIDRMask(cluster.Bits(), 32)
	connection.AddRule(&nftables.Rule{Table: table, Chain: input, Exprs: []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: append([]byte(f.wireguard), 0)},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: clusterMask, Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: clusterAddress[:]},
		&expr.Verdict{Kind: expr.VerdictJump, Chain: workload.Name},
	}})
	states := expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED
	connection.AddRule(&nftables.Rule{Table: table, Chain: workload, Exprs: []expr.Any{
		&expr.Ct{Key: expr.CtKeySTATE, Register: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4,
			Mask: binaryutil.NativeEndian.PutUint32(states), Xor: binaryutil.NativeEndian.PutUint32(0)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0, 0, 0, 0}},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}})
	connection.AddRule(&nftables.Rule{Table: table, Chain: workload, Exprs: []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_ICMP}},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}})
	gateway := netip.MustParseAddr(config.Gateway).As4()
	for _, protocol := range []byte{unix.IPPROTO_UDP, unix.IPPROTO_TCP} {
		connection.AddRule(&nftables.Rule{Table: table, Chain: workload, Exprs: []expr.Any{
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: gateway[:]},
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{protocol}},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(53)},
			&expr.Verdict{Kind: expr.VerdictAccept},
		}})
	}
	for _, port := range []uint16{80, 443} {
		connection.AddRule(&nftables.Rule{Table: table, Chain: workload, Exprs: tcpPortAccept(port)})
	}
	connection.AddRule(&nftables.Rule{Table: table, Chain: workload, Exprs: []expr.Any{
		&expr.Verdict{Kind: expr.VerdictJump, Chain: ingress.Name},
	}})
	connection.AddRule(&nftables.Rule{Table: table, Chain: workload, Exprs: []expr.Any{rejectAdministratively()}})
	return ingress
}

func (f *firewall) addIngressPortRules(connection *nftables.Conn, table *nftables.Table, ingress *nftables.Chain, ports []uint16) {
	for _, port := range ports {
		connection.AddRule(&nftables.Rule{Table: table, Chain: ingress, Exprs: tcpPortAccept(port)})
	}
}

// allowIngress replaces the raw TCP route ports workloads may reach on this
// node. Before the table exists it only records them for prepare.
func (f *firewall) allowIngress(ports []uint16) error {
	ports = slices.Clone(ports)
	slices.Sort(ports)
	ports = slices.Compact(ports)
	if f.prepared && slices.Equal(ports, f.ingressPorts) {
		return nil
	}
	if !f.prepared {
		f.ingressPorts = ports
		return nil
	}
	connection, err := nftables.New()
	if err != nil {
		return fmt.Errorf("open nftables: %w", err)
	}
	table := &nftables.Table{Name: f.tableName, Family: nftables.TableFamilyIPv4}
	ingress := &nftables.Chain{Name: ingressPortsChain, Table: table}
	connection.FlushChain(ingress)
	f.addIngressPortRules(connection, table, ingress, ports)
	if err := connection.Flush(); err != nil {
		return fmt.Errorf("install ingress port rules: %w", err)
	}
	f.ingressPorts = ports
	return nil
}

func tcpPortAccept(port uint16) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(port)},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

// rejectAdministratively answers with ICMP administratively prohibited, so a
// blocked connection fails at once instead of timing out.
func rejectAdministratively() *expr.Reject {
	return &expr.Reject{Type: unix.NFT_REJECT_ICMP_UNREACH, Code: icmpAdminProhibited}
}
