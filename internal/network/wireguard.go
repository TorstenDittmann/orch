package network

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/TorstenDittmann/orch/internal/model"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

const (
	wireGuardKeyFilename   = "wireguard.key"
	wireGuardOwnerFilename = "wireguard-owner.json"
	wireGuardOwnerVersion  = 1
)

// WireGuardOptions identifies the single interface owned by one orch agent.
type WireGuardOptions struct {
	ClusterID string
	NodeID    string
	StateDir  string
	KeyPath   string
}

// WireGuardManager owns exactly one deterministic interface and only routes
// recorded in its private ownership marker.
type WireGuardManager interface {
	PublicKey() string
	InterfaceName() string
	Preflight(context.Context) error
	EnsureOwnership(context.Context) error
	Reconcile(context.Context, model.WireGuardConfig) error
	Close() error
}

type wireGuardLink struct {
	Name  string
	Alias string
	Kind  string
}

type wireGuardDevicePeer struct {
	PublicKey     wgtypes.Key
	Endpoint      netip.AddrPort
	AllowedSubnet netip.Prefix
}

type wireGuardDeviceConfig struct {
	PrivateKey wgtypes.Key
	ListenPort int
	Peers      []wireGuardDevicePeer
}

// wireGuardBackend is deliberately narrow so ownership and operation ordering
// can be tested without changing host networking.
type wireGuardBackend interface {
	Preflight(context.Context) error
	Link(string) (wireGuardLink, bool, error)
	CreateLink(string, string) error
	EnsureRoute(string, netip.Prefix, netip.Addr) error
	Configure(string, wireGuardDeviceConfig) error
	SetLinkUp(string) error
	RemoveRoute(string, netip.Prefix) error
	Close() error
}

type wireGuardManager struct {
	mu        sync.Mutex
	options   WireGuardOptions
	backend   wireGuardBackend
	key       wgtypes.Key
	publicKey string
	name      string
	alias     string
	ownerPath string
	ready     bool
	closed    bool
}

type wireGuardOwner struct {
	Version          int      `json:"version"`
	ClusterID        string   `json:"cluster_id"`
	NodeID           string   `json:"node_id"`
	InterfaceName    string   `json:"interface_name"`
	Alias            string   `json:"alias"`
	PublicKey        string   `json:"public_key"`
	Generation       uint64   `json:"generation"`
	ConfigurationSHA string   `json:"configuration_sha256,omitempty"`
	Routes           []string `json:"routes,omitempty"`
}

func ValidateWireGuardOptions(options WireGuardOptions) error {
	for name, value := range map[string]string{
		"cluster id": options.ClusterID, "node id": options.NodeID, "network state directory": options.StateDir,
	} {
		if value == "" || strings.TrimSpace(value) != value || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("WireGuard %s is required and must be canonical", name)
		}
	}
	if options.KeyPath != "" && (strings.TrimSpace(options.KeyPath) != options.KeyPath || strings.ContainsRune(options.KeyPath, '\x00')) {
		return fmt.Errorf("WireGuard key path must be canonical")
	}
	return nil
}

// WireGuardInterfaceName produces a Linux-safe (at most 15-byte) name scoped
// to the cluster and node. The full identity is retained in the ownership
// marker and link alias; the short name alone never establishes ownership.
func WireGuardInterfaceName(clusterID, nodeID string) string {
	digest := sha256.Sum256([]byte(clusterID + "\x00" + nodeID))
	return "orchwg" + hex.EncodeToString(digest[:])[:9]
}

func newWireGuardManager(options WireGuardOptions, backend wireGuardBackend, random io.Reader) (*wireGuardManager, error) {
	if err := ValidateWireGuardOptions(options); err != nil {
		return nil, err
	}
	if backend == nil {
		return nil, fmt.Errorf("WireGuard backend is required")
	}
	if random == nil {
		random = rand.Reader
	}
	if options.KeyPath == "" {
		options.KeyPath = filepath.Join(options.StateDir, wireGuardKeyFilename)
	}
	key, _, err := loadOrCreateWireGuardKey(options.KeyPath, random)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(options.ClusterID + "\x00" + options.NodeID))
	identity := hex.EncodeToString(digest[:])
	return &wireGuardManager{
		options: options, backend: backend, key: key, publicKey: key.PublicKey().String(),
		name:      WireGuardInterfaceName(options.ClusterID, options.NodeID),
		alias:     "orch-managed-wireguard-" + identity,
		ownerPath: filepath.Join(options.StateDir, wireGuardOwnerFilename),
	}, nil
}

func (m *wireGuardManager) PublicKey() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ""
	}
	return m.publicKey
}

func (m *wireGuardManager) InterfaceName() string { return m.name }

func (m *wireGuardManager) Preflight(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("WireGuard manager is closed")
	}
	m.ready = false
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.backend.Preflight(ctx); err != nil {
		return err
	}
	m.ready = true
	return nil
}

// EnsureOwnership reserves or verifies the deterministic interface without
// installing peer routes. This lets the bridge gateway exist before routes
// receive their preferred source while ensuring the firewall never trusts a
// foreign interface with the same short name.
func (m *wireGuardManager) EnsureOwnership(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("WireGuard manager is closed")
	}
	if !m.ready {
		return fmt.Errorf("WireGuard preflight has not succeeded")
	}
	_, err := m.ensureOwned(ctx)
	return err
}

func (m *wireGuardManager) Reconcile(ctx context.Context, configuration model.WireGuardConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("WireGuard manager is closed")
	}
	if !m.ready {
		return fmt.Errorf("WireGuard preflight has not succeeded")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := model.ValidateWireGuardConfig(configuration); err != nil {
		return fmt.Errorf("invalid WireGuard configuration: %w", err)
	}

	wantOwner, err := m.ownerFor(configuration)
	if err != nil {
		return err
	}
	currentOwner, err := m.ensureOwned(ctx)
	if err != nil {
		return err
	}

	device, desiredRoutes, err := m.deviceConfiguration(configuration)
	if err != nil {
		return err
	}
	// Persist the union before adding anything to the host. If reconciliation
	// stops partway through, the next run can still distinguish a route we may
	// have created from unrelated host routes and clean it up safely.
	workingOwner := currentOwner
	workingOwner.Routes = routeUnion(currentOwner.Routes, wantOwner.Routes)
	if !wireGuardOwnersEqual(currentOwner, workingOwner) {
		if err := replaceWireGuardOwner(m.ownerPath, workingOwner); err != nil {
			return err
		}
		currentOwner = workingOwner
	}
	// A fresh point-to-point interface must be up before Linux accepts routes
	// through it. It is still an inert sink here: peer configuration happens
	// only after all desired route additions have succeeded.
	if err := m.backend.SetLinkUp(m.name); err != nil {
		return fmt.Errorf("bring WireGuard interface %q up: %w", m.name, err)
	}
	// Install additions before changing peers, then withdraw only routes listed
	// in our previous marker. This makes retries safe after partial failure and
	// never removes a route merely because it points at the orch interface.
	localSubnet := netip.MustParsePrefix(configuration.NodeSubnet)
	sourceAddress := localSubnet.Addr().Next()
	for _, prefix := range desiredRoutes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.backend.EnsureRoute(m.name, prefix, sourceAddress); err != nil {
			return fmt.Errorf("install WireGuard route %s: %w", prefix, err)
		}
	}
	if err := m.backend.Configure(m.name, device); err != nil {
		return fmt.Errorf("configure WireGuard interface %q: %w", m.name, err)
	}
	desiredSet := make(map[string]struct{}, len(wantOwner.Routes))
	for _, prefix := range wantOwner.Routes {
		desiredSet[prefix] = struct{}{}
	}
	for _, value := range currentOwner.Routes {
		if _, keep := desiredSet[value]; keep {
			continue
		}
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return fmt.Errorf("WireGuard ownership marker contains an invalid route")
		}
		if err := m.backend.RemoveRoute(m.name, prefix); err != nil {
			return fmt.Errorf("remove stale WireGuard route %s: %w", prefix, err)
		}
	}
	if !wireGuardOwnersEqual(currentOwner, wantOwner) {
		if err := replaceWireGuardOwner(m.ownerPath, wantOwner); err != nil {
			return err
		}
	}
	return nil
}

func (m *wireGuardManager) ensureOwned(ctx context.Context) (wireGuardOwner, error) {
	if err := ctx.Err(); err != nil {
		return wireGuardOwner{}, err
	}
	currentOwner, markerErr := readWireGuardOwner(m.ownerPath)
	link, exists, err := m.backend.Link(m.name)
	if err != nil {
		return wireGuardOwner{}, fmt.Errorf("inspect WireGuard interface %q: %w", m.name, err)
	}
	if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
		return wireGuardOwner{}, markerErr
	}
	if errors.Is(markerErr, os.ErrNotExist) && exists {
		return wireGuardOwner{}, fmt.Errorf("WireGuard interface %q exists without an orch ownership marker: %w", m.name, ErrOwnershipMismatch)
	}
	if markerErr == nil {
		if err := m.validateOwner(currentOwner); err != nil {
			return wireGuardOwner{}, err
		}
	}
	if !exists {
		if errors.Is(markerErr, os.ErrNotExist) {
			initialOwner := wireGuardOwner{
				Version: wireGuardOwnerVersion, ClusterID: m.options.ClusterID, NodeID: m.options.NodeID,
				InterfaceName: m.name, Alias: m.alias, PublicKey: m.publicKey,
			}
			if err := createWireGuardOwner(m.ownerPath, initialOwner); err != nil {
				return wireGuardOwner{}, err
			}
			currentOwner = initialOwner
		}
		if err := m.backend.CreateLink(m.name, m.alias); err != nil {
			return wireGuardOwner{}, fmt.Errorf("create WireGuard interface %q: %w", m.name, err)
		}
		link, exists, err = m.backend.Link(m.name)
		if err != nil || !exists {
			return wireGuardOwner{}, fmt.Errorf("inspect created WireGuard interface %q: %w", m.name, err)
		}
	}
	if link.Kind != "wireguard" || link.Alias != m.alias {
		return wireGuardOwner{}, fmt.Errorf("WireGuard interface %q does not match its ownership marker: %w", m.name, ErrOwnershipMismatch)
	}
	return currentOwner, nil
}

func wireGuardOwnersEqual(left, right wireGuardOwner) bool {
	return left.Version == right.Version && left.ClusterID == right.ClusterID && left.NodeID == right.NodeID &&
		left.InterfaceName == right.InterfaceName && left.Alias == right.Alias && left.PublicKey == right.PublicKey &&
		left.Generation == right.Generation && left.ConfigurationSHA == right.ConfigurationSHA && slices.Equal(left.Routes, right.Routes)
}

func routeUnion(left, right []string) []string {
	values := make(map[string]struct{}, len(left)+len(right))
	for _, value := range left {
		values[value] = struct{}{}
	}
	for _, value := range right {
		values[value] = struct{}{}
	}
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

func (m *wireGuardManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	m.ready = false
	m.publicKey = ""
	for index := range m.key {
		m.key[index] = 0
	}
	return m.backend.Close()
}

func (m *wireGuardManager) validateOwner(owner wireGuardOwner) error {
	if owner.Version != wireGuardOwnerVersion || owner.ClusterID != m.options.ClusterID || owner.NodeID != m.options.NodeID ||
		owner.InterfaceName != m.name || owner.Alias != m.alias || owner.PublicKey != m.publicKey {
		return fmt.Errorf("WireGuard ownership marker does not match this agent: %w", ErrOwnershipMismatch)
	}
	return nil
}

func (m *wireGuardManager) ownerFor(configuration model.WireGuardConfig) (wireGuardOwner, error) {
	encoded, err := json.Marshal(configuration)
	if err != nil {
		return wireGuardOwner{}, err
	}
	digest := sha256.Sum256(encoded)
	// Keep the whole cluster CIDR routed to WireGuard as an owned sink. The
	// local connected /24 and active peer /24 routes are more specific, while an
	// unused node subnet can no longer fall through to the host's underlay
	// default route in plaintext.
	routes := make([]string, 1, len(configuration.Peers)+1)
	routes[0] = configuration.ClusterCIDR
	for _, peer := range configuration.Peers {
		routes = append(routes, peer.AllowedSubnet)
	}
	slices.Sort(routes)
	return wireGuardOwner{
		Version: wireGuardOwnerVersion, ClusterID: m.options.ClusterID, NodeID: m.options.NodeID,
		InterfaceName: m.name, Alias: m.alias, PublicKey: m.publicKey,
		Generation: configuration.Generation, ConfigurationSHA: hex.EncodeToString(digest[:]), Routes: routes,
	}, nil
}

func (m *wireGuardManager) deviceConfiguration(configuration model.WireGuardConfig) (wireGuardDeviceConfig, []netip.Prefix, error) {
	device := wireGuardDeviceConfig{PrivateKey: m.key, ListenPort: int(configuration.ListenPort)}
	cluster, err := netip.ParsePrefix(configuration.ClusterCIDR)
	if err != nil {
		return wireGuardDeviceConfig{}, nil, fmt.Errorf("parse WireGuard cluster route: %w", err)
	}
	routes := make([]netip.Prefix, 1, len(configuration.Peers)+1)
	routes[0] = cluster
	for _, peer := range configuration.Peers {
		key, err := wgtypes.ParseKey(peer.PublicKey)
		if err != nil {
			return wireGuardDeviceConfig{}, nil, fmt.Errorf("parse WireGuard peer key: %w", err)
		}
		if key == m.key.PublicKey() {
			return wireGuardDeviceConfig{}, nil, fmt.Errorf("WireGuard peer public key matches the local node")
		}
		endpoint, err := netip.ParseAddrPort(peer.Endpoint)
		if err != nil {
			return wireGuardDeviceConfig{}, nil, fmt.Errorf("parse WireGuard peer endpoint: %w", err)
		}
		allowed, err := netip.ParsePrefix(peer.AllowedSubnet)
		if err != nil {
			return wireGuardDeviceConfig{}, nil, fmt.Errorf("parse WireGuard peer subnet: %w", err)
		}
		device.Peers = append(device.Peers, wireGuardDevicePeer{PublicKey: key, Endpoint: endpoint, AllowedSubnet: allowed})
		routes = append(routes, allowed)
	}
	return device, routes, nil
}

func loadOrCreateWireGuardKey(path string, random io.Reader) (wgtypes.Key, bool, error) {
	if random == nil {
		random = rand.Reader
	}
	key, err := readWireGuardKey(path)
	if err == nil {
		return key, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return wgtypes.Key{}, false, err
	}
	raw := make([]byte, wgtypes.KeyLen)
	if _, err := io.ReadFull(random, raw); err != nil {
		return wgtypes.Key{}, false, fmt.Errorf("generate WireGuard private key: %w", err)
	}
	defer wipeBytes(raw)
	clampWireGuardPrivateKey(raw)
	key, err = wgtypes.NewKey(raw)
	if err != nil {
		return wgtypes.Key{}, false, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			loaded, loadErr := readWireGuardKey(path)
			return loaded, false, loadErr
		}
		return wgtypes.Key{}, false, fmt.Errorf("create WireGuard private key: %w", err)
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(raw); err != nil {
		return wgtypes.Key{}, false, fmt.Errorf("write WireGuard private key: %w", err)
	}
	if err := file.Sync(); err != nil {
		return wgtypes.Key{}, false, fmt.Errorf("sync WireGuard private key: %w", err)
	}
	if err := file.Close(); err != nil {
		return wgtypes.Key{}, false, fmt.Errorf("close WireGuard private key: %w", err)
	}
	if err := syncWireGuardDirectory(filepath.Dir(path)); err != nil {
		return wgtypes.Key{}, false, err
	}
	remove = false
	return key, true, nil
}

func readWireGuardKey(path string) (wgtypes.Key, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return wgtypes.Key{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return wgtypes.Key{}, fmt.Errorf("WireGuard private key must be a private regular file with mode 0600")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return wgtypes.Key{}, fmt.Errorf("read WireGuard private key: %w", err)
	}
	defer wipeBytes(raw)
	if len(raw) != wgtypes.KeyLen || !validClampedWireGuardPrivateKey(raw) {
		return wgtypes.Key{}, fmt.Errorf("WireGuard private key is invalid")
	}
	return wgtypes.NewKey(raw)
}

func clampWireGuardPrivateKey(key []byte) {
	key[0] &= 248
	key[31] &= 127
	key[31] |= 64
}

func validClampedWireGuardPrivateKey(key []byte) bool {
	if len(key) != wgtypes.KeyLen || key[0]&7 != 0 || key[31]&128 != 0 || key[31]&64 == 0 {
		return false
	}
	for _, value := range key {
		if value != 0 {
			return true
		}
	}
	return false
}

func wipeBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func readWireGuardOwner(path string) (wireGuardOwner, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return wireGuardOwner{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > 32<<10 {
		return wireGuardOwner{}, fmt.Errorf("WireGuard ownership marker is unsafe")
	}
	contents, err := os.ReadFile(path)
	if err != nil || len(contents) > 32<<10 {
		return wireGuardOwner{}, fmt.Errorf("read WireGuard ownership marker")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var owner wireGuardOwner
	if err := decoder.Decode(&owner); err != nil {
		return wireGuardOwner{}, fmt.Errorf("decode WireGuard ownership marker: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF || owner.Version != wireGuardOwnerVersion || owner.ClusterID == "" ||
		owner.NodeID == "" || owner.InterfaceName == "" || owner.Alias == "" || owner.PublicKey == "" {
		return wireGuardOwner{}, fmt.Errorf("WireGuard ownership marker is incomplete")
	}
	if !slices.IsSorted(owner.Routes) {
		return wireGuardOwner{}, fmt.Errorf("WireGuard ownership marker routes are not canonical")
	}
	for _, value := range owner.Routes {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || !prefix.Addr().Is4() || prefix.String() != value ||
			(prefix.Bits() != 24 && (prefix.Bits() < 16 || prefix.Bits() > 19 || !prefix.Addr().IsPrivate())) {
			return wireGuardOwner{}, fmt.Errorf("WireGuard ownership marker contains an invalid route")
		}
	}
	return owner, nil
}

func createWireGuardOwner(path string, owner wireGuardOwner) error {
	encoded, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create WireGuard ownership marker: %w", err)
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("write WireGuard ownership marker: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync WireGuard ownership marker: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close WireGuard ownership marker: %w", err)
	}
	if err := syncWireGuardDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	remove = false
	return nil
}

func replaceWireGuardOwner(path string, owner wireGuardOwner) error {
	encoded, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".wireguard-owner-*")
	if err != nil {
		return fmt.Errorf("create temporary WireGuard ownership marker: %w", err)
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
		return fmt.Errorf("replace WireGuard ownership marker: %w", err)
	}
	return syncWireGuardDirectory(filepath.Dir(path))
}

func syncWireGuardDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open WireGuard state directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync WireGuard state directory: %w", err)
	}
	return nil
}
