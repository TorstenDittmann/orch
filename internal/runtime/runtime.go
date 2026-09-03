package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/network"
)

var (
	ErrNotFound          = errors.New("runtime object was not found")
	ErrOwnershipMismatch = errors.New("runtime object ownership does not match the agent")
	ErrUnsupported       = errors.New("runtime is unsupported on this platform")
)

type Capabilities struct {
	OperatingSystem   string
	Architecture      string
	RuntimeVersion    string
	Snapshotter       string
	CgroupsV2         bool
	CNIVersion        string
	CNIPluginsVersion string
	Nftables          bool
	CaddyVersion      string
	DNSAvailable      bool
}

type Options struct {
	ClusterID    string
	NodeID       string
	Snapshotter  string
	LogsDir      string
	VolumesDir   string
	SecretsDir   string
	LoggerPath   string
	ResolverPath string
	Network      network.Manager
}

type Object struct {
	ID                string
	AllocationID      string
	SpecDigest        string
	Running           bool
	ExitCode          *int32
	StartedAt         time.Time
	Address           string
	NetworkGeneration uint64
}

type LogFrame struct {
	Stream    string
	Data      []byte
	Offset    uint64
	Truncated bool
}

type ExecFrame struct {
	Stream   string
	Data     []byte
	Exited   bool
	ExitCode int32
}

// Runtime is the only boundary between the agent and containerd. All methods
// operate only within the configured namespace and verified ownership labels.
type Runtime interface {
	Preflight(context.Context) (Capabilities, error)
	PrepareNetwork(context.Context, model.NetworkConfig) error
	ListOwned(context.Context) ([]Object, error)
	Ensure(context.Context, model.AllocationDesired) (Object, error)
	Inspect(context.Context, string) (Object, error)
	Wait(context.Context, string) (<-chan Object, error)
	Stop(context.Context, string, time.Duration) error
	Remove(context.Context, string) error
	TailLogs(context.Context, string, uint32) ([]LogFrame, error)
	Exec(context.Context, string, []string) (<-chan ExecFrame, error)
	Close() error
}

func ObjectID(allocationID, specDigest string) string {
	if len(specDigest) >= 19 {
		return allocationID + "-" + specDigest[7:19]
	}
	return allocationID + "-invalid"
}

func ValidateOptions(options Options) error {
	if options.ClusterID == "" || options.NodeID == "" || options.Snapshotter == "" || options.LogsDir == "" || options.VolumesDir == "" {
		return fmt.Errorf("cluster, node, snapshotter, log, and volume options are required")
	}
	return nil
}
