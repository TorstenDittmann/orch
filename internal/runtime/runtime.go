package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/network"
)

var (
	ErrNotFound          = errors.New("runtime object was not found")
	ErrOwnershipMismatch = errors.New("runtime object ownership does not match the agent")
	ErrUnsupported       = errors.New("runtime is unsupported on this platform")
)

// ImagePullError reports that a workload image could not be pulled. Its
// message is safe to show to operators: it never contains credentials.
type ImagePullError struct {
	Image string
	Err   error
}

func (e *ImagePullError) Error() string { return fmt.Sprintf("pull image %q: %v", e.Image, e.Err) }

func (e *ImagePullError) Unwrap() error { return e.Err }

type Capabilities struct {
	OperatingSystem        string
	Architecture           string
	RuntimeVersion         string
	Snapshotter            string
	CgroupsV2              bool
	CNIVersion             string
	CNIPluginsVersion      string
	Nftables               bool
	CaddyVersion           string
	DNSAvailable           bool
	TotalCPUMillis         uint64
	TotalMemoryBytes       uint64
	AllocatableCPUMillis   uint64
	AllocatableMemoryBytes uint64
	AdvertiseAddress       string
	WireGuardPublicKey     string
	WireGuardListenPort    uint32
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

// DefaultStopGrace is how long a task may take to exit after SIGTERM when its
// allocation does not set a stop grace period. MaxStopGraceSeconds bounds it.
const (
	DefaultStopGrace    = 10 * time.Second
	MaxStopGraceSeconds = 600
)

type Object struct {
	ID           string
	AllocationID string
	SpecDigest   string
	Running      bool
	ExitCode     *int32
	// StartedAt is when the current task was started, not when the container
	// was created, so it moves forward on every restart.
	StartedAt time.Time
	Address   string
	// StopGrace is the allocation's stop grace period, or zero for the
	// default. It is recorded on the object so a removed allocation still
	// stops with the grace period it was started with.
	StopGrace         time.Duration
	NetworkGeneration uint64
}

type LogFrame struct {
	Stream    string
	Data      []byte
	Offset    uint64
	Truncated bool
}

type LogBatch struct {
	Frames      []LogFrame
	NextCursor  string
	CursorReset bool
	Exited      bool
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
	ReadLogs(context.Context, string, uint32, string) (LogBatch, error)
	// ReadPreviousLogs reads the logs of an allocation's most recently
	// removed runtime object, which are kept for a while after removal.
	ReadPreviousLogs(context.Context, string, uint32, string) (LogBatch, error)
	Exec(context.Context, string, []string) (<-chan ExecFrame, error)
	Close() error
}

// TaskCPU is the CPU time one running task has used since it started.
type TaskCPU struct {
	ObjectID     string
	AllocationID string
	UsageMicros  uint64
	ObservedAt   time.Time
}

// TaskCPUReader is implemented by runtimes that can read their tasks' CPU
// use. It is separate from Runtime because usage is best effort.
type TaskCPUReader interface {
	TaskCPU(context.Context) ([]TaskCPU, error)
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
