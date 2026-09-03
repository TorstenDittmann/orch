package model

import "time"

const (
	EventNodeEnrolled             = "node.enrolled"
	EventAssignmentUpdated        = "node.assignment.updated"
	EventApplicationStatusChanged = "application.execution.changed"

	NodeEnrolled     = "ENROLLED"
	NodeConnected    = "CONNECTED"
	NodeDisconnected = "DISCONNECTED"
)

// Node is the one enrolled execution agent for the single-node slice.
type Node struct {
	ID                     string     `json:"id"`
	ClusterID              string     `json:"cluster_id"`
	Name                   string     `json:"name"`
	PublicKeyHash          string     `json:"public_key_hash,omitempty"`
	CertificateSerial      string     `json:"certificate_serial,omitempty"`
	CertificatePEM         []byte     `json:"-"`
	CertificateNotAfter    time.Time  `json:"certificate_not_after"`
	CreatedAt              time.Time  `json:"created_at"`
	LastSeenAt             *time.Time `json:"last_seen_at,omitempty"`
	OperatingSystem        string     `json:"operating_system,omitempty"`
	Architecture           string     `json:"architecture,omitempty"`
	RuntimeVersion         string     `json:"runtime_version,omitempty"`
	Snapshotter            string     `json:"snapshotter,omitempty"`
	CgroupsV2              bool       `json:"cgroups_v2"`
	AssignmentGeneration   uint64     `json:"assignment_generation"`
	AcknowledgedGeneration uint64     `json:"acknowledged_generation"`
	AcknowledgedDigest     string     `json:"acknowledged_digest,omitempty"`
	LastBootID             string     `json:"last_boot_id,omitempty"`
	LastReportSequence     uint64     `json:"last_report_sequence"`
	ConnectionStatus       string     `json:"connection_status"`
}

type EnrollAgent struct {
	NodeID              string
	Name                string
	PublicKeyHash       string
	CertificateSerial   string
	CertificatePEM      []byte
	CertificateNotAfter time.Time
	ClusterID           string
	ControllerEndpoint  string
	CACertificatePEM    []byte
	IdempotencyKey      string
	RequestHash         string
	CreatedAt           time.Time
}

type EnrollAgentResult struct {
	NodeID              string    `json:"node_id"`
	NodeName            string    `json:"node_name"`
	ClusterID           string    `json:"cluster_id"`
	ControllerEndpoint  string    `json:"controller_endpoint"`
	CACertificatePEM    []byte    `json:"ca_certificate_pem"`
	AgentCertificatePEM []byte    `json:"agent_certificate_pem"`
	ExpiresAt           time.Time `json:"expires_at"`
	Changed             bool      `json:"changed"`
	EventSequence       uint64    `json:"event_sequence"`
}
