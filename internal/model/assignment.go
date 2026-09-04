package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
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

	TCPRoutePending = "PENDING"
	TCPRouteActive  = "ACTIVE"
	TCPRouteFailed  = "FAILED"

	JobRunWaiting   = "WAITING"
	JobRunRunning   = "RUNNING"
	JobRunSucceeded = "SUCCEEDED"
	JobRunFailed    = "FAILED"
	JobRunTimedOut  = "TIMED_OUT"
	JobRunSkipped   = "SKIPPED"

	// PublicHTTPRoute is the stable route-status key for the catch-all port-80 listener.
	PublicHTTPRoute = "*"
)

type VolumeMount struct {
	Name     string `json:"name"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

type HTTPDesired struct {
	Port       uint32   `json:"port"`
	Public     bool     `json:"public,omitempty"`
	Domains    []string `json:"domains,omitempty"`
	HealthPath string   `json:"health_path,omitempty"`
	HTTPS      bool     `json:"https"`
}

type TCPDesired struct {
	Port       uint32 `json:"port"`
	PublicPort uint32 `json:"public_port,omitempty"`
}

// SecretReference is non-secret runtime intent. Version IDs are random and do
// not reveal or hash the referenced value.
type SecretReference struct {
	Name      string `json:"name"`
	VersionID string `json:"version_id"`
}

type SecretFileDesired struct {
	SecretReference
	Target string `json:"target"`
}

type RegistryCredentialReference struct {
	Host      string `json:"host"`
	VersionID string `json:"version_id"`
	Username  string `json:"username"`
}

const (
	SecretMaterialApplication = "application_secret"
	SecretMaterialRegistry    = "registry_credential"
)

// SecretMaterial exists only while delivering an assignment over mTLS or
// decrypting the agent cache. It is deliberately excluded from JSON.
type SecretMaterial struct {
	Kind        string `json:"-"`
	Application string `json:"-"`
	Name        string `json:"-"`
	VersionID   string `json:"-"`
	Host        string `json:"-"`
	Username    string `json:"-"`
	Value       []byte `json:"-"`
}

type RollbackSpec struct {
	ApplicationGeneration uint64                       `json:"application_generation"`
	RevisionID            string                       `json:"revision_id"`
	SpecDigest            string                       `json:"spec_digest"`
	Image                 string                       `json:"image"`
	Command               []string                     `json:"command,omitempty"`
	Args                  []string                     `json:"args,omitempty"`
	Environment           map[string]string            `json:"environment,omitempty"`
	RestartPolicy         string                       `json:"restart_policy"`
	CPUMillis             uint64                       `json:"cpu_millis"`
	MemoryBytes           uint64                       `json:"memory_bytes"`
	Mounts                []VolumeMount                `json:"mounts,omitempty"`
	StartDeadlineSeconds  uint32                       `json:"start_deadline_seconds"`
	HTTP                  *HTTPDesired                 `json:"http,omitempty"`
	TCP                   *TCPDesired                  `json:"tcp,omitempty"`
	DependsOn             []string                     `json:"depends_on,omitempty"`
	SecretEnvironment     map[string]SecretReference   `json:"secret_environment,omitempty"`
	SecretFiles           []SecretFileDesired          `json:"secret_files,omitempty"`
	RegistryCredential    *RegistryCredentialReference `json:"registry_credential,omitempty"`
	NetworkGeneration     uint64                       `json:"network_generation"`
	AutoRollback          bool                         `json:"auto_rollback"`
	StartBeforeStop       bool                         `json:"start_before_stop"`
}

type NetworkConfig struct {
	Generation          uint64            `json:"generation"`
	ClusterCIDR         string            `json:"cluster_cidr"`
	NodeSubnet          string            `json:"node_subnet"`
	Gateway             string            `json:"gateway"`
	DNSZone             string            `json:"dns_zone"`
	WireGuardListenPort uint16            `json:"wireguard_listen_port,omitempty"`
	Peers               []WireGuardPeer   `json:"peers,omitempty"`
	Endpoints           []ServiceEndpoint `json:"endpoints,omitempty"`
	IngressEnabled      bool              `json:"ingress_enabled,omitempty"`
}

type ServiceEndpoint struct {
	AllocationID  string   `json:"allocation_id"`
	Application   string   `json:"application"`
	Service       string   `json:"service"`
	Address       string   `json:"address"`
	Port          uint32   `json:"port"`
	NodeID        string   `json:"node_id"`
	Domains       []string `json:"domains,omitempty"`
	HTTPPublic    bool     `json:"http_public,omitempty"`
	HTTPS         bool     `json:"https,omitempty"`
	TCPPublicPort uint32   `json:"tcp_public_port,omitempty"`
}

func NetworkConfigEqual(left, right NetworkConfig) bool { return reflect.DeepEqual(left, right) }

// AllocationDesired is a runtime-independent, canonical unit of work.
type AllocationDesired struct {
	ID                    string                       `json:"id"`
	Application           string                       `json:"application"`
	Service               string                       `json:"service"`
	Replica               uint32                       `json:"replica"`
	ApplicationGeneration uint64                       `json:"application_generation"`
	RevisionID            string                       `json:"revision_id"`
	SpecDigest            string                       `json:"spec_digest"`
	Image                 string                       `json:"image"`
	Command               []string                     `json:"command,omitempty"`
	Args                  []string                     `json:"args,omitempty"`
	Environment           map[string]string            `json:"environment,omitempty"`
	RestartPolicy         string                       `json:"restart_policy"`
	CPUMillis             uint64                       `json:"cpu_millis"`
	MemoryBytes           uint64                       `json:"memory_bytes"`
	Mounts                []VolumeMount                `json:"mounts,omitempty"`
	StartDeadlineSeconds  uint32                       `json:"start_deadline_seconds"`
	AutoRollback          bool                         `json:"auto_rollback"`
	StartBeforeStop       bool                         `json:"start_before_stop"`
	HTTP                  *HTTPDesired                 `json:"http,omitempty"`
	TCP                   *TCPDesired                  `json:"tcp,omitempty"`
	DependsOn             []string                     `json:"depends_on,omitempty"`
	SecretEnvironment     map[string]SecretReference   `json:"secret_environment,omitempty"`
	SecretFiles           []SecretFileDesired          `json:"secret_files,omitempty"`
	RegistryCredential    *RegistryCredentialReference `json:"registry_credential,omitempty"`
	NetworkGeneration     uint64                       `json:"network_generation"`
	MaxSurge              uint32                       `json:"max_surge"`
	MaxUnavailable        uint32                       `json:"max_unavailable"`
	Rollback              *RollbackSpec                `json:"rollback,omitempty"`
	// RuntimeSecretValues and RuntimeRegistrySecrets are populated only after
	// the agent decrypts its cache and are never serialized into assignments.
	RuntimeSecretValues    map[string][]byte `json:"-"`
	RuntimeRegistrySecrets map[string][]byte `json:"-"`
}

// CronJobDesired is an immutable one-shot workload template distributed to
// the agent. Occurrence IDs are derived and claimed by the agent scheduler.
type CronJobDesired struct {
	Application            string                       `json:"application"`
	Job                    string                       `json:"job"`
	ApplicationGeneration  uint64                       `json:"application_generation"`
	RevisionID             string                       `json:"revision_id"`
	SpecDigest             string                       `json:"spec_digest"`
	Image                  string                       `json:"image"`
	Schedule               string                       `json:"schedule"`
	Command                []string                     `json:"command,omitempty"`
	Args                   []string                     `json:"args,omitempty"`
	Environment            map[string]string            `json:"environment,omitempty"`
	CPUMillis              uint64                       `json:"cpu_millis"`
	MemoryBytes            uint64                       `json:"memory_bytes"`
	DependsOn              []string                     `json:"depends_on,omitempty"`
	SecretEnvironment      map[string]SecretReference   `json:"secret_environment,omitempty"`
	SecretFiles            []SecretFileDesired          `json:"secret_files,omitempty"`
	RegistryCredential     *RegistryCredentialReference `json:"registry_credential,omitempty"`
	StartDeadlineSeconds   uint32                       `json:"start_deadline_seconds"`
	TimeoutSeconds         uint32                       `json:"timeout_seconds"`
	NetworkGeneration      uint64                       `json:"network_generation"`
	RuntimeSecretValues    map[string][]byte            `json:"-"`
	RuntimeRegistrySecrets map[string][]byte            `json:"-"`
}

type Assignment struct {
	NodeID      string              `json:"node_id"`
	Generation  uint64              `json:"generation"`
	Digest      string              `json:"digest"`
	Payload     json.RawMessage     `json:"payload"`
	Allocations []AllocationDesired `json:"allocations"`
	CronJobs    []CronJobDesired    `json:"cron_jobs,omitempty"`
	Materials   []SecretMaterial    `json:"-"`
	Network     NetworkConfig       `json:"network"`
	CreatedAt   time.Time           `json:"created_at"`
}

type TCPRouteStatus struct {
	NodeID        string    `json:"node_id,omitempty"`
	NodeName      string    `json:"node_name,omitempty"`
	Application   string    `json:"application"`
	Service       string    `json:"service"`
	PublicPort    uint32    `json:"public_port"`
	TargetPort    uint32    `json:"target_port"`
	State         string    `json:"state"`
	UpstreamCount uint32    `json:"upstream_count"`
	Reason        string    `json:"reason,omitempty"`
	Message       string    `json:"message,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type CronJobStatus struct {
	Application string     `json:"application"`
	Job         string     `json:"job"`
	SpecDigest  string     `json:"spec_digest"`
	NextRunAt   *time.Time `json:"next_run_at,omitempty"`
	ActiveRunID string     `json:"active_run_id,omitempty"`
	LastRunID   string     `json:"last_run_id,omitempty"`
	LastState   string     `json:"last_state,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	Message     string     `json:"message,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

type JobRunStatus struct {
	ID          string     `json:"id"`
	Application string     `json:"application"`
	Job         string     `json:"job"`
	SpecDigest  string     `json:"spec_digest"`
	ScheduledAt time.Time  `json:"scheduled_at"`
	State       string     `json:"state"`
	ExitCode    *int32     `json:"exit_code,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	Message     string     `json:"message,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

type AllocationStatus struct {
	ID                    string     `json:"id"`
	NodeID                string     `json:"node_id,omitempty"`
	NodeName              string     `json:"node_name,omitempty"`
	PlacementReason       string     `json:"placement_reason,omitempty"`
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
	NodeID           string    `json:"node_id,omitempty"`
	NodeName         string    `json:"node_name,omitempty"`
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
	CronJobs          []CronJobDesired
	CreatedAt         time.Time
}

type AgentCapabilities struct {
	NodeID                 string
	BootID                 string
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
	SeenAt                 time.Time
}

type AgentReport struct {
	NodeID               string
	BootID               string
	Sequence             uint64
	AssignmentGeneration uint64
	AssignmentDigest     string
	Allocations          []AllocationStatus
	Routes               []RouteStatus
	TCPRoutes            []TCPRouteStatus
	CronJobs             []CronJobStatus
	JobRuns              []JobRunStatus
	Network              NetworkStatus
	ReportedAt           time.Time
}

// AllocationSpecDigest hashes only runtime-affecting fields. Identity and
// application revision metadata remain outside the digest so unaffected
// allocations survive an application update.
func AllocationSpecDigest(allocation AllocationDesired) (string, error) {
	spec := struct {
		Image                string                     `json:"image"`
		Command              []string                   `json:"command,omitempty"`
		Args                 []string                   `json:"args,omitempty"`
		Environment          map[string]string          `json:"environment,omitempty"`
		RestartPolicy        string                     `json:"restart_policy"`
		CPUMillis            uint64                     `json:"cpu_millis"`
		MemoryBytes          uint64                     `json:"memory_bytes"`
		Mounts               []VolumeMount              `json:"mounts,omitempty"`
		StartDeadlineSeconds uint32                     `json:"start_deadline_seconds"`
		AutoRollback         bool                       `json:"auto_rollback"`
		StartBeforeStop      bool                       `json:"start_before_stop"`
		HTTP                 *HTTPDesired               `json:"http,omitempty"`
		TCP                  *TCPDesired                `json:"tcp,omitempty"`
		DependsOn            []string                   `json:"depends_on,omitempty"`
		SecretEnvironment    map[string]SecretReference `json:"secret_environment,omitempty"`
		SecretFiles          []SecretFileDesired        `json:"secret_files,omitempty"`
		NetworkGeneration    uint64                     `json:"network_generation"`
	}{allocation.Image, allocation.Command, allocation.Args, allocation.Environment,
		allocation.RestartPolicy, allocation.CPUMillis, allocation.MemoryBytes, allocation.Mounts,
		allocation.StartDeadlineSeconds, allocation.AutoRollback, allocation.StartBeforeStop,
		allocation.HTTP, allocation.TCP, allocation.DependsOn, allocation.SecretEnvironment,
		allocation.SecretFiles, allocation.NetworkGeneration}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("encode allocation runtime spec: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func CronJobSpecDigest(job CronJobDesired) (string, error) {
	spec := struct {
		Image                string                     `json:"image"`
		Schedule             string                     `json:"schedule"`
		Command              []string                   `json:"command,omitempty"`
		Args                 []string                   `json:"args,omitempty"`
		Environment          map[string]string          `json:"environment,omitempty"`
		CPUMillis            uint64                     `json:"cpu_millis"`
		MemoryBytes          uint64                     `json:"memory_bytes"`
		DependsOn            []string                   `json:"depends_on,omitempty"`
		SecretEnvironment    map[string]SecretReference `json:"secret_environment,omitempty"`
		SecretFiles          []SecretFileDesired        `json:"secret_files,omitempty"`
		StartDeadlineSeconds uint32                     `json:"start_deadline_seconds"`
		TimeoutSeconds       uint32                     `json:"timeout_seconds"`
		NetworkGeneration    uint64                     `json:"network_generation"`
	}{job.Image, job.Schedule, job.Command, job.Args, job.Environment, job.CPUMillis,
		job.MemoryBytes, job.DependsOn, job.SecretEnvironment, job.SecretFiles,
		job.StartDeadlineSeconds, job.TimeoutSeconds, job.NetworkGeneration}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("encode cron job runtime spec: %w", err)
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
	if allocation.NetworkGeneration != 0 || allocation.HTTP != nil || allocation.TCP != nil || len(allocation.DependsOn) != 0 || allocation.Rollback != nil ||
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
