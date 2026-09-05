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
	CronJobs           []CronJobStatus    `json:"cron_jobs,omitempty"`
	JobRuns            []JobRunStatus     `json:"job_runs,omitempty"`
}

// ApplyApplication contains validated, canonical application intent.
type ApplyApplication struct {
	Name              string
	RevisionID        string
	Digest            string
	CanonicalManifest []byte
	IdempotencyKey    string
	CreatedAt         time.Time
	// Rollback requests are guarded against concurrent desired-state changes.
	ExpectedGeneration     uint64
	RollbackFromRevisionID string
}

// ApplyApplicationResult is persisted as the exact idempotency response.
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
