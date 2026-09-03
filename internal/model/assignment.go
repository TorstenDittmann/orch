package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const (
	AllocationPending  = "PENDING"
	AllocationPulling  = "PULLING"
	AllocationStarting = "STARTING"
	AllocationRunning  = "RUNNING"
	AllocationStopped  = "STOPPED"
	AllocationBackoff  = "BACKOFF"
	AllocationFailed   = "FAILED"

	HealthNotConfigured = "NOT_CONFIGURED"
	HealthChecking      = "CHECKING"
	HealthReady         = "READY"
	HealthUnhealthy     = "UNHEALTHY"

	NetworkPending = "PENDING"
	NetworkReady   = "READY"
	NetworkFailed  = "FAILED"

	SubsystemPending       = "pending"
	SubsystemReady         = "ready"
	SubsystemFailed        = "failed"
	SubsystemBlocked       = "blocked"
	SubsystemNotConfigured = "not_configured"

	RoutePending = "PENDING"
	RouteActive  = "ACTIVE"
	RouteFailed  = "FAILED"
)

type VolumeMount struct {
	Name     string `json:"name"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

type HTTPDesired struct {
	Port       uint32   `json:"port"`
	Domains    []string `json:"domains,omitempty"`
	HealthPath string   `json:"health_path,omitempty"`
	HTTPS      bool     `json:"https"`
}

type RollbackSpec struct {
	ApplicationGeneration uint64            `json:"application_generation"`
	RevisionID            string            `json:"revision_id"`
	SpecDigest            string            `json:"spec_digest"`
	Image                 string            `json:"image"`
	Command               []string          `json:"command,omitempty"`
	Args                  []string          `json:"args,omitempty"`
	Environment           map[string]string `json:"environment,omitempty"`
	RestartPolicy         string            `json:"restart_policy"`
	CPUMillis             uint64            `json:"cpu_millis"`
	MemoryBytes           uint64            `json:"memory_bytes"`
	Mounts                []VolumeMount     `json:"mounts,omitempty"`
	StartDeadlineSeconds  uint32            `json:"start_deadline_seconds"`
	HTTP                  *HTTPDesired      `json:"http,omitempty"`
	NetworkGeneration     uint64            `json:"network_generation"`
	AutoRollback          bool              `json:"auto_rollback"`
	StartBeforeStop       bool              `json:"start_before_stop"`
}

type NetworkConfig struct {
	Generation  uint64 `json:"generation"`
	ClusterCIDR string `json:"cluster_cidr"`
	NodeSubnet  string `json:"node_subnet"`
	Gateway     string `json:"gateway"`
	DNSZone     string `json:"dns_zone"`
}

// AllocationDesired is a runtime-independent, canonical unit of work.
type AllocationDesired struct {
	ID                    string            `json:"id"`
	Application           string            `json:"application"`
	Service               string            `json:"service"`
	Replica               uint32            `json:"replica"`
	ApplicationGeneration uint64            `json:"application_generation"`
	RevisionID            string            `json:"revision_id"`
	SpecDigest            string            `json:"spec_digest"`
	Image                 string            `json:"image"`
	Command               []string          `json:"command,omitempty"`
	Args                  []string          `json:"args,omitempty"`
	Environment           map[string]string `json:"environment,omitempty"`
	RestartPolicy         string            `json:"restart_policy"`
	CPUMillis             uint64            `json:"cpu_millis"`
	MemoryBytes           uint64            `json:"memory_bytes"`
	Mounts                []VolumeMount     `json:"mounts,omitempty"`
	StartDeadlineSeconds  uint32            `json:"start_deadline_seconds"`
	AutoRollback          bool              `json:"auto_rollback"`
	StartBeforeStop       bool              `json:"start_before_stop"`
	HTTP                  *HTTPDesired      `json:"http,omitempty"`
	NetworkGeneration     uint64            `json:"network_generation"`
	MaxSurge              uint32            `json:"max_surge"`
	MaxUnavailable        uint32            `json:"max_unavailable"`
	Rollback              *RollbackSpec     `json:"rollback,omitempty"`
}

type Assignment struct {
	NodeID      string              `json:"node_id"`
	Generation  uint64              `json:"generation"`
	Digest      string              `json:"digest"`
	Payload     json.RawMessage     `json:"payload"`
	Allocations []AllocationDesired `json:"allocations"`
	Network     NetworkConfig       `json:"network"`
	CreatedAt   time.Time           `json:"created_at"`
}

type AllocationStatus struct {
	ID                    string     `json:"id"`
	Application           string     `json:"application"`
	Service               string     `json:"service"`
	Replica               uint32     `json:"replica"`
	ApplicationGeneration uint64     `json:"application_generation"`
	SpecDigest            string     `json:"spec_digest"`
	Phase                 string     `json:"phase"`
	Reason                string     `json:"reason,omitempty"`
	Message               string     `json:"message,omitempty"`
	ExitCode              *int32     `json:"exit_code,omitempty"`
	RestartCount          uint32     `json:"restart_count"`
	UpdatedAt             time.Time  `json:"updated_at"`
	HealthStatus          string     `json:"health_status"`
	Address               string     `json:"address,omitempty"`
	ActiveSpecDigest      string     `json:"active_spec_digest,omitempty"`
	ActiveGeneration      uint64     `json:"active_application_generation,omitempty"`
	ActiveRevisionID      string     `json:"active_revision_id,omitempty"`
	ReadyAt               *time.Time `json:"ready_at,omitempty"`
	DeclaredPort          uint32     `json:"declared_port,omitempty"`
	ConsecutiveSuccesses  uint32     `json:"-"`
	ConsecutiveFailures   uint32     `json:"-"`
}

type RouteStatus struct {
	Application      string    `json:"application"`
	Service          string    `json:"service"`
	Domain           string    `json:"domain"`
	HTTPS            bool      `json:"https"`
	State            string    `json:"state"`
	CertificateState string    `json:"certificate_state,omitempty"`
	UpstreamCount    uint32    `json:"upstream_count"`
	Reason           string    `json:"reason,omitempty"`
	Message          string    `json:"message,omitempty"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type NetworkStatus struct {
	Generation      uint64 `json:"generation"`
	State           string `json:"state"`
	ClusterCIDR     string `json:"cluster_cidr"`
	NodeSubnet      string `json:"node_subnet"`
	Gateway         string `json:"gateway"`
	Reason          string `json:"reason,omitempty"`
	Message         string `json:"message,omitempty"`
	DNSState        string `json:"dns_state,omitempty"`
	IngressState    string `json:"ingress_state,omitempty"`
	DrainingObjects uint32 `json:"draining_objects"`
}

type PublishAssignment struct {
	NodeID            string
	Digest            string
	Payload           []byte
	Allocations       []AllocationDesired
	Network           NetworkConfig
	ApplicationStates map[string]ApplicationExecution
	CreatedAt         time.Time
}

type AgentCapabilities struct {
	NodeID            string
	BootID            string
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
	SeenAt            time.Time
}

type AgentReport struct {
	NodeID               string
	BootID               string
	Sequence             uint64
	AssignmentGeneration uint64
	AssignmentDigest     string
	Allocations          []AllocationStatus
	Routes               []RouteStatus
	Network              NetworkStatus
	ReportedAt           time.Time
}

// AllocationSpecDigest hashes only runtime-affecting fields. Identity and
// application revision metadata remain outside the digest so unaffected
// allocations survive an application update.
func AllocationSpecDigest(allocation AllocationDesired) (string, error) {
	spec := struct {
		Image                string            `json:"image"`
		Command              []string          `json:"command,omitempty"`
		Args                 []string          `json:"args,omitempty"`
		Environment          map[string]string `json:"environment,omitempty"`
		RestartPolicy        string            `json:"restart_policy"`
		CPUMillis            uint64            `json:"cpu_millis"`
		MemoryBytes          uint64            `json:"memory_bytes"`
		Mounts               []VolumeMount     `json:"mounts,omitempty"`
		StartDeadlineSeconds uint32            `json:"start_deadline_seconds"`
		AutoRollback         bool              `json:"auto_rollback"`
		StartBeforeStop      bool              `json:"start_before_stop"`
		HTTP                 *HTTPDesired      `json:"http,omitempty"`
		NetworkGeneration    uint64            `json:"network_generation"`
	}{allocation.Image, allocation.Command, allocation.Args, allocation.Environment,
		allocation.RestartPolicy, allocation.CPUMillis, allocation.MemoryBytes, allocation.Mounts,
		allocation.StartDeadlineSeconds, allocation.AutoRollback, allocation.StartBeforeStop,
		allocation.HTTP, allocation.NetworkGeneration}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("encode allocation runtime spec: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// Plan4AllocationSpecDigest reproduces the immutable digest written by the
// process-only runtime from Plan #4. It exists solely so the network upgrade
// can retain and restart already-authorized workloads until the controller
// freezes the first network generation and publishes their replacements.
func Plan4AllocationSpecDigest(allocation AllocationDesired) (string, error) {
	spec := struct {
		Image                string            `json:"image"`
		Command              []string          `json:"command,omitempty"`
		Args                 []string          `json:"args,omitempty"`
		Environment          map[string]string `json:"environment,omitempty"`
		RestartPolicy        string            `json:"restart_policy"`
		CPUMillis            uint64            `json:"cpu_millis"`
		MemoryBytes          uint64            `json:"memory_bytes"`
		Mounts               []VolumeMount     `json:"mounts,omitempty"`
		StartDeadlineSeconds uint32            `json:"start_deadline_seconds"`
		AutoRollback         bool              `json:"auto_rollback"`
		StartBeforeStop      bool              `json:"start_before_stop"`
	}{allocation.Image, allocation.Command, allocation.Args, allocation.Environment,
		allocation.RestartPolicy, allocation.CPUMillis, allocation.MemoryBytes, allocation.Mounts,
		allocation.StartDeadlineSeconds, allocation.AutoRollback, allocation.StartBeforeStop}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("encode Plan #4 allocation runtime spec: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// AllocationSpecDigestMatches accepts the current digest and the narrowly
// defined process-only predecessor. New network, HTTP, rollout-budget, or
// rollback fields can never be authorized by the legacy digest.
func AllocationSpecDigestMatches(allocation AllocationDesired) (bool, error) {
	current, err := AllocationSpecDigest(allocation)
	if err != nil || current == allocation.SpecDigest {
		return current == allocation.SpecDigest, err
	}
	legacy, err := IsPlan4Allocation(allocation)
	return legacy, err
}

// IsPlan4Allocation reports whether the supplied digest is the exact legacy
// digest and all fields added by the network slice remain unset.
func IsPlan4Allocation(allocation AllocationDesired) (bool, error) {
	if allocation.NetworkGeneration != 0 || allocation.HTTP != nil || allocation.Rollback != nil ||
		allocation.MaxSurge != 0 || allocation.MaxUnavailable != 0 {
		return false, nil
	}
	current, err := AllocationSpecDigest(allocation)
	if err != nil || current == allocation.SpecDigest {
		return false, err
	}
	legacy, err := Plan4AllocationSpecDigest(allocation)
	if err != nil {
		return false, err
	}
	return legacy == allocation.SpecDigest, nil
}
