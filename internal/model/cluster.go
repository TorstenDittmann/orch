package model

import (
	"encoding/json"
	"time"
)

const EventClusterInitialized = "cluster.initialized"

// Cluster is the durable identity and control endpoint of one orch cluster.
type Cluster struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Endpoint   string    `json:"endpoint"`
	Generation uint64    `json:"generation"`
	CreatedAt  time.Time `json:"created_at"`
}

// Event is an ordered, append-only record of a durable state transition.
type Event struct {
	Sequence  int64           `json:"sequence"`
	ClusterID string          `json:"cluster_id"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}
