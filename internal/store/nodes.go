package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
)

const enrollAgentOperation = "agent.enroll"

var (
	ErrNodeNotFound        = errors.New("node was not found")
	ErrNodeAlreadyEnrolled = errors.New("a node is already enrolled")
	ErrStaleAgentReport    = errors.New("agent report is stale")
)

// EnrollAgent atomically records the single node, its event, and the exact
// public enrollment response used for idempotent replay.
func (s *Store) EnrollAgent(ctx context.Context, input model.EnrollAgent) (model.EnrollAgentResult, error) {
	if err := validateEnrollAgent(input); err != nil {
		return model.EnrollAgentResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.EnrollAgentResult{}, fmt.Errorf("begin agent enrollment: %w", err)
	}
	defer tx.Rollback()

	replayed, found, err := replayEnrollment(ctx, tx, input.IdempotencyKey, input.RequestHash)
	if err != nil || found {
		return replayed, err
	}

	existing, err := nodeInTransaction(ctx, tx)
	changed := false
	if err == nil {
		if existing.ID != input.NodeID || existing.ClusterID != input.ClusterID ||
			existing.Name != input.Name || existing.PublicKeyHash != input.PublicKeyHash {
			return model.EnrollAgentResult{}, ErrNodeAlreadyEnrolled
		}
	} else if !errors.Is(err, ErrNodeNotFound) {
		return model.EnrollAgentResult{}, err
	} else {
		changed = true
		stamp := input.CreatedAt.UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `INSERT INTO nodes
			(singleton, id, cluster_id, name, public_key_hash, certificate_serial,
			 certificate_pem, certificate_not_after, created_at)
			VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)`, input.NodeID, input.ClusterID,
			input.Name, input.PublicKeyHash, input.CertificateSerial,
			input.CertificatePEM, input.CertificateNotAfter.UTC().Format(time.RFC3339Nano), stamp); err != nil {
			return model.EnrollAgentResult{}, fmt.Errorf("insert enrolled node: %w", err)
		}
		existing = model.Node{
			ID: input.NodeID, ClusterID: input.ClusterID, Name: input.Name, PublicKeyHash: input.PublicKeyHash,
			CertificateSerial: input.CertificateSerial, CertificatePEM: input.CertificatePEM,
			CertificateNotAfter: input.CertificateNotAfter.UTC(), CreatedAt: input.CreatedAt.UTC(),
		}
	}

	result := model.EnrollAgentResult{
		NodeID: existing.ID, NodeName: existing.Name, ClusterID: input.ClusterID,
		ControllerEndpoint: input.ControllerEndpoint, CACertificatePEM: input.CACertificatePEM,
		AgentCertificatePEM: existing.CertificatePEM, ExpiresAt: existing.CertificateNotAfter,
		Changed: changed,
	}
	if changed {
		payload, _ := json.Marshal(struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}{existing.ID, existing.Name})
		inserted, err := tx.ExecContext(ctx, `INSERT INTO events
			(cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
			input.ClusterID, model.EventNodeEnrolled, string(payload), input.CreatedAt.UTC().Format(time.RFC3339Nano))
		if err != nil {
			return model.EnrollAgentResult{}, fmt.Errorf("record node enrollment event: %w", err)
		}
		sequence, err := inserted.LastInsertId()
		if err != nil || sequence < 1 {
			return model.EnrollAgentResult{}, fmt.Errorf("read node enrollment event sequence")
		}
		result.EventSequence = uint64(sequence)
	}
	response, err := json.Marshal(result)
	if err != nil {
		return model.EnrollAgentResult{}, fmt.Errorf("encode enrollment response: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_keys
		(key, operation, request_hash, response, created_at) VALUES (?, ?, ?, ?, ?)`,
		input.IdempotencyKey, enrollAgentOperation, input.RequestHash, string(response),
		input.CreatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return model.EnrollAgentResult{}, fmt.Errorf("record enrollment response: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.EnrollAgentResult{}, fmt.Errorf("commit agent enrollment: %w", err)
	}
	return result, nil
}

func validateEnrollAgent(input model.EnrollAgent) error {
	for label, value := range map[string]string{
		"node id": input.NodeID, "node name": input.Name, "cluster id": input.ClusterID,
		"controller endpoint": input.ControllerEndpoint, "public key hash": input.PublicKeyHash,
		"certificate serial": input.CertificateSerial, "request hash": input.RequestHash,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", label)
		}
	}
	if len(input.PublicKeyHash) != 64 || len(input.RequestHash) != 64 {
		return fmt.Errorf("public key and request hashes must be SHA-256 hex values")
	}
	if len(input.IdempotencyKey) < 16 || len(input.IdempotencyKey) > 128 {
		return fmt.Errorf("idempotency key must contain between 16 and 128 bytes")
	}
	if len(input.CertificatePEM) == 0 || len(input.CACertificatePEM) == 0 {
		return fmt.Errorf("agent and CA certificates are required")
	}
	if input.CreatedAt.IsZero() || input.CertificateNotAfter.IsZero() {
		return fmt.Errorf("enrollment timestamps are required")
	}
	return nil
}

func replayEnrollment(ctx context.Context, tx *sql.Tx, key, requestHash string) (model.EnrollAgentResult, bool, error) {
	var operation, storedHash, response string
	err := tx.QueryRowContext(ctx, `SELECT operation, request_hash, response
		FROM idempotency_keys WHERE key = ?`, key).Scan(&operation, &storedHash, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return model.EnrollAgentResult{}, false, nil
	}
	if err != nil {
		return model.EnrollAgentResult{}, false, fmt.Errorf("read enrollment replay: %w", err)
	}
	if operation != enrollAgentOperation || storedHash != requestHash {
		return model.EnrollAgentResult{}, false, ErrIdempotencyConflict
	}
	var result model.EnrollAgentResult
	if err := json.Unmarshal([]byte(response), &result); err != nil {
		return model.EnrollAgentResult{}, false, fmt.Errorf("decode enrollment replay: %w", err)
	}
	return result, true, nil
}

func (s *Store) Node(ctx context.Context) (model.Node, error) {
	return scanNode(s.db.QueryRowContext(ctx, nodeSelect))
}

func (s *Store) Nodes(ctx context.Context) ([]model.Node, error) {
	node, err := s.Node(ctx)
	if errors.Is(err, ErrNodeNotFound) {
		return []model.Node{}, nil
	}
	if err != nil {
		return nil, err
	}
	return []model.Node{node}, nil
}

const nodeSelect = `SELECT id, cluster_id, name, public_key_hash, certificate_serial,
	certificate_pem, certificate_not_after, created_at, last_seen_at,
	operating_system, architecture, runtime_version, snapshotter, cgroups_v2,
		cni_version, cni_plugins_version, nftables, dns_available, caddy_version, node_subnet, network_generation,
	network_state, network_reason, network_message, dns_state, ingress_state,
	draining_objects,
	assignment_generation, acknowledged_generation, acknowledged_digest,
	last_boot_id, last_report_sequence FROM nodes WHERE singleton = 1`

func nodeInTransaction(ctx context.Context, tx *sql.Tx) (model.Node, error) {
	return scanNode(tx.QueryRowContext(ctx, nodeSelect))
}

func scanNode(row rowScanner) (model.Node, error) {
	var node model.Node
	var notAfter, createdAt string
	var lastSeen sql.NullString
	var cgroups, nftables, dnsAvailable int
	var assignmentGeneration, acknowledgedGeneration, networkGeneration, lastReport, drainingObjects int64
	err := row.Scan(&node.ID, &node.ClusterID, &node.Name, &node.PublicKeyHash, &node.CertificateSerial,
		&node.CertificatePEM, &notAfter, &createdAt, &lastSeen,
		&node.OperatingSystem, &node.Architecture, &node.RuntimeVersion,
		&node.Snapshotter, &cgroups, &node.CNIVersion, &node.CNIPluginsVersion, &nftables, &dnsAvailable, &node.CaddyVersion,
		&node.NodeSubnet, &networkGeneration, &node.NetworkState, &node.NetworkReason,
		&node.NetworkMessage, &node.DNSState, &node.IngressState, &drainingObjects, &assignmentGeneration,
		&acknowledgedGeneration, &node.AcknowledgedDigest, &node.LastBootID, &lastReport)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Node{}, ErrNodeNotFound
	}
	if err != nil {
		return model.Node{}, fmt.Errorf("read node: %w", err)
	}
	if assignmentGeneration < 0 || acknowledgedGeneration < 0 || networkGeneration < 0 || lastReport < 0 || drainingObjects < 0 {
		return model.Node{}, fmt.Errorf("read node: invalid generation or sequence")
	}
	node.AssignmentGeneration = uint64(assignmentGeneration)
	node.AcknowledgedGeneration = uint64(acknowledgedGeneration)
	node.LastReportSequence = uint64(lastReport)
	node.CgroupsV2 = cgroups == 1
	node.Nftables = nftables == 1
	node.DNSAvailable = dnsAvailable == 1
	node.NetworkGeneration = uint64(networkGeneration)
	node.DrainingObjects = uint32(drainingObjects)
	node.CertificateNotAfter, err = time.Parse(time.RFC3339Nano, notAfter)
	if err != nil {
		return model.Node{}, fmt.Errorf("read node certificate expiry: %w", err)
	}
	node.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return model.Node{}, fmt.Errorf("read node creation time: %w", err)
	}
	if lastSeen.Valid {
		stamp, err := time.Parse(time.RFC3339Nano, lastSeen.String)
		if err != nil {
			return model.Node{}, fmt.Errorf("read node last-seen time: %w", err)
		}
		node.LastSeenAt = &stamp
	}
	node.ConnectionStatus = model.NodeEnrolled
	return node, nil
}

func (s *Store) UpdateAgentCapabilities(ctx context.Context, capabilities model.AgentCapabilities) error {
	if capabilities.NodeID == "" || capabilities.BootID == "" || capabilities.SeenAt.IsZero() {
		return fmt.Errorf("node, boot id, and seen time are required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE nodes SET
		last_seen_at = ?, operating_system = ?, architecture = ?, runtime_version = ?,
		snapshotter = ?, cgroups_v2 = ?, cni_version = ?, cni_plugins_version = ?, nftables = ?, dns_available = ?, caddy_version = ?,
		last_report_sequence = CASE
			WHEN last_boot_id = ? THEN last_report_sequence ELSE 0 END,
		last_boot_id = ? WHERE id = ?`, capabilities.SeenAt.UTC().Format(time.RFC3339Nano),
		capabilities.OperatingSystem, capabilities.Architecture, capabilities.RuntimeVersion,
		capabilities.Snapshotter, boolInt(capabilities.CgroupsV2), capabilities.CNIVersion,
		capabilities.CNIPluginsVersion, boolInt(capabilities.Nftables), boolInt(capabilities.DNSAvailable),
		capabilities.CaddyVersion, capabilities.BootID,
		capabilities.BootID, capabilities.NodeID)
	if err != nil {
		return fmt.Errorf("update agent capabilities: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return ErrNodeNotFound
	}
	return nil
}

func (s *Store) TouchAgent(ctx context.Context, nodeID, bootID string, seenAt time.Time) error {
	if nodeID == "" || bootID == "" || seenAt.IsZero() {
		return fmt.Errorf("node, boot id, and seen time are required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE nodes SET last_seen_at = ?
		WHERE id = ? AND last_boot_id = ?`, seenAt.UTC().Format(time.RFC3339Nano), nodeID, bootID)
	if err != nil {
		return fmt.Errorf("record agent heartbeat: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return ErrStaleAgentReport
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
