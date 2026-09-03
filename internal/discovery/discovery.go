package discovery

import (
	"context"
	"net/netip"
	"sync"
)

type Controller interface {
	Replace(Snapshot) error
	Start(string) error
	Close(context.Context) error
}

type Fake struct {
	mu         sync.Mutex
	Snapshot   Snapshot
	Address    string
	Started    bool
	ReplaceErr error
	StartErr   error
	Closed     bool
}

func NewFake() *Fake { return &Fake{} }

func (f *Fake) Replace(snapshot Snapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ReplaceErr != nil {
		return f.ReplaceErr
	}
	f.Snapshot = cloneSnapshot(snapshot)
	return nil
}

func (f *Fake) Start(address string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StartErr != nil {
		return f.StartErr
	}
	f.Address = address
	f.Started = true
	f.Closed = false
	return nil
}

func (f *Fake) Close(context.Context) error {
	f.mu.Lock()
	f.Closed = true
	f.Started = false
	f.Address = ""
	f.mu.Unlock()
	return nil
}

func (f *Fake) Current() (Snapshot, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneSnapshot(f.Snapshot), f.Address, f.Started
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	result := Snapshot{Known: make(map[string]struct{}, len(snapshot.Known)), Addresses: make(map[string][]netip.Addr, len(snapshot.Addresses))}
	for name := range snapshot.Known {
		result.Known[name] = struct{}{}
	}
	for name, addresses := range snapshot.Addresses {
		result.Addresses[name] = append([]netip.Addr(nil), addresses...)
	}
	return result
}

var _ Controller = (*Server)(nil)
var _ Controller = (*Fake)(nil)
