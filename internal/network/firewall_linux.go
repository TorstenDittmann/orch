//go:build linux

package network

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"

	"change.codes/torsten/better-swarm/internal/model"
	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
)

type firewall struct {
	stateDir  string
	tableName string
	bridge    string
	wireguard string
	// prepared is set once the table exists; published is the last accepted
	// node-port set and is reinstalled whenever prepare rebuilds the table.
	prepared  bool
	published []NodePort
}

type firewallOwner struct {
	Table  string `json:"table"`
	Bridge string `json:"bridge"`
}

func (f *firewall) preflight() error {
	connection, err := nftables.New()
	if err != nil {
		return fmt.Errorf("open nftables: %w", err)
	}
	_, err = connection.ListTables()
	if err != nil {
		return fmt.Errorf("list nftables tables: %w", err)
	}
	return nil
}

func (f *firewall) prepare(config model.NetworkConfig) error {
	connection, err := nftables.New()
	if err != nil {
		return fmt.Errorf("open nftables: %w", err)
	}
	tables, err := connection.ListTables()
	if err != nil {
		return fmt.Errorf("list nftables tables: %w", err)
	}
	markerPath := filepath.Join(f.stateDir, "firewall-owner.json")
	marker, markerErr := readFirewallOwner(markerPath)
	var existing *nftables.Table
	for _, table := range tables {
		if table.Family == nftables.TableFamilyIPv4 && table.Name == f.tableName {
			existing = table
			break
		}
	}
	if existing != nil && markerErr != nil {
		return fmt.Errorf("nftables table %q exists without an orch ownership marker", f.tableName)
	}
	if markerErr == nil && (marker.Table != f.tableName || marker.Bridge != f.bridge) {
		return ErrOwnershipMismatch
	}
	if existing == nil && os.IsNotExist(markerErr) {
		if err := writeFirewallOwner(markerPath, firewallOwner{Table: f.tableName, Bridge: f.bridge}); err != nil {
			return err
		}
	} else if markerErr != nil && !os.IsNotExist(markerErr) {
		return markerErr
	}
	if existing != nil {
		connection.DelTable(existing)
	}
	table := connection.AddTable(&nftables.Table{Name: f.tableName, Family: nftables.TableFamilyIPv4})
	forward := connection.AddChain(&nftables.Chain{
		Name: "forward", Table: table, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter,
	})
	postrouting := connection.AddChain(&nftables.Chain{
		Name: "postrouting", Table: table, Type: nftables.ChainTypeNAT,
		Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource,
	})
	prerouting, published := f.addNodePortChains(connection, table)
	interfaceName := append([]byte(f.bridge), 0)
	connection.AddRule(&nftables.Rule{Table: table, Chain: forward, Exprs: []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: interfaceName},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}})
	// WireGuard cryptographically authenticates peers and enforces their
	// configured source prefixes. Permit those routed packets onto the local
	// workload bridge without opening any foreign host interface.
	connection.AddRule(&nftables.Rule{Table: table, Chain: forward, Exprs: []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: append([]byte(f.wireguard), 0)},
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: interfaceName},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}})
	states := expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED
	connection.AddRule(&nftables.Rule{Table: table, Chain: forward, Exprs: []expr.Any{
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: interfaceName},
		&expr.Ct{Key: expr.CtKeySTATE, Register: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4,
			Mask: binaryutil.NativeEndian.PutUint32(states), Xor: binaryutil.NativeEndian.PutUint32(0)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0, 0, 0, 0}},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}})
	// Published node ports were DNATed in prerouting; admit exactly those
	// destinations before the default reject below.
	connection.AddRule(&nftables.Rule{Table: table, Chain: forward, Exprs: []expr.Any{
		&expr.Verdict{Kind: expr.VerdictJump, Chain: published.Name},
	}})
	// Reject only new forwarded connections whose destination is the orch
	// bridge. The chain otherwise falls through, so foreign forwarding policy
	// remains untouched.
	connection.AddRule(&nftables.Rule{Table: table, Chain: forward, Exprs: []expr.Any{
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: interfaceName},
		&expr.Verdict{Kind: expr.VerdictDrop},
	}})
	cluster := netip.MustParsePrefix(config.ClusterCIDR)
	clusterAddress := cluster.Addr().As4()
	clusterMask := binary.BigEndian.Uint32(net.CIDRMask(cluster.Bits(), 32))
	maskBytes, clusterBytes := make([]byte, 4), make([]byte, 4)
	binary.BigEndian.PutUint32(maskBytes, clusterMask)
	binary.BigEndian.PutUint32(clusterBytes, binary.BigEndian.Uint32(clusterAddress[:])&clusterMask)
	// Cluster traffic must retain allocation source addresses across the mesh.
	connection.AddRule(&nftables.Rule{Table: table, Chain: postrouting, Exprs: []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: maskBytes, Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: clusterBytes},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}})
	subnet := netip.MustParsePrefix(config.NodeSubnet)
	address := subnet.Addr().As4()
	prefixBytes := (subnet.Bits() + 7) / 8
	connection.AddRule(&nftables.Rule{Table: table, Chain: postrouting, Exprs: []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: uint32(prefixBytes)},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: address[:prefixBytes]},
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: interfaceName},
		&expr.Masq{},
	}})
	f.addNodePortRules(connection, table, prerouting, published, f.published)
	if err := connection.Flush(); err != nil {
		return fmt.Errorf("install orch nftables rules: %w", err)
	}
	f.prepared = true
	return nil
}

func shortIdentity(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:6])
}

func readFirewallOwner(path string) (firewallOwner, error) {
	contents, err := readRootOwnedStateFile(path, 4<<10)
	if err != nil {
		return firewallOwner{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var owner firewallOwner
	if err := decoder.Decode(&owner); err != nil {
		return firewallOwner{}, fmt.Errorf("decode firewall ownership marker: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF || owner.Table == "" || owner.Bridge == "" {
		return firewallOwner{}, fmt.Errorf("firewall ownership marker is incomplete")
	}
	return owner, nil
}

func writeFirewallOwner(path string, owner firewallOwner) error {
	encoded, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create firewall ownership marker: %w", err)
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return syncNetworkDirectory(filepath.Dir(path))
}
