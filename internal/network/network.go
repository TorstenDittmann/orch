package network

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
)

var (
	ErrNotFound          = errors.New("network attachment was not found")
	ErrOwnershipMismatch = errors.New("network ownership does not match")
	ErrCIDRConflict      = errors.New("workload CIDR conflicts with host networking")
	ErrUnsupported       = errors.New("workload networking is unsupported on this platform")
)

const (
	CNISpecVersion    = "1.1.0"
	CNIPluginsVersion = "v1.9.1"
)

type Capabilities struct {
	CNIVersion        string
	CNIPluginsVersion string
	Nftables          bool
}

type Options struct {
	ClusterID string
	NodeID    string
	BinDir    string
	StateDir  string
}

type AttachmentRequest struct {
	AllocationID      string
	ObjectID          string
	SpecDigest        string
	NetworkGeneration uint64
	NetNS             string
	IfName            string
}

type Attachment struct {
	AllocationID      string
	ObjectID          string
	SpecDigest        string
	NetworkGeneration uint64
	Address           netip.Addr
	NetNS             string
	CreatedAt         time.Time
}

type AttachmentStore interface {
	SaveAttachment(context.Context, Attachment) error
	Attachment(context.Context, string) (Attachment, error)
	Attachments(context.Context) ([]Attachment, error)
	DeleteAttachment(context.Context, string) error
}

type Manager interface {
	Preflight(context.Context) (Capabilities, error)
	Prepare(context.Context, model.NetworkConfig) error
	Attach(context.Context, AttachmentRequest) (Attachment, error)
	Inspect(context.Context, AttachmentRequest) (Attachment, error)
	Check(context.Context, AttachmentRequest) error
	GC(context.Context, []AttachmentRequest) error
	Detach(context.Context, AttachmentRequest) error
	Close() error
}

func ValidateOptions(options Options) error {
	for name, value := range map[string]string{
		"cluster id": options.ClusterID, "node id": options.NodeID,
		"CNI binary directory": options.BinDir, "network state directory": options.StateDir,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	return nil
}

type Fake struct {
	mu           sync.Mutex
	Capabilities Capabilities
	PreflightErr error
	PrepareErr   error
	AttachErr    error
	Config       model.NetworkConfig
	Attachments  map[string]Attachment
	next         netip.Addr
}

func NewFake() *Fake {
	return &Fake{Capabilities: Capabilities{
		CNIVersion: CNISpecVersion, CNIPluginsVersion: CNIPluginsVersion, Nftables: true,
	}, Attachments: make(map[string]Attachment)}
}

func (f *Fake) Preflight(context.Context) (Capabilities, error) {
	return f.Capabilities, f.PreflightErr
}

func (f *Fake) Prepare(_ context.Context, config model.NetworkConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.PrepareErr != nil {
		return f.PrepareErr
	}
	prefix, err := netip.ParsePrefix(config.NodeSubnet)
	if err != nil || config.Generation == 0 || !prefix.Addr().Is4() {
		return fmt.Errorf("invalid fake network configuration")
	}
	if f.Config == config {
		return nil
	}
	f.Config = config
	f.next = prefix.Addr().Next().Next()
	return nil
}

func (f *Fake) Attach(_ context.Context, request AttachmentRequest) (Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.AttachErr != nil {
		return Attachment{}, f.AttachErr
	}
	if current, ok := f.Attachments[request.ObjectID]; ok {
		if current.AllocationID != request.AllocationID || current.SpecDigest != request.SpecDigest {
			return Attachment{}, ErrOwnershipMismatch
		}
		return current, nil
	}
	if f.Config.Generation == 0 || request.NetworkGeneration != f.Config.Generation {
		return Attachment{}, fmt.Errorf("network is not prepared for generation %d", request.NetworkGeneration)
	}
	attachment := Attachment{AllocationID: request.AllocationID, ObjectID: request.ObjectID,
		SpecDigest: request.SpecDigest, NetworkGeneration: request.NetworkGeneration,
		Address: f.next, NetNS: request.NetNS}
	f.next = f.next.Next()
	f.Attachments[request.ObjectID] = attachment
	return attachment, nil
}

func (f *Fake) Inspect(_ context.Context, request AttachmentRequest) (Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	attachment, ok := f.Attachments[request.ObjectID]
	if !ok {
		return Attachment{}, ErrNotFound
	}
	if attachment.AllocationID != request.AllocationID || attachment.SpecDigest != request.SpecDigest {
		return Attachment{}, ErrOwnershipMismatch
	}
	return attachment, nil
}

func (f *Fake) Check(_ context.Context, request AttachmentRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	attachment, ok := f.Attachments[request.ObjectID]
	if !ok {
		return ErrNotFound
	}
	if attachment.AllocationID != request.AllocationID || attachment.SpecDigest != request.SpecDigest ||
		attachment.NetworkGeneration != request.NetworkGeneration {
		return ErrOwnershipMismatch
	}
	return nil
}

func (f *Fake) GC(_ context.Context, valid []AttachmentRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	keep := make(map[string]struct{}, len(valid))
	for _, request := range valid {
		keep[request.ObjectID] = struct{}{}
	}
	for objectID := range f.Attachments {
		if _, ok := keep[objectID]; !ok {
			delete(f.Attachments, objectID)
		}
	}
	return nil
}

func (f *Fake) Detach(_ context.Context, request AttachmentRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.Attachments[request.ObjectID]
	if !ok {
		return nil
	}
	if current.AllocationID != request.AllocationID || current.SpecDigest != request.SpecDigest {
		return ErrOwnershipMismatch
	}
	delete(f.Attachments, request.ObjectID)
	return nil
}

func (*Fake) Close() error { return nil }
