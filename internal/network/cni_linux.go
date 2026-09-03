//go:build linux

package network

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"change.codes/torsten/better-swarm/internal/model"
	"github.com/containernetworking/cni/libcni"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	types100 "github.com/containernetworking/cni/pkg/types/100"
)

const (
	cniSpecVersion    = CNISpecVersion
	cniPluginsVersion = CNIPluginsVersion
)

var netNSPattern = regexp.MustCompile(`^/proc/[1-9][0-9]*/ns/net$`)

type linuxManager struct {
	mu             sync.Mutex
	options        Options
	cni            *libcni.CNIConfig
	list           *libcni.NetworkConfigList
	loopback       *libcni.NetworkConfig
	config         model.NetworkConfig
	name           string
	bridge         string
	bridgeManager  *bridgeManager
	firewall       *firewall
	preflightReady bool
}

func New(options Options) (Manager, error) {
	if err := ValidateOptions(options); err != nil {
		return nil, err
	}
	for _, path := range []*string{&options.BinDir, &options.StateDir} {
		absolute, err := filepath.Abs(*path)
		if err != nil {
			return nil, err
		}
		*path = absolute
	}
	if err := ensurePrivateDirectory(options.StateDir); err != nil {
		return nil, err
	}
	digest := shortIdentity(options.ClusterID + "\x00" + options.NodeID)
	name := "orch-" + digest
	bridge := "orch" + digest[:11]
	cacheDir := filepath.Join(options.StateDir, "cni-cache")
	if err := ensurePrivateDirectory(cacheDir); err != nil {
		return nil, err
	}
	return &linuxManager{
		options: options, cni: libcni.NewCNIConfigWithCacheDir([]string{options.BinDir}, cacheDir, nil),
		name: name, bridge: bridge,
		bridgeManager: &bridgeManager{
			stateDir: options.StateDir, name: bridge, alias: "orch-managed-" + digest,
		},
		firewall: &firewall{stateDir: options.StateDir, tableName: "orch_" + digest, bridge: bridge,
			wireguard: WireGuardInterfaceName(options.ClusterID, options.NodeID)},
	}, nil
}

func (m *linuxManager) Preflight(ctx context.Context) (Capabilities, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.preflightReady = false
	if runtime.GOOS != "linux" {
		return Capabilities{}, ErrUnsupported
	}
	binDirectory, err := os.Lstat(m.options.BinDir)
	if err != nil || !binDirectory.IsDir() || binDirectory.Mode().Perm()&0o022 != 0 {
		return Capabilities{}, fmt.Errorf("CNI binary directory must be a root-owned non-symlink directory that is not group/world writable")
	}
	binStat, ok := binDirectory.Sys().(*syscall.Stat_t)
	if !ok || binStat.Uid != 0 {
		return Capabilities{}, fmt.Errorf("CNI binary directory must be owned by root")
	}
	resolvedBinDirectory, err := filepath.EvalSymlinks(m.options.BinDir)
	if err != nil || resolvedBinDirectory != m.options.BinDir {
		return Capabilities{}, fmt.Errorf("CNI binary directory path must not traverse symbolic links")
	}
	for _, name := range []string{"loopback", "bridge", "host-local"} {
		path := filepath.Join(m.options.BinDir, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 {
			return Capabilities{}, fmt.Errorf("CNI plugin %q must be a root-owned executable regular file that is not group/world writable", name)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return Capabilities{}, fmt.Errorf("CNI plugin %q must be owned by root", name)
		}
		versionOutput, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
		if err != nil || !strings.Contains(string(versionOutput), cniPluginsVersion) {
			return Capabilities{}, fmt.Errorf("CNI plugin %q must be from the official plugins %s release", name, cniPluginsVersion)
		}
		versionInfo, err := m.cni.GetVersionInfo(ctx, name)
		if err != nil {
			return Capabilities{}, fmt.Errorf("query CNI plugin %q version: %w", name, err)
		}
		supported := false
		for _, version := range versionInfo.SupportedVersions() {
			if version == cniSpecVersion {
				supported = true
				break
			}
		}
		if !supported {
			return Capabilities{}, fmt.Errorf("CNI plugin %q does not support spec %s", name, cniSpecVersion)
		}
	}
	if value, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err != nil || strings.TrimSpace(string(value)) != "1" {
		return Capabilities{}, fmt.Errorf("net.ipv4.ip_forward=1 is required")
	}
	if err := m.firewall.preflight(); err != nil {
		return Capabilities{}, err
	}
	m.preflightReady = true
	return Capabilities{CNIVersion: cniSpecVersion, CNIPluginsVersion: cniPluginsVersion, Nftables: true}, nil
}

func (m *linuxManager) Prepare(ctx context.Context, desired model.NetworkConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateDesired(desired); err != nil {
		return err
	}
	if m.list != nil && m.loopback != nil && model.NetworkConfigEqual(m.config, desired) {
		return nil
	}
	// DNS must bind the gateway before the controller can freeze the network
	// and publish the first networked allocation. CNI normally creates the
	// bridge during ADD, which would deadlock that preflight sequence, so create
	// and own the same deterministic bridge here first.
	if err := m.bridgeManager.prepare(desired); err != nil {
		return err
	}
	return m.prepareAfterBridge(ctx, desired)
}

func (m *linuxManager) Validate(_ context.Context, desired model.NetworkConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.validateDesired(desired)
}

func (m *linuxManager) validateDesired(desired model.NetworkConfig) error {
	if !m.preflightReady {
		return fmt.Errorf("network preflight has not succeeded")
	}
	if err := model.ValidateNodeNetwork(desired); err != nil {
		return fmt.Errorf("network configuration is invalid: %w", err)
	}
	if err := m.detectOverlap(desired); err != nil {
		return err
	}
	return nil
}

func (m *linuxManager) prepareAfterBridge(ctx context.Context, desired model.NetworkConfig) error {
	hostLocalDir := filepath.Join(m.options.StateDir, "host-local")
	if err := ensurePrivateDirectory(hostLocalDir); err != nil {
		return err
	}
	document := map[string]any{
		"cniVersion": cniSpecVersion,
		"name":       m.name,
		"plugins": []any{
			map[string]any{
				"type": "bridge", "bridge": m.bridge, "isGateway": true, "hairpinMode": true,
				"ipMasq": false,
				"ipam": map[string]any{
					"type": "host-local", "dataDir": hostLocalDir,
					"ranges": []any{[]any{map[string]any{"subnet": desired.NodeSubnet, "gateway": desired.Gateway}}},
					"routes": []any{map[string]any{"dst": "0.0.0.0/0"}},
				},
			},
		},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return err
	}
	list, err := libcni.ConfListFromBytes(encoded)
	if err != nil {
		return fmt.Errorf("build CNI network list: %w", err)
	}
	if _, err := m.cni.ValidateNetworkList(ctx, list); err != nil {
		return fmt.Errorf("validate CNI network list: %w", err)
	}
	loopbackBytes, err := json.Marshal(map[string]any{
		"cniVersion": cniSpecVersion, "name": m.name + "-loopback", "type": "loopback",
	})
	if err != nil {
		return err
	}
	loopback, err := libcni.ConfFromBytes(loopbackBytes)
	if err != nil {
		return fmt.Errorf("build loopback CNI configuration: %w", err)
	}
	if _, err := m.cni.ValidateNetwork(ctx, loopback); err != nil {
		return fmt.Errorf("validate loopback CNI configuration: %w", err)
	}
	if err := m.firewall.prepare(desired); err != nil {
		return err
	}
	m.list = list
	m.loopback = loopback
	m.config = desired
	return nil
}

func (m *linuxManager) Attach(ctx context.Context, request AttachmentRequest) (Attachment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateRequest(request, true); err != nil {
		return Attachment{}, err
	}
	loopbackConfig := runtimeConf(request, "lo")
	if _, err := m.cni.AddNetwork(ctx, m.loopback, loopbackConfig); err != nil {
		return Attachment{}, fmt.Errorf("configure workload loopback: %w", err)
	}
	runtimeConfig := runtimeConf(request, request.IfName)
	result, err := m.cni.AddNetworkList(ctx, m.list, runtimeConfig)
	if err != nil {
		_ = m.cni.DelNetwork(context.Background(), m.loopback, loopbackConfig)
		return Attachment{}, fmt.Errorf("attach CNI network: %w", err)
	}
	attachment, err := m.attachmentFromResult(request, result)
	if err != nil {
		_ = m.cni.DelNetworkList(context.Background(), m.list, runtimeConfig)
		_ = m.cni.DelNetwork(context.Background(), m.loopback, loopbackConfig)
		return Attachment{}, err
	}
	return attachment, nil
}

func (m *linuxManager) Inspect(_ context.Context, request AttachmentRequest) (Attachment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateRequest(request, false); err != nil {
		return Attachment{}, err
	}
	result, err := m.cni.GetNetworkListCachedResult(m.list, runtimeConf(request, request.IfName))
	if err != nil {
		return Attachment{}, ErrNotFound
	}
	return m.attachmentFromResult(request, result)
}

func (m *linuxManager) Check(ctx context.Context, request AttachmentRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateRequest(request, true); err != nil {
		return err
	}
	if err := m.cni.CheckNetwork(ctx, m.loopback, runtimeConf(request, "lo")); err != nil {
		return fmt.Errorf("check workload loopback: %w", err)
	}
	if err := m.cni.CheckNetworkList(ctx, m.list, runtimeConf(request, request.IfName)); err != nil {
		return fmt.Errorf("check CNI attachment: %w", err)
	}
	return nil
}

func (m *linuxManager) GC(ctx context.Context, valid []AttachmentRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.list == nil || m.config.Generation == 0 {
		return fmt.Errorf("network is not prepared")
	}
	attachments := make([]cnitypes.GCAttachment, 0, len(valid))
	seen := make(map[string]struct{}, len(valid))
	for _, request := range valid {
		if request.AllocationID == "" || request.ObjectID == "" || request.SpecDigest == "" ||
			request.NetworkGeneration != m.config.Generation || request.IfName != "eth0" {
			return fmt.Errorf("network GC request is invalid")
		}
		if _, exists := seen[request.ObjectID]; exists {
			continue
		}
		seen[request.ObjectID] = struct{}{}
		attachments = append(attachments, cnitypes.GCAttachment{ContainerID: request.ObjectID, IfName: request.IfName})
	}
	sort.Slice(attachments, func(i, j int) bool { return attachments[i].ContainerID < attachments[j].ContainerID })
	if err := m.cni.GCNetworkList(ctx, m.list, &libcni.GCArgs{ValidAttachments: attachments}); err != nil {
		return fmt.Errorf("garbage-collect owned CNI network: %w", err)
	}
	return nil
}

func (m *linuxManager) Detach(ctx context.Context, request AttachmentRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateRequest(request, false); err != nil {
		return err
	}
	if err := m.cni.DelNetworkList(ctx, m.list, runtimeConf(request, request.IfName)); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "not found") {
			return fmt.Errorf("detach CNI network: %w", err)
		}
	}
	if err := m.cni.DelNetwork(ctx, m.loopback, runtimeConf(request, "lo")); err != nil &&
		!strings.Contains(strings.ToLower(err.Error()), "not found") {
		return fmt.Errorf("detach workload loopback: %w", err)
	}
	return nil
}

func (m *linuxManager) validateRequest(request AttachmentRequest, requireNetNS bool) error {
	if m.list == nil || m.config.Generation == 0 {
		return fmt.Errorf("network is not prepared")
	}
	if request.AllocationID == "" || request.ObjectID == "" || request.SpecDigest == "" ||
		request.NetworkGeneration != m.config.Generation || request.IfName != "eth0" {
		return fmt.Errorf("network attachment request is invalid")
	}
	if (requireNetNS || request.NetNS != "") && !netNSPattern.MatchString(request.NetNS) {
		return fmt.Errorf("network namespace path is invalid")
	}
	return nil
}

func runtimeConf(request AttachmentRequest, ifName string) *libcni.RuntimeConf {
	return &libcni.RuntimeConf{
		ContainerID: request.ObjectID, NetNS: request.NetNS, IfName: ifName,
		Args: [][2]string{{"IgnoreUnknown", "1"}, {"K8S_POD_NAME", request.AllocationID}},
	}
}

func (m *linuxManager) attachmentFromResult(request AttachmentRequest, result cnitypes.Result) (Attachment, error) {
	converted, err := types100.NewResultFromResult(result)
	if err != nil {
		return Attachment{}, fmt.Errorf("convert CNI result: %w", err)
	}
	nodeSubnet := netip.MustParsePrefix(m.config.NodeSubnet)
	address, err := allocationAddressFromResult(converted.IPs, nodeSubnet, netip.MustParseAddr(m.config.Gateway))
	if err != nil {
		return Attachment{}, err
	}
	return Attachment{AllocationID: request.AllocationID, ObjectID: request.ObjectID,
		SpecDigest: request.SpecDigest, NetworkGeneration: request.NetworkGeneration,
		Address: address, NetNS: request.NetNS}, nil
}

func (m *linuxManager) detectOverlap(desired model.NetworkConfig) error {
	cluster := netip.MustParsePrefix(desired.ClusterCIDR)
	interfaces, err := net.Interfaces()
	if err != nil {
		return fmt.Errorf("list host interfaces: %w", err)
	}
	for _, item := range interfaces {
		if item.Name == m.bridge || item.Name == m.firewall.wireguard {
			continue
		}
		addresses, err := item.Addrs()
		if err != nil {
			return err
		}
		for _, value := range addresses {
			prefix, err := netip.ParsePrefix(value.String())
			if err == nil && prefix.Addr().Is4() && prefixesOverlap(cluster, prefix.Masked()) {
				return fmt.Errorf("%w: workload CIDR overlaps host address prefix %s", ErrCIDRConflict, prefix.Masked())
			}
		}
	}
	routes, err := hostIPv4Routes()
	if err != nil {
		return err
	}
	for _, route := range routes {
		if route.Interface != m.bridge && route.Interface != m.firewall.wireguard && route.Prefix.Bits() != 0 && prefixesOverlap(cluster, route.Prefix) {
			return fmt.Errorf("%w: workload CIDR overlaps host route %s", ErrCIDRConflict, route.Prefix)
		}
	}
	return nil
}

func prefixesOverlap(left, right netip.Prefix) bool {
	return left.Contains(right.Addr()) || right.Contains(left.Addr())
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("network state path must be a private non-symlink directory: %s", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return fmt.Errorf("network state path must be owned by root: %s", path)
	}
	return nil
}

type hostRoute struct {
	Interface string
	Prefix    netip.Prefix
}

func hostIPv4Routes() ([]hostRoute, error) {
	contents, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return nil, fmt.Errorf("read host routes: %w", err)
	}
	var routes []hostRoute
	lines := strings.Split(string(contents), "\n")
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		destination, err1 := strconv.ParseUint(fields[1], 16, 32)
		mask, err2 := strconv.ParseUint(fields[7], 16, 32)
		if err1 != nil || err2 != nil {
			continue
		}
		address := netip.AddrFrom4([4]byte{byte(destination), byte(destination >> 8), byte(destination >> 16), byte(destination >> 24)})
		maskBytes := net.IPMask{byte(mask), byte(mask >> 8), byte(mask >> 16), byte(mask >> 24)}
		ones, bits := maskBytes.Size()
		if bits != 32 || ones < 0 {
			continue
		}
		routes = append(routes, hostRoute{Interface: fields[0], Prefix: netip.PrefixFrom(address, ones).Masked()})
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Prefix.String() < routes[j].Prefix.String() })
	return routes, nil
}

func (*linuxManager) Close() error { return nil }
