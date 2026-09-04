//go:build linux

package network

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"

	"change.codes/torsten/better-swarm/internal/model"
	"github.com/vishvananda/netlink"
)

const bridgeOwnerFilename = "bridge-owner.json"

type bridgeManager struct {
	stateDir string
	name     string
	alias    string
}

type bridgeOwner struct {
	Name              string `json:"name"`
	Alias             string `json:"alias"`
	NetworkGeneration uint64 `json:"network_generation"`
	ClusterCIDR       string `json:"cluster_cidr"`
	NodeSubnet        string `json:"node_subnet"`
	Gateway           string `json:"gateway"`
}

func (b *bridgeManager) prepare(config model.NetworkConfig) error {
	if err := model.ValidateNodeNetwork(config); err != nil {
		return fmt.Errorf("bridge network configuration is invalid: %w", err)
	}
	want := bridgeOwner{
		Name: b.name, Alias: b.alias, NetworkGeneration: config.Generation,
		ClusterCIDR: config.ClusterCIDR, NodeSubnet: config.NodeSubnet, Gateway: config.Gateway,
	}
	markerPath := filepath.Join(b.stateDir, bridgeOwnerFilename)
	owner, markerErr := readBridgeOwner(markerPath)
	link, linkErr := netlink.LinkByName(b.name)
	var notFound netlink.LinkNotFoundError
	linkMissing := errors.As(linkErr, &notFound)
	if linkErr != nil && !linkMissing {
		return fmt.Errorf("inspect workload bridge %q: %w", b.name, linkErr)
	}

	if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
		return markerErr
	}
	if markerErr == nil && (owner.Name != b.name || owner.Alias != b.alias) {
		return fmt.Errorf("workload bridge ownership marker does not match: %w", ErrOwnershipMismatch)
	}
	if !linkMissing && errors.Is(markerErr, os.ErrNotExist) {
		return fmt.Errorf("workload bridge %q exists without an orch ownership marker: %w", b.name, ErrOwnershipMismatch)
	}
	if errors.Is(markerErr, os.ErrNotExist) {
		if err := createBridgeOwner(markerPath, want); err != nil {
			return err
		}
		owner = want
	}
	if linkMissing {
		candidate := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: b.name, Alias: b.alias}}
		if err := netlink.LinkAdd(candidate); err != nil {
			return fmt.Errorf("create workload bridge %q: %w", b.name, err)
		}
		link, linkErr = netlink.LinkByName(b.name)
		if linkErr != nil {
			return fmt.Errorf("inspect created workload bridge %q: %w", b.name, linkErr)
		}
	}
	if link.Type() != "bridge" {
		return fmt.Errorf("workload link %q is not a bridge: %w", b.name, ErrOwnershipMismatch)
	}
	if alias := link.Attrs().Alias; alias != "" && alias != b.alias {
		return fmt.Errorf("workload bridge %q has a foreign alias: %w", b.name, ErrOwnershipMismatch)
	}
	if link.Attrs().Alias != b.alias {
		if err := netlink.LinkSetAlias(link, b.alias); err != nil {
			return fmt.Errorf("label workload bridge %q: %w", b.name, err)
		}
	}

	wantedHardwareAddress := bridgeHardwareAddress(b.alias)
	addressChanged := !bytes.Equal(link.Attrs().HardwareAddr, wantedHardwareAddress)
	if owner != want || addressChanged {
		attached, err := bridgeHasAttachedLinks(link.Attrs().Index)
		if err != nil {
			return err
		}
		if attached {
			return fmt.Errorf("refusing to reconfigure workload bridge identity with attached interfaces: %w", ErrOwnershipMismatch)
		}
	}
	if addressChanged {
		if err := netlink.LinkSetHardwareAddr(link, wantedHardwareAddress); err != nil {
			return fmt.Errorf("set stable workload bridge MAC: %w", err)
		}
	}
	if err := configureBridgeAddress(link, owner, want); err != nil {
		return err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring workload bridge %q up: %w", b.name, err)
	}
	if owner != want {
		if err := replaceBridgeOwner(markerPath, want); err != nil {
			return err
		}
	}
	return nil
}

// bridgeHardwareAddress keeps the bridge identity stable as veth members join
// while avoiding collision with globally assigned hardware addresses.
func bridgeHardwareAddress(identity string) net.HardwareAddr {
	digest := sha256.Sum256([]byte(identity))
	address := net.HardwareAddr(append([]byte(nil), digest[:6]...))
	address[0] = address[0]&0xfe | 0x02
	return address
}

func bridgeHasAttachedLinks(bridgeIndex int) (bool, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return false, fmt.Errorf("list links attached to workload bridge: %w", err)
	}
	for _, link := range links {
		if link.Attrs().MasterIndex == bridgeIndex {
			return true, nil
		}
	}
	return false, nil
}

func configureBridgeAddress(link netlink.Link, current, desired bridgeOwner) error {
	nodeSubnet := netip.MustParsePrefix(desired.NodeSubnet)
	wanted, err := netlink.ParseAddr(desired.Gateway + "/" + fmt.Sprintf("%d", nodeSubnet.Bits()))
	if err != nil {
		return fmt.Errorf("parse workload bridge gateway: %w", err)
	}
	addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list workload bridge addresses: %w", err)
	}
	var previous *netlink.Addr
	if current.NodeSubnet != "" && current.Gateway != "" {
		prefix, parseErr := netip.ParsePrefix(current.NodeSubnet)
		if parseErr != nil {
			return fmt.Errorf("workload bridge ownership marker has an invalid subnet")
		}
		previous, err = netlink.ParseAddr(current.Gateway + "/" + fmt.Sprintf("%d", prefix.Bits()))
		if err != nil {
			return fmt.Errorf("workload bridge ownership marker has an invalid gateway")
		}
	}
	found := false
	for index := range addresses {
		address := addresses[index]
		if address.Equal(*wanted) {
			found = true
			continue
		}
		if previous != nil && address.Equal(*previous) {
			continue
		}
		return fmt.Errorf("workload bridge %q has an unowned IPv4 address %s: %w",
			link.Attrs().Name, address.IPNet, ErrOwnershipMismatch)
	}
	if !found {
		if err := netlink.AddrReplace(link, wanted); err != nil {
			return fmt.Errorf("configure workload bridge gateway: %w", err)
		}
	}
	if previous != nil && !previous.Equal(*wanted) {
		for index := range addresses {
			if addresses[index].Equal(*previous) {
				if err := netlink.AddrDel(link, &addresses[index]); err != nil {
					return fmt.Errorf("remove prior workload bridge gateway: %w", err)
				}
			}
		}
	}
	return nil
}

func readBridgeOwner(path string) (bridgeOwner, error) {
	contents, err := readRootOwnedStateFile(path, 4<<10)
	if err != nil {
		return bridgeOwner{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var owner bridgeOwner
	if err := decoder.Decode(&owner); err != nil {
		return bridgeOwner{}, fmt.Errorf("decode workload bridge ownership marker: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF || owner.Name == "" || owner.Alias == "" ||
		owner.NetworkGeneration == 0 || owner.ClusterCIDR == "" || owner.NodeSubnet == "" || owner.Gateway == "" {
		return bridgeOwner{}, fmt.Errorf("workload bridge ownership marker is incomplete")
	}
	return owner, nil
}

func readRootOwnedStateFile(path string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maximum || !ok || stat.Uid != 0 {
		return nil, fmt.Errorf("network ownership marker %q is unsafe", filepath.Base(path))
	}
	contents, err := os.ReadFile(path)
	if err != nil || int64(len(contents)) > maximum {
		return nil, fmt.Errorf("read bounded network ownership marker %q", filepath.Base(path))
	}
	return contents, nil
}

func createBridgeOwner(path string, owner bridgeOwner) error {
	encoded, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create workload bridge ownership marker: %w", err)
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

func replaceBridgeOwner(path string, owner bridgeOwner) error {
	encoded, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".bridge-owner-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncNetworkDirectory(filepath.Dir(path))
}

func syncNetworkDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
