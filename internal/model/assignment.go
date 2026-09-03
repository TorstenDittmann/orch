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
)

type VolumeMount struct {
	Name     string `json:"name"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
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
}

type Assignment struct {
	NodeID      string              `json:"node_id"`
	Generation  uint64              `json:"generation"`
	Digest      string              `json:"digest"`
	Payload     json.RawMessage     `json:"payload"`
	Allocations []AllocationDesired `json:"allocations"`
	CreatedAt   time.Time           `json:"created_at"`
}

type AllocationStatus struct {
	ID                    string    `json:"id"`
	Application           string    `json:"application"`
	Service               string    `json:"service"`
	Replica               uint32    `json:"replica"`
	ApplicationGeneration uint64    `json:"application_generation"`
	SpecDigest            string    `json:"spec_digest"`
	Phase                 string    `json:"phase"`
	Reason                string    `json:"reason,omitempty"`
	Message               string    `json:"message,omitempty"`
	ExitCode              *int32    `json:"exit_code,omitempty"`
	RestartCount          uint32    `json:"restart_count"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type PublishAssignment struct {
	NodeID            string
	Digest            string
	Payload           []byte
	Allocations       []AllocationDesired
	ApplicationStates map[string]ApplicationExecution
	CreatedAt         time.Time
}

type AgentCapabilities struct {
	NodeID          string
	BootID          string
	OperatingSystem string
	Architecture    string
	RuntimeVersion  string
	Snapshotter     string
	CgroupsV2       bool
	SeenAt          time.Time
}

type AgentReport struct {
	NodeID               string
	BootID               string
	Sequence             uint64
	AssignmentGeneration uint64
	AssignmentDigest     string
	Allocations          []AllocationStatus
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
	}{allocation.Image, allocation.Command, allocation.Args, allocation.Environment,
		allocation.RestartPolicy, allocation.CPUMillis, allocation.MemoryBytes, allocation.Mounts,
		allocation.StartDeadlineSeconds, allocation.AutoRollback, allocation.StartBeforeStop}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("encode allocation runtime spec: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}
