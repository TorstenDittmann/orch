package model

import (
	"encoding/json"
	"time"
)

const EventClusterInitialized = "cluster.initialized"

const (
	EventNetworkConfigured = "network.configured"
	EventNetworkReady      = "network.ready"
)

// Cluster is the durable identity and control endpoint of one orch cluster.
type Cluster struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Endpoint          string     `json:"endpoint"`
	Generation        uint64     `json:"generation"`
	CreatedAt         time.Time  `json:"created_at"`
	WorkloadCIDR      string     `json:"workload_cidr"`
	NetworkGeneration uint64     `json:"network_generation"`
	NetworkFrozenAt   *time.Time `json:"network_frozen_at,omitempty"`
}

type ConfigureNetwork struct {
	CIDR           string
	IdempotencyKey string
	RequestHash    string
	UpdatedAt      time.Time
}

type ConfigureNetworkResult struct {
	CIDR          string    `json:"cidr"`
	Generation    uint64    `json:"generation"`
	Frozen        bool      `json:"frozen"`
	Changed       bool      `json:"changed"`
	EventSequence uint64    `json:"event_sequence"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Event is an ordered, append-only record of a durable state transition.
type Event struct {
	Sequence  int64           `json:"sequence"`
	ClusterID string          `json:"cluster_id"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}
