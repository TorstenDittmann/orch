package network

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"change.codes/torsten/better-swarm/internal/model"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type fakeWireGuardBackend struct {
	link          *wireGuardLink
	routes        map[netip.Prefix]struct{}
	routeSources  map[netip.Prefix]netip.Addr
	operations    []string
	configuration wireGuardDeviceConfig
	preflightErr  error
	configureErr  error
	closed        bool
}

func (f *fakeWireGuardBackend) Preflight(context.Context) error { return f.preflightErr }

func (f *fakeWireGuardBackend) Link(name string) (wireGuardLink, bool, error) {
	if f.link == nil {
		return wireGuardLink{}, false, nil
	}
	return *f.link, true, nil
}

func (f *fakeWireGuardBackend) CreateLink(name, alias string) error {
	if f.link != nil {
		return errors.New("link already exists")
	}
	f.operations = append(f.operations, "create")
	f.link = &wireGuardLink{Name: name, Alias: alias, Kind: "wireguard"}
	return nil
}

func (f *fakeWireGuardBackend) EnsureRoute(_ string, prefix netip.Prefix, source netip.Addr) error {
	f.operations = append(f.operations, "route+:"+prefix.String())
	if f.routes == nil {
		f.routes = make(map[netip.Prefix]struct{})
	}
	f.routes[prefix] = struct{}{}
	if f.routeSources == nil {
		f.routeSources = make(map[netip.Prefix]netip.Addr)
	}
	f.routeSources[prefix] = source
	return nil
}

func (f *fakeWireGuardBackend) Configure(_ string, configuration wireGuardDeviceConfig) error {
	f.operations = append(f.operations, "configure")
	if f.configureErr != nil {
		return f.configureErr
	}
	f.configuration = configuration
	f.configuration.Peers = slices.Clone(configuration.Peers)
	return nil
}

func (f *fakeWireGuardBackend) SetLinkUp(string) error {
	f.operations = append(f.operations, "up")
	return nil
}

func (f *fakeWireGuardBackend) RemoveRoute(_ string, prefix netip.Prefix) error {
	f.operations = append(f.operations, "route-:"+prefix.String())
	delete(f.routes, prefix)
	delete(f.routeSources, prefix)
	return nil
}

func (f *fakeWireGuardBackend) Close() error {
	f.closed = true
	return nil
}

func TestWireGuardInterfaceNameIsStableAndScoped(t *testing.T) {
	name := WireGuardInterfaceName("cluster-one", "node-one")
	if name != WireGuardInterfaceName("cluster-one", "node-one") || len(name) > 15 || !strings.HasPrefix(name, "orchwg") {
		t.Fatalf("WireGuardInterfaceName() = %q", name)
	}
	if name == WireGuardInterfaceName("cluster-one", "node-two") || name == WireGuardInterfaceName("cluster-two", "node-one") {
		t.Fatal("WireGuard interface names were not scoped to both cluster and node")
	}
}

func TestWireGuardKeyCreationIsPrivateDurableAndStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wireguard.key")
	random := bytes.NewReader(slices.Repeat([]byte{0xff}, wgtypes.KeyLen))
	first, created, err := loadOrCreateWireGuardKey(path, random)
	if err != nil || !created {
		t.Fatalf("create key = %v, %t, %v", first, created, err)
	}
	if first[0]&7 != 0 || first[31]&128 != 0 || first[31]&64 == 0 {
		t.Fatalf("private key was not clamped: %x ... %x", first[0], first[31])
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("key metadata = %#v, %v", info, err)
	}
	second, created, err := loadOrCreateWireGuardKey(path, bytes.NewReader(make([]byte, wgtypes.KeyLen)))
	if err != nil || created || second != first {
		t.Fatalf("reload key = %v, %t, %v; want original", second, created, err)
	}
}

func TestWireGuardKeyRejectsUnsafeFiles(t *testing.T) {
	directory := t.TempDir()
	valid := make([]byte, wgtypes.KeyLen)
	for index := range valid {
		valid[index] = byte(index + 1)
	}
	clampWireGuardPrivateKey(valid)

	permissive := filepath.Join(directory, "permissive")
	if err := os.WriteFile(permissive, valid, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readWireGuardKey(permissive); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("permissive key error = %v", err)
	}

	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, valid, 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(directory, "symlink")
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := readWireGuardKey(symlink); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink key error = %v", err)
	}

	invalid := filepath.Join(directory, "invalid")
	if err := os.WriteFile(invalid, make([]byte, wgtypes.KeyLen), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readWireGuardKey(invalid); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("invalid key error = %v", err)
	}
}

func TestWireGuardReconcileOwnsInterfaceAndOrdersRouteChanges(t *testing.T) {
	backend := &fakeWireGuardBackend{}
	manager := newTestWireGuardManager(t, backend)
	if err := manager.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := testWireGuardConfiguration(t, "node-one", "10.88.1.0/24", "10.0.0.1:51820", 1)
	if err := manager.Reconcile(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if got, want := backend.operations, []string{"create", "up", "route+:10.88.0.0/16", "route+:10.88.1.0/24", "configure"}; !slices.Equal(got, want) {
		t.Fatalf("first operations = %v, want %v", got, want)
	}
	if backend.configuration.ListenPort != model.DefaultWireGuardListenPort || len(backend.configuration.Peers) != 1 ||
		backend.configuration.Peers[0].AllowedSubnet.String() != "10.88.1.0/24" {
		t.Fatalf("device configuration = %#v", backend.configuration)
	}
	if source := backend.routeSources[netip.MustParsePrefix("10.88.1.0/24")]; source.String() != "10.88.0.1" {
		t.Fatalf("route preferred source = %s, want bridge gateway", source)
	}
	if source := backend.routeSources[netip.MustParsePrefix("10.88.0.0/16")]; source.String() != "10.88.0.1" {
		t.Fatalf("cluster sink route preferred source = %s, want bridge gateway", source)
	}
	marker, err := readWireGuardOwner(filepath.Join(manager.options.StateDir, wireGuardOwnerFilename))
	if err != nil || marker.Generation != 1 || !slices.Equal(marker.Routes, []string{"10.88.0.0/16", "10.88.1.0/24"}) {
		t.Fatalf("ownership marker = %#v, %v", marker, err)
	}

	backend.operations = nil
	second := testWireGuardConfiguration(t, "node-two", "10.88.2.0/24", "10.0.0.2:51820", 2)
	if err := manager.Reconcile(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if got, want := backend.operations, []string{"up", "route+:10.88.0.0/16", "route+:10.88.2.0/24", "configure", "route-:10.88.1.0/24"}; !slices.Equal(got, want) {
		t.Fatalf("replacement operations = %v, want %v", got, want)
	}
	if _, exists := backend.routes[netip.MustParsePrefix("10.88.1.0/24")]; exists {
		t.Fatal("stale managed route remains")
	}
	if _, exists := backend.routes[netip.MustParsePrefix("10.88.2.0/24")]; !exists {
		t.Fatal("new managed route is missing")
	}
	if _, exists := backend.routes[netip.MustParsePrefix("10.88.0.0/16")]; !exists {
		t.Fatal("cluster sink route was removed during peer replacement")
	}
}

func TestWireGuardReconcileRefusesForeignInterfaceAndAlias(t *testing.T) {
	t.Run("unmarked interface", func(t *testing.T) {
		backend := &fakeWireGuardBackend{link: &wireGuardLink{Kind: "wireguard", Alias: "foreign"}}
		manager := newTestWireGuardManager(t, backend)
		backend.link.Name = manager.InterfaceName()
		if err := manager.Preflight(context.Background()); err != nil {
			t.Fatal(err)
		}
		err := manager.Reconcile(context.Background(), testWireGuardConfiguration(t, "node-one", "10.88.1.0/24", "10.0.0.1:51820", 1))
		if !errors.Is(err, ErrOwnershipMismatch) {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if len(backend.operations) != 0 {
			t.Fatalf("foreign interface was modified: %v", backend.operations)
		}
	})

	t.Run("marked interface with changed alias", func(t *testing.T) {
		backend := &fakeWireGuardBackend{}
		manager := newTestWireGuardManager(t, backend)
		if err := manager.Preflight(context.Background()); err != nil {
			t.Fatal(err)
		}
		configuration := testWireGuardConfiguration(t, "node-one", "10.88.1.0/24", "10.0.0.1:51820", 1)
		if err := manager.Reconcile(context.Background(), configuration); err != nil {
			t.Fatal(err)
		}
		backend.operations = nil
		backend.link.Alias = "changed"
		if err := manager.Reconcile(context.Background(), configuration); !errors.Is(err, ErrOwnershipMismatch) {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if len(backend.operations) != 0 {
			t.Fatalf("changed interface was modified: %v", backend.operations)
		}
	})
}

func TestWireGuardReconcileRequiresPreflightAndValidCanonicalIntent(t *testing.T) {
	backend := &fakeWireGuardBackend{}
	manager := newTestWireGuardManager(t, backend)
	configuration := testWireGuardConfiguration(t, "node-one", "10.88.1.0/24", "10.0.0.1:51820", 1)
	if err := manager.Reconcile(context.Background(), configuration); err == nil || !strings.Contains(err.Error(), "preflight") {
		t.Fatalf("Reconcile before preflight error = %v", err)
	}
	if err := manager.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	configuration.Peers[0].Endpoint = "127.0.0.1:51820"
	if err := manager.Reconcile(context.Background(), configuration); err == nil || !strings.Contains(err.Error(), "unicast IPv4") {
		t.Fatalf("invalid Reconcile error = %v", err)
	}
	if len(backend.operations) != 0 {
		t.Fatalf("invalid configuration modified backend: %v", backend.operations)
	}
}

func TestWireGuardReconcileTracksPartialRouteAdditionsForLaterCleanup(t *testing.T) {
	backend := &fakeWireGuardBackend{configureErr: errors.New("configuration failed")}
	manager := newTestWireGuardManager(t, backend)
	if err := manager.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := testWireGuardConfiguration(t, "node-one", "10.88.1.0/24", "10.0.0.1:51820", 1)
	if err := manager.Reconcile(context.Background(), first); err == nil {
		t.Fatal("Reconcile() succeeded despite backend failure")
	}
	marker, err := readWireGuardOwner(filepath.Join(manager.options.StateDir, wireGuardOwnerFilename))
	if err != nil || !slices.Equal(marker.Routes, []string{"10.88.0.0/16", "10.88.1.0/24"}) {
		t.Fatalf("partial ownership marker = %#v, %v", marker, err)
	}
	backend.configureErr = nil
	backend.operations = nil
	second := testWireGuardConfiguration(t, "node-two", "10.88.2.0/24", "10.0.0.2:51820", 2)
	if err := manager.Reconcile(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if got, want := backend.operations, []string{"up", "route+:10.88.0.0/16", "route+:10.88.2.0/24", "configure", "route-:10.88.1.0/24"}; !slices.Equal(got, want) {
		t.Fatalf("recovery operations = %v, want %v", got, want)
	}
}

func TestWireGuardManagerCloseWipesIdentityAndClosesBackend(t *testing.T) {
	backend := &fakeWireGuardBackend{}
	manager := newTestWireGuardManager(t, backend)
	if manager.PublicKey() == "" {
		t.Fatal("manager public key is empty before close")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if manager.PublicKey() != "" || !backend.closed {
		t.Fatalf("closed state: public key %q, backend closed %t", manager.PublicKey(), backend.closed)
	}
	if manager.key != (wgtypes.Key{}) {
		t.Fatal("private key was not wiped")
	}
}

func newTestWireGuardManager(t *testing.T, backend wireGuardBackend) *wireGuardManager {
	t.Helper()
	directory := t.TempDir()
	manager, err := newWireGuardManager(WireGuardOptions{
		ClusterID: "cluster-one", NodeID: "node-local", StateDir: directory,
	}, backend, bytes.NewReader(slices.Repeat([]byte{0xab}, wgtypes.KeyLen)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func testWireGuardConfiguration(t *testing.T, nodeID, subnet, endpoint string, generation uint64) model.WireGuardConfig {
	t.Helper()
	raw := bytes.Repeat([]byte{byte(generation + 1)}, wgtypes.KeyLen)
	key, err := wgtypes.NewKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	return model.WireGuardConfig{
		Generation: generation, ClusterCIDR: model.DefaultWorkloadCIDR, NodeSubnet: "10.88.0.0/24",
		ListenPort: model.DefaultWireGuardListenPort,
		Peers:      []model.WireGuardPeer{{NodeID: nodeID, PublicKey: key.String(), Endpoint: endpoint, AllowedSubnet: subnet}},
	}
}

func TestWireGuardOwnerRejectsUnknownFieldsAndUnsafeMode(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, wireGuardOwnerFilename)
	if err := os.WriteFile(path, []byte(`{"version":1,"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readWireGuardOwner(path); err == nil {
		t.Fatal("ownership marker with unknown fields was accepted")
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"version":%d}`, wireGuardOwnerVersion)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readWireGuardOwner(path); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("unsafe ownership marker error = %v", err)
	}
}
