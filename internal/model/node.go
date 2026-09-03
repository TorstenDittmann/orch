package model

import "time"

const (
	EventNodeEnrolled             = "node.enrolled"
	EventAssignmentUpdated        = "node.assignment.updated"
	EventApplicationStatusChanged = "application.execution.changed"
	EventAllocationReady          = "allocation.ready"
	EventAllocationUnhealthy      = "allocation.unhealthy"
	EventRouteActive              = "route.active"
	EventRouteFailed              = "route.failed"

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
	CNIVersion             string     `json:"cni_version,omitempty"`
	CNIPluginsVersion      string     `json:"cni_plugins_version,omitempty"`
	Nftables               bool       `json:"nftables"`
	DNSAvailable           bool       `json:"dns_available"`
	CaddyVersion           string     `json:"caddy_version,omitempty"`
	NodeSubnet             string     `json:"node_subnet,omitempty"`
	NetworkGeneration      uint64     `json:"network_generation"`
	NetworkState           string     `json:"network_state,omitempty"`
	NetworkReason          string     `json:"network_reason,omitempty"`
	NetworkMessage         string     `json:"network_message,omitempty"`
	DNSState               string     `json:"dns_state,omitempty"`
	IngressState           string     `json:"ingress_state,omitempty"`
	DrainingObjects        uint32     `json:"draining_objects"`
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
