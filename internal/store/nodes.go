package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
)

const enrollAgentOperation = "agent.enroll"

var (
	ErrNodeNotFound         = errors.New("node was not found")
	ErrNodeAlreadyEnrolled  = errors.New("a node is already enrolled")
	ErrStaleAgentReport     = errors.New("agent report is stale")
	ErrNodeLimitReached     = errors.New("the cluster already has 20 active nodes")
	ErrNodeRemoved          = errors.New("node was removed")
	ErrMeshIdentityConflict = errors.New("mesh advertise address or WireGuard public key is already used by another node")
)

const maxActiveNodes = 20

const (
	updateNodeOperation = "node.update"
	removeNodeOperation = "node.remove"
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

	existing, err := nodeByIDInTransaction(ctx, tx, input.NodeID)
	changed := false
	if err == nil {
		if existing.Lifecycle == model.NodeLifecycleRemoved {
			return model.EnrollAgentResult{}, ErrNodeRemoved
		}
		if existing.ID != input.NodeID || existing.ClusterID != input.ClusterID ||
			existing.Name != input.Name || existing.PublicKeyHash != input.PublicKeyHash {
			return model.EnrollAgentResult{}, ErrNodeAlreadyEnrolled
		}
	} else if !errors.Is(err, ErrNodeNotFound) {
		return model.EnrollAgentResult{}, err
	} else {
		var activeCount int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM nodes WHERE lifecycle != 'REMOVED'`).Scan(&activeCount); err != nil {
			return model.EnrollAgentResult{}, fmt.Errorf("read node inventory: %w", err)
		}
		if activeCount >= maxActiveNodes {
			return model.EnrollAgentResult{}, ErrNodeLimitReached
		}
		var nextOrdinal int64
		for ; nextOrdinal <= maxActiveNodes; nextOrdinal++ {
			var used int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM nodes WHERE ordinal = ? AND lifecycle != 'REMOVED'`, nextOrdinal).Scan(&used); err != nil {
				return model.EnrollAgentResult{}, fmt.Errorf("allocate node subnet slot: %w", err)
			}
			if used == 0 {
				break
			}
		}
		changed = true
		stamp := input.CreatedAt.UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `INSERT INTO nodes
			(id, cluster_id, name, public_key_hash, certificate_serial,
			 certificate_pem, certificate_not_after, created_at, ordinal, lifecycle, ingress)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'ACTIVE', ?)`, input.NodeID, input.ClusterID,
			input.Name, input.PublicKeyHash, input.CertificateSerial,
			input.CertificatePEM, input.CertificateNotAfter.UTC().Format(time.RFC3339Nano), stamp, nextOrdinal, boolInt(activeCount == 0)); err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				return model.EnrollAgentResult{}, ErrNodeAlreadyEnrolled
			}
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
	return scanNode(s.db.QueryRowContext(ctx, nodeSelect+` WHERE lifecycle != 'REMOVED' ORDER BY ordinal LIMIT 1`))
}

func (s *Store) NodeByID(ctx context.Context, nodeID string) (model.Node, error) {
	if strings.TrimSpace(nodeID) == "" {
		return model.Node{}, ErrNodeNotFound
	}
	return scanNode(s.db.QueryRowContext(ctx, nodeSelect+` WHERE id = ?`, nodeID))
}

func (s *Store) NodeByName(ctx context.Context, name string) (model.Node, error) {
	if strings.TrimSpace(name) == "" {
		return model.Node{}, ErrNodeNotFound
	}
	return scanNode(s.db.QueryRowContext(ctx, nodeSelect+` WHERE name = ? AND lifecycle != 'REMOVED'`, name))
}

func (s *Store) Nodes(ctx context.Context) ([]model.Node, error) {
	rows, err := s.db.QueryContext(ctx, nodeSelect+` WHERE lifecycle != 'REMOVED' ORDER BY ordinal, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nodes := make([]model.Node, 0)
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate nodes: %w", err)
	}
	return nodes, nil
}

// NodeAssignedResources sums the exact desired reservations on a node. During
// an admitted start-before-stop transition, the authorized rollback process
// is included because both processes may coexist.
func (s *Store) NodeAssignedResources(ctx context.Context, nodeID string) (uint64, uint64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT spec FROM desired_allocations WHERE node_id = ?
		UNION ALL SELECT spec FROM desired_cron_jobs WHERE node_id = ?`, nodeID, nodeID)
	if err != nil {
		return 0, 0, fmt.Errorf("read node resource reservations: %w", err)
	}
	defer rows.Close()
	var cpu, memory uint64
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return 0, 0, fmt.Errorf("scan node resource reservation: %w", err)
		}
		var allocation model.AllocationDesired
		if err := json.Unmarshal([]byte(encoded), &allocation); err == nil && allocation.ID != "" {
			cpu += allocation.CPUMillis
			memory += allocation.MemoryBytes
			if allocation.StartBeforeStop && allocation.Rollback != nil {
				cpu += allocation.Rollback.CPUMillis
				memory += allocation.Rollback.MemoryBytes
			}
			continue
		}
		var job model.CronJobDesired
		if err := json.Unmarshal([]byte(encoded), &job); err != nil || job.Job == "" {
			return 0, 0, fmt.Errorf("decode node resource reservation")
		}
		cpu += job.CPUMillis
		memory += job.MemoryBytes
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate node resource reservations: %w", err)
	}
	return cpu, memory, nil
}

func (s *Store) NodeDrainBlockers(ctx context.Context, node model.Node) ([]string, error) {
	var localVolumes, cronJobs int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM volume_claims v
		JOIN applications a ON a.name = v.application_name
		WHERE v.node_id = ? AND a.deleted_at IS NULL`, node.ID).Scan(&localVolumes); err != nil {
		return nil, fmt.Errorf("read node local-volume blockers: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM desired_cron_jobs WHERE node_id = ?`, node.ID).Scan(&cronJobs); err != nil {
		return nil, fmt.Errorf("read node cron blockers: %w", err)
	}
	var blockers []string
	if localVolumes != 0 {
		blockers = append(blockers, "local_volume_pinned")
	}
	if cronJobs != 0 {
		blockers = append(blockers, "cron_handoff_pending")
	}
	if node.AssignmentGeneration != node.AcknowledgedGeneration {
		blockers = append(blockers, "assignment_ack_pending")
	}
	if node.DrainingObjects != 0 {
		blockers = append(blockers, "runtime_objects_draining")
	}
	return blockers, nil
}

const nodeSelect = `SELECT id, cluster_id, name, public_key_hash, certificate_serial,
	certificate_pem, certificate_not_after, created_at, last_seen_at,
	operating_system, architecture, runtime_version, snapshotter, cgroups_v2,
		cni_version, cni_plugins_version, nftables, dns_available, caddy_version, node_subnet, network_generation,
	network_state, network_reason, network_message, dns_state, ingress_state,
	draining_objects,
	assignment_generation, acknowledged_generation, acknowledged_digest,
	last_boot_id, last_report_sequence, ordinal, lifecycle, labels_json, ingress,
	total_cpu_millis, total_memory_bytes, allocatable_cpu_millis, allocatable_memory_bytes,
	advertise_address, wireguard_public_key, wireguard_listen_port, removed_at FROM nodes`

func nodeByIDInTransaction(ctx context.Context, tx *sql.Tx, nodeID string) (model.Node, error) {
	return scanNode(tx.QueryRowContext(ctx, nodeSelect+` WHERE id = ?`, nodeID))
}

func scanNode(row rowScanner) (model.Node, error) {
	var node model.Node
	var notAfter, createdAt, labelsJSON string
	var lastSeen, removedAt sql.NullString
	var cgroups, nftables, dnsAvailable, ingress int
	var assignmentGeneration, acknowledgedGeneration, networkGeneration, lastReport, drainingObjects, ordinal int64
	var totalCPU, totalMemory, allocatableCPU, allocatableMemory, wireGuardPort int64
	err := row.Scan(&node.ID, &node.ClusterID, &node.Name, &node.PublicKeyHash, &node.CertificateSerial,
		&node.CertificatePEM, &notAfter, &createdAt, &lastSeen,
		&node.OperatingSystem, &node.Architecture, &node.RuntimeVersion,
		&node.Snapshotter, &cgroups, &node.CNIVersion, &node.CNIPluginsVersion, &nftables, &dnsAvailable, &node.CaddyVersion,
		&node.NodeSubnet, &networkGeneration, &node.NetworkState, &node.NetworkReason,
		&node.NetworkMessage, &node.DNSState, &node.IngressState, &drainingObjects, &assignmentGeneration,
		&acknowledgedGeneration, &node.AcknowledgedDigest, &node.LastBootID, &lastReport,
		&ordinal, &node.Lifecycle, &labelsJSON, &ingress, &totalCPU, &totalMemory,
		&allocatableCPU, &allocatableMemory, &node.AdvertiseAddress,
		&node.WireGuardPublicKey, &wireGuardPort, &removedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Node{}, ErrNodeNotFound
	}
	if err != nil {
		return model.Node{}, fmt.Errorf("read node: %w", err)
	}
	if assignmentGeneration < 0 || acknowledgedGeneration < 0 || networkGeneration < 0 || lastReport < 0 || drainingObjects < 0 ||
		ordinal < 0 || totalCPU < 0 || totalMemory < 0 || allocatableCPU < 0 || allocatableMemory < 0 || wireGuardPort < 0 || wireGuardPort > 65535 {
		return model.Node{}, fmt.Errorf("read node: invalid generation or sequence")
	}
	if err := json.Unmarshal([]byte(labelsJSON), &node.Labels); err != nil {
		return model.Node{}, fmt.Errorf("read node labels: %w", err)
	}
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	node.Ordinal = uint32(ordinal)
	node.Ingress = ingress == 1
	node.TotalCPUMillis = uint64(totalCPU)
	node.TotalMemoryBytes = uint64(totalMemory)
	node.AllocatableCPUMillis = uint64(allocatableCPU)
	node.AllocatableMemoryBytes = uint64(allocatableMemory)
	node.WireGuardListenPort = uint32(wireGuardPort)
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
	if removedAt.Valid {
		stamp, err := time.Parse(time.RFC3339Nano, removedAt.String)
		if err != nil {
			return model.Node{}, fmt.Errorf("read node removal time: %w", err)
		}
		node.RemovedAt = &stamp
	}
	node.ConnectionStatus = model.NodeEnrolled
	return node, nil
}

func (s *Store) UpdateAgentCapabilities(ctx context.Context, capabilities model.AgentCapabilities) error {
	if capabilities.NodeID == "" || capabilities.BootID == "" || capabilities.SeenAt.IsZero() {
		return fmt.Errorf("node, boot id, and seen time are required")
	}
	if err := model.ValidateMeshIdentity(capabilities.AdvertiseAddress, capabilities.WireGuardPublicKey, capabilities.WireGuardListenPort); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE nodes SET
		last_seen_at = ?, operating_system = ?, architecture = ?, runtime_version = ?,
		snapshotter = ?, cgroups_v2 = ?, cni_version = ?, cni_plugins_version = ?, nftables = ?, dns_available = ?, caddy_version = ?,
		total_cpu_millis = ?, total_memory_bytes = ?, allocatable_cpu_millis = ?, allocatable_memory_bytes = ?,
		advertise_address = ?, wireguard_public_key = ?, wireguard_listen_port = ?,
		last_report_sequence = CASE
			WHEN last_boot_id = ? THEN last_report_sequence ELSE 0 END,
		last_boot_id = ? WHERE id = ? AND lifecycle != 'REMOVED'`, capabilities.SeenAt.UTC().Format(time.RFC3339Nano),
		capabilities.OperatingSystem, capabilities.Architecture, capabilities.RuntimeVersion,
		capabilities.Snapshotter, boolInt(capabilities.CgroupsV2), capabilities.CNIVersion,
		capabilities.CNIPluginsVersion, boolInt(capabilities.Nftables), boolInt(capabilities.DNSAvailable),
		capabilities.CaddyVersion, capabilities.TotalCPUMillis, capabilities.TotalMemoryBytes,
		capabilities.AllocatableCPUMillis, capabilities.AllocatableMemoryBytes,
		capabilities.AdvertiseAddress, capabilities.WireGuardPublicKey, capabilities.WireGuardListenPort,
		capabilities.BootID,
		capabilities.BootID, capabilities.NodeID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: nodes.advertise_address") ||
			strings.Contains(err.Error(), "UNIQUE constraint failed: nodes.wireguard_public_key") {
			return ErrMeshIdentityConflict
		}
		return fmt.Errorf("update agent capabilities: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return ErrNodeNotFound
	}
	return nil
}

// UpdateNodeLabels replaces operator labels atomically. The caller validates
// the public label syntax so this store boundary only persists canonical JSON.
func (s *Store) UpdateNodeLabels(ctx context.Context, nodeID string, labels map[string]string) error {
	encoded, err := json.Marshal(labels)
	if err != nil {
		return fmt.Errorf("encode node labels: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE nodes SET labels_json = ? WHERE id = ? AND lifecycle != 'REMOVED'`, string(encoded), nodeID)
	if err != nil {
		return fmt.Errorf("update node labels: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return ErrNodeNotFound
	}
	return nil
}

func (s *Store) SetNodeLifecycle(ctx context.Context, nodeID, lifecycle string, now time.Time) error {
	if now.IsZero() {
		return fmt.Errorf("node lifecycle time is required")
	}
	switch lifecycle {
	case model.NodeLifecycleActive, model.NodeLifecycleCordoned, model.NodeLifecycleDraining:
	default:
		return fmt.Errorf("invalid node lifecycle %q", lifecycle)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE nodes SET lifecycle = ? WHERE id = ? AND lifecycle != 'REMOVED'`, lifecycle, nodeID)
	if err != nil {
		return fmt.Errorf("update node lifecycle: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return ErrNodeNotFound
	}
	return nil
}

func (s *Store) SetNodeIngress(ctx context.Context, nodeID string, enabled bool) error {
	result, err := s.db.ExecContext(ctx, `UPDATE nodes SET ingress = ? WHERE id = ? AND lifecycle != 'REMOVED'`, boolInt(enabled), nodeID)
	if err != nil {
		return fmt.Errorf("update node ingress role: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return ErrNodeNotFound
	}
	return nil
}

func (s *Store) UpdateNode(ctx context.Context, input model.UpdateNode) (model.UpdateNodeResult, error) {
	if input.NodeName == "" || len(input.IdempotencyKey) < 16 || len(input.IdempotencyKey) > 128 || len(input.RequestHash) != 64 || input.UpdatedAt.IsZero() {
		return model.UpdateNodeResult{}, fmt.Errorf("node update idempotency data is invalid")
	}
	if input.SetLifecycle {
		switch input.Lifecycle {
		case model.NodeLifecycleActive, model.NodeLifecycleCordoned, model.NodeLifecycleDraining:
		default:
			return model.UpdateNodeResult{}, fmt.Errorf("invalid node lifecycle %q", input.Lifecycle)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.UpdateNodeResult{}, fmt.Errorf("begin node update: %w", err)
	}
	defer tx.Rollback()
	var replay model.UpdateNodeResult
	if found, err := replayNodeMutation(ctx, tx, input.IdempotencyKey, updateNodeOperation, input.RequestHash, &replay); err != nil || found {
		return replay, err
	}
	node, err := scanNode(tx.QueryRowContext(ctx, nodeSelect+` WHERE name = ? AND lifecycle != 'REMOVED'`, input.NodeName))
	if err != nil {
		return model.UpdateNodeResult{}, err
	}
	labels := make(map[string]string, len(node.Labels)+len(input.Labels))
	for key, value := range node.Labels {
		labels[key] = value
	}
	for key, value := range input.Labels {
		labels[key] = value
	}
	for _, key := range input.RemoveLabels {
		delete(labels, key)
	}
	lifecycle := node.Lifecycle
	if input.SetLifecycle {
		lifecycle = input.Lifecycle
	}
	ingress := node.Ingress
	if input.SetIngress {
		ingress = input.Ingress
	}
	result := model.UpdateNodeResult{Node: node}
	result.Changed = !reflect.DeepEqual(node.Labels, labels) || lifecycle != node.Lifecycle || ingress != node.Ingress
	stamp := input.UpdatedAt.UTC().Format(time.RFC3339Nano)
	if result.Changed {
		encodedLabels, err := json.Marshal(labels)
		if err != nil {
			return model.UpdateNodeResult{}, fmt.Errorf("encode node labels: %w", err)
		}
		changed, err := tx.ExecContext(ctx, `UPDATE nodes SET labels_json = ?, lifecycle = ?, ingress = ? WHERE id = ? AND lifecycle != 'REMOVED'`,
			string(encodedLabels), lifecycle, boolInt(ingress), node.ID)
		if err != nil {
			return model.UpdateNodeResult{}, fmt.Errorf("update node: %w", err)
		}
		if rows, _ := changed.RowsAffected(); rows != 1 {
			return model.UpdateNodeResult{}, ErrNodeNotFound
		}
		if lifecycle == model.NodeLifecycleDraining {
			if _, err := tx.ExecContext(ctx, `INSERT INTO node_operations(node_id, kind, lost, started_at, updated_at)
				VALUES (?, 'DRAIN', 0, ?, ?) ON CONFLICT(node_id) DO UPDATE SET kind = 'DRAIN', lost = 0, updated_at = excluded.updated_at`,
				node.ID, stamp, stamp); err != nil {
				return model.UpdateNodeResult{}, fmt.Errorf("record drain operation: %w", err)
			}
		} else if _, err := tx.ExecContext(ctx, `DELETE FROM node_operations WHERE node_id = ? AND kind = 'DRAIN'`, node.ID); err != nil {
			return model.UpdateNodeResult{}, fmt.Errorf("clear drain operation: %w", err)
		}
		payload, _ := json.Marshal(struct {
			Name      string            `json:"name"`
			Lifecycle string            `json:"lifecycle"`
			Labels    map[string]string `json:"labels"`
			Ingress   bool              `json:"ingress"`
		}{node.Name, lifecycle, labels, ingress})
		inserted, err := tx.ExecContext(ctx, `INSERT INTO events(cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
			node.ClusterID, model.EventNodeUpdated, string(payload), stamp)
		if err != nil {
			return model.UpdateNodeResult{}, fmt.Errorf("record node update event: %w", err)
		}
		sequence, err := inserted.LastInsertId()
		if err != nil || sequence < 1 {
			return model.UpdateNodeResult{}, fmt.Errorf("read node update event sequence")
		}
		result.EventSequence = uint64(sequence)
		result.Node, err = nodeByIDInTransaction(ctx, tx, node.ID)
		if err != nil {
			return model.UpdateNodeResult{}, err
		}
	}
	// Administrator responses never need the stored certificate body. Keeping
	// it out makes the persisted replay equivalent and avoids copying
	// credential material through the public result.
	result.Node.CertificatePEM = nil
	if err := recordNodeMutation(ctx, tx, input.IdempotencyKey, updateNodeOperation, input.RequestHash, result, stamp); err != nil {
		return model.UpdateNodeResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.UpdateNodeResult{}, fmt.Errorf("commit node update: %w", err)
	}
	return result, nil
}

func (s *Store) RemoveNode(ctx context.Context, input model.RemoveNode) (model.RemoveNodeResult, error) {
	if input.NodeName == "" || len(input.IdempotencyKey) < 16 || len(input.IdempotencyKey) > 128 || len(input.RequestHash) != 64 || input.RemovedAt.IsZero() {
		return model.RemoveNodeResult{}, fmt.Errorf("node removal idempotency data is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.RemoveNodeResult{}, fmt.Errorf("begin node removal: %w", err)
	}
	defer tx.Rollback()
	var replay model.RemoveNodeResult
	if found, err := replayNodeMutation(ctx, tx, input.IdempotencyKey, removeNodeOperation, input.RequestHash, &replay); err != nil || found {
		return replay, err
	}
	node, err := scanNode(tx.QueryRowContext(ctx, nodeSelect+` WHERE name = ? AND lifecycle != 'REMOVED'`, input.NodeName))
	if err != nil {
		return model.RemoveNodeResult{}, err
	}
	if !input.Lost {
		if !input.Connected {
			return model.RemoveNodeResult{}, fmt.Errorf("normal node removal requires a connected drained node")
		}
		if node.Lifecycle != model.NodeLifecycleDraining {
			return model.RemoveNodeResult{}, fmt.Errorf("normal node removal requires lifecycle DRAINING")
		}
		if node.AcknowledgedGeneration != node.AssignmentGeneration {
			return model.RemoveNodeResult{}, fmt.Errorf("node has not acknowledged its final assignment")
		}
		if node.DrainingObjects != 0 {
			return model.RemoveNodeResult{}, fmt.Errorf("node still reports %d draining runtime objects", node.DrainingObjects)
		}
		var desired int
		if err := tx.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM desired_allocations WHERE node_id = ?) +
			(SELECT count(*) FROM desired_cron_jobs WHERE node_id = ?)`, node.ID, node.ID).Scan(&desired); err != nil {
			return model.RemoveNodeResult{}, fmt.Errorf("read node workloads: %w", err)
		}
		if desired != 0 {
			return model.RemoveNodeResult{}, fmt.Errorf("node still has desired workloads")
		}
	}
	for _, table := range []string{"allocation_observations", "route_observations", "tcp_route_observations", "cron_job_observations", "cron_run_observations", "desired_allocations", "desired_cron_jobs"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE node_id = ?`, node.ID); err != nil {
			return model.RemoveNodeResult{}, fmt.Errorf("clear removed node %s: %w", table, err)
		}
	}
	stamp := input.RemovedAt.UTC().Format(time.RFC3339Nano)
	changed, err := tx.ExecContext(ctx, `UPDATE nodes SET lifecycle = 'REMOVED', removed_at = ? WHERE id = ? AND lifecycle != 'REMOVED'`, stamp, node.ID)
	if err != nil {
		return model.RemoveNodeResult{}, fmt.Errorf("remove node: %w", err)
	}
	if rows, _ := changed.RowsAffected(); rows != 1 {
		return model.RemoveNodeResult{}, ErrNodeNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM node_operations WHERE node_id = ?`, node.ID); err != nil {
		return model.RemoveNodeResult{}, fmt.Errorf("clear node operation: %w", err)
	}
	payload, _ := json.Marshal(struct {
		Name string `json:"name"`
		Lost bool   `json:"lost"`
	}{node.Name, input.Lost})
	inserted, err := tx.ExecContext(ctx, `INSERT INTO events(cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
		node.ClusterID, model.EventNodeRemoved, string(payload), stamp)
	if err != nil {
		return model.RemoveNodeResult{}, fmt.Errorf("record node removal event: %w", err)
	}
	sequence, err := inserted.LastInsertId()
	if err != nil || sequence < 1 {
		return model.RemoveNodeResult{}, fmt.Errorf("read node removal event sequence")
	}
	result := model.RemoveNodeResult{Name: node.Name, Changed: true, EventSequence: uint64(sequence)}
	if err := recordNodeMutation(ctx, tx, input.IdempotencyKey, removeNodeOperation, input.RequestHash, result, stamp); err != nil {
		return model.RemoveNodeResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.RemoveNodeResult{}, fmt.Errorf("commit node removal: %w", err)
	}
	return result, nil
}

func replayNodeMutation(ctx context.Context, tx *sql.Tx, key, operation, requestHash string, result any) (bool, error) {
	var storedOperation, storedHash, response string
	err := tx.QueryRowContext(ctx, `SELECT operation, request_hash, response FROM idempotency_keys WHERE key = ?`, key).
		Scan(&storedOperation, &storedHash, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read node mutation replay: %w", err)
	}
	if storedOperation != operation || storedHash != requestHash {
		return false, ErrIdempotencyConflict
	}
	if err := json.Unmarshal([]byte(response), result); err != nil {
		return false, fmt.Errorf("decode node mutation replay: %w", err)
	}
	return true, nil
}

func recordNodeMutation(ctx context.Context, tx *sql.Tx, key, operation, requestHash string, result any, stamp string) error {
	response, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode node mutation response: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_keys(key, operation, request_hash, response, created_at) VALUES (?, ?, ?, ?, ?)`,
		key, operation, requestHash, string(response), stamp); err != nil {
		return fmt.Errorf("record node mutation response: %w", err)
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
