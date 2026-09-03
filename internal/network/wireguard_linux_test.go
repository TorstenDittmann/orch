//go:build linux

package network

import (
	"errors"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestLinuxWireGuardBackendLabelsLinkAfterCreation(t *testing.T) {
	t.Parallel()
	links := &fakeWireGuardLinkManager{}
	backend := &linuxWireGuardBackend{links: links}
	if err := backend.CreateLink("orchwg-test", "orch-managed-test"); err != nil {
		t.Fatal(err)
	}
	if links.added == nil || links.added.Attrs().Alias != "" {
		t.Fatalf("link create alias = %q, want explicit post-create labelling", links.added.Attrs().Alias)
	}
	if links.alias != "orch-managed-test" || links.deleted {
		t.Fatalf("alias = %q, deleted = %t", links.alias, links.deleted)
	}
}

func TestLinuxWireGuardBackendRemovesLinkWhenLabellingFails(t *testing.T) {
	t.Parallel()
	links := &fakeWireGuardLinkManager{setAliasErr: errors.New("alias rejected")}
	backend := &linuxWireGuardBackend{links: links}
	err := backend.CreateLink("orchwg-test", "orch-managed-test")
	if err == nil || !links.deleted {
		t.Fatalf("CreateLink() = %v, deleted = %t", err, links.deleted)
	}
}

type fakeWireGuardLinkManager struct {
	added       netlink.Link
	alias       string
	setAliasErr error
	deleted     bool
}

func (f *fakeWireGuardLinkManager) Add(link netlink.Link) error {
	f.added = link
	return nil
}

func (f *fakeWireGuardLinkManager) ByName(string) (netlink.Link, error) {
	return f.added, nil
}

func (f *fakeWireGuardLinkManager) SetAlias(_ netlink.Link, alias string) error {
	f.alias = alias
	return f.setAliasErr
}

func (f *fakeWireGuardLinkManager) Delete(netlink.Link) error {
	f.deleted = true
	return nil
}
