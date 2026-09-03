package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"runtime"
	"sort"
	"sync"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/network"
)

type Fake struct {
	mu              sync.Mutex
	Capabilities    Capabilities
	Objects         map[string]Object
	Allocations     map[string]model.AllocationDesired
	Logs            map[string][]LogFrame
	ExecFrames      map[string][]ExecFrame
	EnsureErrors    map[string]error
	NetworkError    error
	Operations      []string
	PreparedNetwork model.NetworkConfig
	nextAddress     netip.Addr
	waiters         map[string][]chan Object
	closed          bool
}

func NewFake() *Fake {
	return &Fake{
		Capabilities: Capabilities{
			OperatingSystem: "linux", Architecture: "amd64", RuntimeVersion: "containerd 2.3.4",
			Snapshotter: "overlayfs", CgroupsV2: true, CNIVersion: network.CNISpecVersion,
			CNIPluginsVersion: network.CNIPluginsVersion, Nftables: true,
		},
		Objects: make(map[string]Object), Allocations: make(map[string]model.AllocationDesired),
		Logs: make(map[string][]LogFrame), ExecFrames: make(map[string][]ExecFrame),
		EnsureErrors: make(map[string]error), waiters: make(map[string][]chan Object),
	}
}

func (f *Fake) PrepareNetwork(_ context.Context, config model.NetworkConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.NetworkError != nil {
		return f.NetworkError
	}
	if config.Generation != 0 {
		if f.PreparedNetwork != config {
			f.Operations = append(f.Operations, "network:"+config.NodeSubnet)
			f.PreparedNetwork = config
			if prefix, err := netip.ParsePrefix(config.NodeSubnet); err == nil {
				f.nextAddress = prefix.Addr().Next().Next()
				used := make(map[string]struct{}, len(f.Objects))
				for _, object := range f.Objects {
					used[object.Address] = struct{}{}
				}
				for {
					if _, exists := used[f.nextAddress.String()]; !exists {
						break
					}
					f.nextAddress = f.nextAddress.Next()
				}
			}
		}
	}
	return nil
}

func (f *Fake) Preflight(context.Context) (Capabilities, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return Capabilities{}, errors.New("fake runtime is closed")
	}
	capabilities := f.Capabilities
	if capabilities.Architecture == "native" {
		capabilities.Architecture = runtime.GOARCH
	}
	return capabilities, nil
}

func (f *Fake) ListOwned(context.Context) ([]Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]Object, 0, len(f.Objects))
	for _, object := range f.Objects {
		result = append(result, object)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (f *Fake) Ensure(_ context.Context, allocation model.AllocationDesired) (Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.EnsureErrors[allocation.ID]; err != nil {
		f.Operations = append(f.Operations, "ensure-error:"+allocation.ID)
		return Object{}, err
	}
	id := ObjectID(allocation.ID, allocation.SpecDigest)
	if current, ok := f.Objects[id]; ok {
		if current.AllocationID != allocation.ID || current.SpecDigest != allocation.SpecDigest {
			return Object{}, ErrOwnershipMismatch
		}
		if !current.Running {
			current.Running = true
			current.ExitCode = nil
			current.StartedAt = time.Now().UTC()
			f.Objects[id] = current
		}
		f.Operations = append(f.Operations, "adopt:"+id)
		return current, nil
	}
	address := ""
	if allocation.NetworkGeneration != 0 {
		if !f.nextAddress.IsValid() {
			return Object{}, fmt.Errorf("network generation %d is not prepared", allocation.NetworkGeneration)
		}
		address = f.nextAddress.String()
		f.nextAddress = f.nextAddress.Next()
	}
	object := Object{ID: id, AllocationID: allocation.ID, SpecDigest: allocation.SpecDigest, Running: true, StartedAt: time.Now().UTC(), Address: address, NetworkGeneration: allocation.NetworkGeneration}
	f.Objects[id] = object
	f.Allocations[id] = allocation
	f.Operations = append(f.Operations, "ensure:"+id)
	return object, nil
}

func (f *Fake) Inspect(_ context.Context, id string) (Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, ok := f.Objects[id]
	if !ok {
		return Object{}, ErrNotFound
	}
	return object, nil
}

func (f *Fake) Wait(ctx context.Context, id string) (<-chan Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Objects[id]; !ok {
		return nil, ErrNotFound
	}
	output := make(chan Object, 1)
	f.waiters[id] = append(f.waiters[id], output)
	go func() {
		<-ctx.Done()
	}()
	return output, nil
}

func (f *Fake) Stop(_ context.Context, id string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, ok := f.Objects[id]
	if !ok {
		return nil
	}
	if object.Running {
		exit := int32(143)
		object.Running = false
		object.ExitCode = &exit
		f.Objects[id] = object
		f.notifyWaiters(id, object)
	}
	f.Operations = append(f.Operations, "stop:"+id)
	return nil
}

func (f *Fake) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, ok := f.Objects[id]
	if !ok {
		return nil
	}
	if object.Running {
		return fmt.Errorf("remove running object: %w", ErrOwnershipMismatch)
	}
	delete(f.Objects, id)
	delete(f.Allocations, id)
	f.Operations = append(f.Operations, "remove:"+id)
	return nil
}

func (f *Fake) TailLogs(_ context.Context, id string, _ uint32) ([]LogFrame, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Objects[id]; !ok {
		return nil, ErrNotFound
	}
	frames := append([]LogFrame(nil), f.Logs[id]...)
	for index := range frames {
		frames[index].Data = append([]byte(nil), frames[index].Data...)
	}
	return frames, nil
}

func (f *Fake) Exec(ctx context.Context, id string, command []string) (<-chan ExecFrame, error) {
	f.mu.Lock()
	object, ok := f.Objects[id]
	if !ok || !object.Running {
		f.mu.Unlock()
		return nil, ErrNotFound
	}
	key := id
	if len(command) != 0 {
		key += ":" + command[0]
	}
	frames := append([]ExecFrame(nil), f.ExecFrames[key]...)
	f.Operations = append(f.Operations, "exec:"+id)
	f.mu.Unlock()
	output := make(chan ExecFrame, len(frames))
	go func() {
		defer close(output)
		for _, frame := range frames {
			select {
			case <-ctx.Done():
				return
			case output <- frame:
			}
		}
	}()
	return output, nil
}

func (f *Fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *Fake) Exit(id string, code int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, ok := f.Objects[id]
	if !ok {
		return ErrNotFound
	}
	object.Running = false
	object.ExitCode = &code
	f.Objects[id] = object
	f.notifyWaiters(id, object)
	return nil
}

func (f *Fake) InjectObject(object Object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Objects[object.ID] = object
}

func (f *Fake) SnapshotObjects() map[string]Object {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make(map[string]Object, len(f.Objects))
	for id, object := range f.Objects {
		result[id] = object
	}
	return result
}

func (f *Fake) SetLogs(id string, frames []LogFrame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Logs[id] = append([]LogFrame(nil), frames...)
}

func (f *Fake) SetExecFrames(id, command string, frames []ExecFrame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ExecFrames[id+":"+command] = append([]ExecFrame(nil), frames...)
}

func (f *Fake) SetEnsureError(allocationID string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.EnsureErrors, allocationID)
		return
	}
	f.EnsureErrors[allocationID] = err
}

func (f *Fake) SnapshotOperations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Operations...)
}

func (f *Fake) notifyWaiters(id string, object Object) {
	for _, waiter := range f.waiters[id] {
		waiter <- object
		close(waiter)
	}
	delete(f.waiters, id)
}
