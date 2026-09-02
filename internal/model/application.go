package model

import (
	"encoding/json"
	"time"
)

const (
	EventApplicationDesiredUpdated = "application.desired.updated"
	ExecutionPending               = "PENDING"
)

// Application is the current durable desired state of one application.
type Application struct {
	Name              string          `json:"name"`
	Generation        uint64          `json:"generation"`
	RevisionID        string          `json:"revision_id"`
	Digest            string          `json:"digest"`
	CanonicalManifest json.RawMessage `json:"canonical_manifest"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

// ApplyApplication contains validated, canonical application intent.
type ApplyApplication struct {
	Name              string
	RevisionID        string
	Digest            string
	CanonicalManifest []byte
	IdempotencyKey    string
	CreatedAt         time.Time
}

// ApplyApplicationResult is persisted as the exact idempotency response.
type ApplyApplicationResult struct {
	Name          string    `json:"name"`
	Generation    uint64    `json:"generation"`
	RevisionID    string    `json:"revision_id"`
	Digest        string    `json:"digest"`
	Changed       bool      `json:"changed"`
	EventSequence uint64    `json:"event_sequence"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}
