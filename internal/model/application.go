package model

import (
	"encoding/json"
	"time"
)

const (
	EventApplicationDesiredUpdated   = "application.desired.updated"
	EventApplicationDesiredRemoved   = "application.desired.removed"
	EventApplicationExecutionChanged = "application.execution.changed"
	EventApplicationHealthy          = "application.healthy"
	EventApplicationRolledBack       = "application.rolled_back"
	EventServiceScaled               = "service.scaled"
	EventDeploymentFinished          = "deployment.finished"
	EventCronRunFailed               = "cron.run.failed"
	EventVolumeBackupFailed          = "volume.backup.failed"

	ExecutionPending    = "PENDING"
	ExecutionBlocked    = "BLOCKED"
	ExecutionStarting   = "STARTING"
	ExecutionRunning    = "RUNNING"
	ExecutionDegraded   = "DEGRADED"
	ExecutionFailed     = "FAILED"
	ExecutionStopping   = "STOPPING"
	ExecutionRemoved    = "REMOVED"
	ExecutionHealthy    = "HEALTHY"
	ExecutionRolledBack = "ROLLED_BACK"
)

// Application is the current durable desired state of one application.
type Application struct {
	Name              string               `json:"name"`
	Generation        uint64               `json:"generation"`
	RevisionID        string               `json:"revision_id"`
	Digest            string               `json:"digest"`
	CanonicalManifest json.RawMessage      `json:"canonical_manifest"`
	CreatedAt         time.Time            `json:"created_at"`
	UpdatedAt         time.Time            `json:"updated_at"`
	DeletedAt         *time.Time           `json:"deleted_at,omitempty"`
	Execution         ApplicationExecution `json:"execution"`
}

// ApplicationExecution is the controller's current observation of one
// application's desired generation. RUNNING is process state, not readiness.
type ApplicationExecution struct {
	Status             string             `json:"status"`
	ObservedGeneration uint64             `json:"observed_generation"`
	Reason             string             `json:"reason"`
	Message            string             `json:"message"`
	UpdatedAt          time.Time          `json:"updated_at"`
	Allocations        []AllocationStatus `json:"allocations,omitempty"`
	ActiveGeneration   uint64             `json:"active_generation,omitempty"`
	ActiveRevisionID   string             `json:"active_revision_id,omitempty"`
	Routes             []RouteStatus      `json:"routes,omitempty"`
	TCPRoutes          []TCPRouteStatus   `json:"tcp_routes,omitempty"`
	NodePorts          []NodePortStatus   `json:"node_ports,omitempty"`
	CronJobs           []CronJobStatus    `json:"cron_jobs,omitempty"`
	JobRuns            []JobRunStatus     `json:"job_runs,omitempty"`
}

// ApplyApplication contains validated, canonical application intent.
type ApplyApplication struct {
	Name              string
	RevisionID        string
	Digest            string
	CanonicalManifest []byte
	// RequestedManifest retains authored image references before resolution.
	RequestedManifest []byte
	IdempotencyKey    string
	CreatedAt         time.Time
	// Rollback requests are guarded against concurrent desired-state changes.
	ExpectedGeneration     uint64
	RollbackFromRevisionID string
	// Source records where the deployment came from. It is history metadata
	// and never part of the digest or idempotent replay.
	Source *DeploymentSource
}

// DeploymentSource describes the commit and CI run behind a deployment.
type DeploymentSource struct {
	Repository string `json:"repository,omitempty"`
	Commit     string `json:"commit,omitempty"`
	Ref        string `json:"ref,omitempty"`
	RunURL     string `json:"run_url,omitempty"`
	Actor      string `json:"actor,omitempty"`
}

// DeploymentFinished is the payload of a deployment.finished event.
type DeploymentFinished struct {
	Application string `json:"application"`
	RevisionID  string `json:"revision_id"`
	Outcome     string `json:"outcome"`
	Reason      string `json:"reason"`
	Message     string `json:"message"`
}

// CronRunFailed is the payload of a cron.run.failed event.
type CronRunFailed struct {
	Application string `json:"application"`
	Job         string `json:"job"`
	RunID       string `json:"run_id"`
	State       string `json:"state"`
	ExitCode    *int32 `json:"exit_code,omitempty"`
	Reason      string `json:"reason"`
	Message     string `json:"message"`
}

// VolumeBackupFailed is the payload of a volume.backup.failed event.
type VolumeBackupFailed struct {
	Application string    `json:"application"`
	Volume      string    `json:"volume"`
	NodeID      string    `json:"node_id"`
	FailedAt    time.Time `json:"failed_at"`
	Message     string    `json:"message"`
}

// ApplyApplicationResult is persisted as the exact idempotency response.
// Digest hashes the authored apply request, or the resolved source for rollback.
type ApplyApplicationResult struct {
	Name            string    `json:"name"`
	Generation      uint64    `json:"generation"`
	RevisionID      string    `json:"revision_id"`
	Digest          string    `json:"digest"`
	Changed         bool      `json:"changed"`
	EventSequence   uint64    `json:"event_sequence"`
	ExecutionStatus string    `json:"execution_status"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// RemoveApplicationResult is persisted as an exact idempotency response.
type RemoveApplicationResult struct {
	Name            string    `json:"name"`
	Generation      uint64    `json:"generation"`
	Changed         bool      `json:"changed"`
	EventSequence   uint64    `json:"event_sequence"`
	ExecutionStatus string    `json:"execution_status"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// GitHubTrust lets GitHub Actions runs from one repository, branch and
// optional environment deploy one application with short-lived credentials.
// AccessToken lets a holder of its secret deploy, or only inspect, one
// application until it expires or is deleted. A token without an
// application is an administrator token for the whole cluster.
type AccessToken struct {
	ID          string
	Name        string
	Application string
	ReadOnly    bool
	SecretHash  string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	LastUsedAt  time.Time
}

// Administrator reports whether the token grants administrator access
// instead of access to one application.
func (token AccessToken) Administrator() bool { return token.Application == "" }

// ClusterNotification is a webhook for cluster events, set with orch notify
// add. Its URL is kept encrypted in the store.
type ClusterNotification struct {
	Name      string
	Kind      string
	Events    []string
	CreatedAt time.Time
}

// ClusterAlert is the payload of a cluster event the controller raises
// itself, such as a lost node.
type ClusterAlert struct {
	Node     string    `json:"node,omitempty"`
	Subject  string    `json:"subject,omitempty"`
	Message  string    `json:"message"`
	NotAfter time.Time `json:"not_after,omitzero"`
}

type GitHubTrust struct {
	Repository  string
	Application string
	Branch      string
	Environment string
	CreatedAt   time.Time
	// RepositoryID and RepositoryOwnerID are GitHub's IDs for the repository
	// and its owner, bound on the first deploy. Empty until then.
	RepositoryID      string
	RepositoryOwnerID string
}
