package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
)

var ErrAssignmentNotFound = errors.New("assignment was not found")

// PublishAssignment atomically writes an immutable full assignment, replaces
// the desired-allocation view, and updates application execution decisions.
func (s *Store) PublishAssignment(ctx context.Context, input model.PublishAssignment) (model.Assignment, bool, error) {
	if input.NodeID == "" || input.CreatedAt.IsZero() || !json.Valid(input.Payload) {
		return model.Assignment{}, false, fmt.Errorf("node, creation time, and valid assignment payload are required")
	}
	sum := sha256.Sum256(input.Payload)
	if input.Digest != "sha256:"+hex.EncodeToString(sum[:]) {
		return model.Assignment{}, false, fmt.Errorf("assignment digest does not match payload")
	}
	canonical, err := json.Marshal(input.Allocations)
	if err != nil {
		return model.Assignment{}, false, fmt.Errorf("encode assignment allocations: %w", err)
	}
	if !bytes.Equal(canonical, input.Payload) {
		return model.Assignment{}, false, fmt.Errorf("assignment payload does not match desired allocations")
	}
	previousID := ""
	for _, allocation := range input.Allocations {
		if previousID != "" && allocation.ID <= previousID {
			return model.Assignment{}, false, fmt.Errorf("desired allocations are not in strict canonical order")
		}
		previousID = allocation.ID
		if allocation.ID == "" || allocation.Application == "" || allocation.Service == "" ||
			allocation.Replica == 0 || allocation.ApplicationGeneration == 0 || allocation.RevisionID == "" {
			return model.Assignment{}, false, fmt.Errorf("desired allocation is incomplete")
		}
		specDigest, err := model.AllocationSpecDigest(allocation)
		if err != nil || specDigest != allocation.SpecDigest {
			return model.Assignment{}, false, fmt.Errorf("desired allocation %q spec digest does not match", allocation.ID)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Assignment{}, false, fmt.Errorf("begin assignment publication: %w", err)
	}
	defer tx.Rollback()
	var currentGeneration int64
	if err := tx.QueryRowContext(ctx, `SELECT assignment_generation FROM nodes WHERE id = ?`, input.NodeID).Scan(&currentGeneration); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Assignment{}, false, ErrNodeNotFound
		}
		return model.Assignment{}, false, fmt.Errorf("read assignment generation: %w", err)
	}
	if currentGeneration > 0 {
		var currentDigest string
		if err := tx.QueryRowContext(ctx, `SELECT digest FROM assignment_revisions
			WHERE node_id = ? AND generation = ?`, input.NodeID, currentGeneration).Scan(&currentDigest); err != nil {
			return model.Assignment{}, false, fmt.Errorf("read current assignment digest: %w", err)
		}
		if currentDigest == input.Digest {
			for name, execution := range input.ApplicationStates {
				if execution.UpdatedAt.IsZero() {
					execution.UpdatedAt = input.CreatedAt.UTC()
				}
				if _, err := upsertExecutionTx(ctx, tx, name, execution, true); err != nil {
					return model.Assignment{}, false, err
				}
			}
			assignment, err := assignmentInTransaction(ctx, tx, input.NodeID, uint64(currentGeneration))
			if err != nil {
				return model.Assignment{}, false, err
			}
			if err := tx.Commit(); err != nil {
				return model.Assignment{}, false, fmt.Errorf("commit unchanged assignment state: %w", err)
			}
			return assignment, false, nil
		}
	}
	generation := uint64(currentGeneration + 1)
	stamp := input.CreatedAt.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO assignment_revisions
		(node_id, generation, digest, payload, created_at) VALUES (?, ?, ?, ?, ?)`,
		input.NodeID, generation, input.Digest, string(input.Payload), stamp); err != nil {
		return model.Assignment{}, false, fmt.Errorf("insert assignment revision: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM desired_allocations WHERE node_id = ?`, input.NodeID); err != nil {
		return model.Assignment{}, false, fmt.Errorf("replace desired allocations: %w", err)
	}
	for _, allocation := range input.Allocations {
		encoded, err := json.Marshal(allocation)
		if err != nil {
			return model.Assignment{}, false, fmt.Errorf("encode desired allocation %q: %w", allocation.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO desired_allocations
			(id, node_id, application_name, service_name, replica,
			 application_generation, revision_id, spec_digest, spec, assignment_generation)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, allocation.ID, input.NodeID,
			allocation.Application, allocation.Service, allocation.Replica,
			allocation.ApplicationGeneration, allocation.RevisionID,
			allocation.SpecDigest, string(encoded), generation); err != nil {
			return model.Assignment{}, false, fmt.Errorf("insert desired allocation %q: %w", allocation.ID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET assignment_generation = ? WHERE id = ?`, generation, input.NodeID); err != nil {
		return model.Assignment{}, false, fmt.Errorf("advance node assignment: %w", err)
	}
	for name, execution := range input.ApplicationStates {
		if execution.UpdatedAt.IsZero() {
			execution.UpdatedAt = input.CreatedAt.UTC()
		}
		if _, err := upsertExecutionTx(ctx, tx, name, execution, false); err != nil {
			return model.Assignment{}, false, err
		}
	}
	var clusterID string
	if err := tx.QueryRowContext(ctx, "SELECT id FROM clusters WHERE singleton = 1").Scan(&clusterID); err != nil {
		return model.Assignment{}, false, fmt.Errorf("read cluster for assignment event: %w", err)
	}
	payload, _ := json.Marshal(struct {
		NodeID      string `json:"node_id"`
		Generation  uint64 `json:"generation"`
		Digest      string `json:"digest"`
		Allocations int    `json:"allocations"`
	}{input.NodeID, generation, input.Digest, len(input.Allocations)})
	if _, err := tx.ExecContext(ctx, `INSERT INTO events
		(cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
		clusterID, model.EventAssignmentUpdated, string(payload), stamp); err != nil {
		return model.Assignment{}, false, fmt.Errorf("record assignment event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Assignment{}, false, fmt.Errorf("commit assignment: %w", err)
	}
	return model.Assignment{
		NodeID: input.NodeID, Generation: generation, Digest: input.Digest,
		Payload: append([]byte(nil), input.Payload...), Allocations: cloneAllocations(input.Allocations),
		CreatedAt: input.CreatedAt.UTC(),
	}, true, nil
}

func (s *Store) CurrentAssignment(ctx context.Context, nodeID string) (model.Assignment, error) {
	var generation int64
	if err := s.db.QueryRowContext(ctx, `SELECT assignment_generation FROM nodes WHERE id = ?`, nodeID).Scan(&generation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Assignment{}, ErrNodeNotFound
		}
		return model.Assignment{}, fmt.Errorf("read current assignment generation: %w", err)
	}
	if generation == 0 {
		return model.Assignment{}, ErrAssignmentNotFound
	}
	return scanAssignment(s.db.QueryRowContext(ctx, `SELECT node_id, generation, digest, payload, created_at
		FROM assignment_revisions WHERE node_id = ? AND generation = ?`, nodeID, generation))
}

func assignmentInTransaction(ctx context.Context, tx *sql.Tx, nodeID string, generation uint64) (model.Assignment, error) {
	return scanAssignment(tx.QueryRowContext(ctx, `SELECT node_id, generation, digest, payload, created_at
		FROM assignment_revisions WHERE node_id = ? AND generation = ?`, nodeID, generation))
}

func scanAssignment(row rowScanner) (model.Assignment, error) {
	var assignment model.Assignment
	var generation int64
	var payload, createdAt string
	if err := row.Scan(&assignment.NodeID, &generation, &assignment.Digest, &payload, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Assignment{}, ErrAssignmentNotFound
		}
		return model.Assignment{}, fmt.Errorf("read assignment: %w", err)
	}
	if generation < 1 || !json.Valid([]byte(payload)) {
		return model.Assignment{}, fmt.Errorf("read assignment: invalid stored values")
	}
	assignment.Generation = uint64(generation)
	assignment.Payload = json.RawMessage(payload)
	if err := json.Unmarshal([]byte(payload), &assignment.Allocations); err != nil {
		return model.Assignment{}, fmt.Errorf("decode assignment allocations: %w", err)
	}
	var err error
	assignment.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return model.Assignment{}, fmt.Errorf("read assignment creation time: %w", err)
	}
	return assignment, nil
}

func (s *Store) AcknowledgeAssignment(ctx context.Context, nodeID string, generation uint64, digest string) error {
	if nodeID == "" || generation == 0 || digest == "" {
		return fmt.Errorf("node, assignment generation, and digest are required")
	}
	var storedDigest string
	if err := s.db.QueryRowContext(ctx, `SELECT digest FROM assignment_revisions
		WHERE node_id = ? AND generation = ?`, nodeID, generation).Scan(&storedDigest); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAssignmentNotFound
		}
		return fmt.Errorf("read acknowledged assignment: %w", err)
	}
	if storedDigest != digest {
		return fmt.Errorf("assignment acknowledgement digest does not match")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE nodes SET
		acknowledged_generation = CASE WHEN acknowledged_generation < ? THEN ? ELSE acknowledged_generation END,
		acknowledged_digest = CASE WHEN acknowledged_generation < ? THEN ? ELSE acknowledged_digest END
		WHERE id = ? AND assignment_generation >= ?`, generation, generation,
		generation, digest, nodeID, generation)
	if err != nil {
		return fmt.Errorf("record assignment acknowledgement: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return ErrNodeNotFound
	}
	return nil
}

// UpdateApplicationExecutions records controller-side pending or blocked
// decisions when no assignment can be sent.
func (s *Store) UpdateApplicationExecutions(ctx context.Context, states map[string]model.ApplicationExecution) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin execution update: %w", err)
	}
	defer tx.Rollback()
	for name, execution := range states {
		if _, err := upsertExecutionTx(ctx, tx, name, execution, true); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit execution update: %w", err)
	}
	return nil
}

func (s *Store) RecordAgentReport(ctx context.Context, report model.AgentReport) (bool, error) {
	if report.NodeID == "" || report.BootID == "" || report.Sequence == 0 || report.ReportedAt.IsZero() {
		return false, fmt.Errorf("node, boot id, report sequence, and report time are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin agent report: %w", err)
	}
	defer tx.Rollback()
	var bootID, assignmentDigest, acknowledgedDigest string
	var lastSequence, assignmentGeneration, acknowledgedGeneration int64
	if err := tx.QueryRowContext(ctx, `SELECT last_boot_id, last_report_sequence,
		assignment_generation, acknowledged_generation, acknowledged_digest,
		COALESCE((SELECT digest FROM assignment_revisions
		WHERE node_id = nodes.id AND generation = nodes.assignment_generation), '')
		FROM nodes WHERE id = ?`, report.NodeID).Scan(&bootID, &lastSequence, &assignmentGeneration,
		&acknowledgedGeneration, &acknowledgedDigest, &assignmentDigest); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNodeNotFound
		}
		return false, fmt.Errorf("read node report cursor: %w", err)
	}
	if bootID != report.BootID || report.AssignmentGeneration != uint64(assignmentGeneration) ||
		report.AssignmentGeneration != uint64(acknowledgedGeneration) ||
		report.AssignmentDigest != assignmentDigest || report.AssignmentDigest != acknowledgedDigest {
		return false, ErrStaleAgentReport
	}
	if report.Sequence <= uint64(lastSequence) {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM allocation_observations WHERE node_id = ?`, report.NodeID); err != nil {
		return false, fmt.Errorf("replace allocation observations: %w", err)
	}
	for _, observation := range report.Allocations {
		if err := validateObservation(observation); err != nil {
			return false, err
		}
		var desiredApplication, desiredService string
		var desiredReplica int64
		if err := tx.QueryRowContext(ctx, `SELECT application_name, service_name, replica FROM desired_allocations
			WHERE id = ? AND node_id = ? AND application_generation = ? AND spec_digest = ?`,
			observation.ID, report.NodeID, observation.ApplicationGeneration, observation.SpecDigest).Scan(
			&desiredApplication, &desiredService, &desiredReplica); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, fmt.Errorf("allocation %q is not in the acknowledged assignment", observation.ID)
			}
			return false, fmt.Errorf("validate allocation report: %w", err)
		}
		if observation.Application != desiredApplication || observation.Service != desiredService ||
			uint64(observation.Replica) != uint64(desiredReplica) {
			return false, fmt.Errorf("allocation %q identity does not match the acknowledged assignment", observation.ID)
		}
		var exitCode any
		if observation.ExitCode != nil {
			exitCode = *observation.ExitCode
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO allocation_observations
			(id, node_id, application_name, service_name, replica, application_generation,
			 spec_digest, phase, reason, message, exit_code, restart_count, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, observation.ID, report.NodeID,
			observation.Application, observation.Service, observation.Replica,
			observation.ApplicationGeneration, observation.SpecDigest, observation.Phase,
			observation.Reason, observation.Message, exitCode, observation.RestartCount,
			observation.UpdatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return false, fmt.Errorf("record allocation observation %q: %w", observation.ID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET last_report_sequence = ?, last_seen_at = ?
		WHERE id = ? AND last_boot_id = ?`, report.Sequence,
		report.ReportedAt.UTC().Format(time.RFC3339Nano), report.NodeID, report.BootID); err != nil {
		return false, fmt.Errorf("advance agent report cursor: %w", err)
	}
	if err := deriveApplicationExecutions(ctx, tx, report.ReportedAt.UTC()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit agent report: %w", err)
	}
	return true, nil
}

func validateObservation(observation model.AllocationStatus) error {
	if observation.ID == "" || observation.Application == "" || observation.Service == "" ||
		observation.Replica == 0 || observation.ApplicationGeneration == 0 ||
		observation.SpecDigest == "" || observation.UpdatedAt.IsZero() {
		return fmt.Errorf("allocation observation is incomplete")
	}
	switch observation.Phase {
	case model.AllocationPending, model.AllocationPulling, model.AllocationStarting,
		model.AllocationRunning, model.AllocationStopped, model.AllocationBackoff, model.AllocationFailed:
	default:
		return fmt.Errorf("allocation %q has invalid phase %q", observation.ID, observation.Phase)
	}
	return nil
}

func deriveApplicationExecutions(ctx context.Context, tx *sql.Tx, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT a.name, a.generation, a.deleted_at,
		COALESCE(x.status, 'PENDING') FROM applications a
		LEFT JOIN application_execution x ON x.application_name = a.name ORDER BY a.name`)
	if err != nil {
		return fmt.Errorf("read applications for execution derivation: %w", err)
	}
	type applicationRow struct {
		name       string
		generation uint64
		deleted    bool
		status     string
	}
	var applications []applicationRow
	for rows.Next() {
		var item applicationRow
		var generation int64
		var deleted sql.NullString
		if err := rows.Scan(&item.name, &generation, &deleted, &item.status); err != nil {
			rows.Close()
			return fmt.Errorf("scan application for execution derivation: %w", err)
		}
		item.generation = uint64(generation)
		item.deleted = deleted.Valid
		applications = append(applications, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, application := range applications {
		execution := model.ApplicationExecution{UpdatedAt: now}
		var desired, observed, running, failed int
		if err := tx.QueryRowContext(ctx, `SELECT count(*),
			count(o.id),
			COALESCE(sum(CASE WHEN o.phase = 'RUNNING' THEN 1 ELSE 0 END), 0),
			COALESCE(sum(CASE WHEN o.phase IN ('FAILED', 'BACKOFF', 'STOPPED') THEN 1 ELSE 0 END), 0)
			FROM desired_allocations d LEFT JOIN allocation_observations o ON o.id = d.id
			WHERE d.application_name = ?`, application.name).Scan(&desired, &observed, &running, &failed); err != nil {
			return fmt.Errorf("summarize application execution: %w", err)
		}
		if application.deleted {
			if desired != 0 {
				continue
			}
			execution.Status = model.ExecutionRemoved
			execution.ObservedGeneration = application.generation
			execution.Reason = "runtime_objects_removed"
			execution.Message = "the agent reports no desired runtime objects for the removed application"
			if _, err := upsertExecutionTx(ctx, tx, application.name, execution, true); err != nil {
				return err
			}
			continue
		}
		if desired == 0 && application.status == model.ExecutionBlocked {
			continue
		}
		execution.ObservedGeneration = application.generation
		switch {
		case desired == 0:
			execution.Status = model.ExecutionPending
			execution.Reason = "assignment_empty"
			execution.Message = "no executable allocations are assigned"
		case observed == desired && running == desired:
			execution.Status = model.ExecutionRunning
			execution.Reason = "all_processes_running"
			execution.Message = "all desired container processes are running; health and routing are not evaluated"
		case running > 0 && failed > 0:
			execution.Status = model.ExecutionDegraded
			execution.Reason = "allocation_failures"
			execution.Message = "some desired processes are running while others are failed or backing off"
		case running == 0 && failed > 0 && observed == desired:
			execution.Status = model.ExecutionFailed
			execution.Reason = "all_allocations_failed"
			execution.Message = "no desired process is running"
		default:
			execution.Status = model.ExecutionStarting
			execution.Reason = "allocations_starting"
			execution.Message = "the agent is reconciling desired processes"
		}
		if _, err := upsertExecutionTx(ctx, tx, application.name, execution, true); err != nil {
			return err
		}
	}
	return nil
}

func upsertExecutionTx(ctx context.Context, tx *sql.Tx, name string, execution model.ApplicationExecution, recordEvent bool) (bool, error) {
	if !validExecutionStatus(execution.Status) || execution.UpdatedAt.IsZero() {
		return false, fmt.Errorf("invalid execution state for application %q", name)
	}
	var oldStatus, oldReason, oldMessage string
	var oldGeneration int64
	err := tx.QueryRowContext(ctx, `SELECT status, observed_generation, reason, message
		FROM application_execution WHERE application_name = ?`, name).Scan(&oldStatus, &oldGeneration, &oldReason, &oldMessage)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("read application execution state: %w", err)
	}
	changed := errors.Is(err, sql.ErrNoRows) || oldStatus != execution.Status || uint64(oldGeneration) != execution.ObservedGeneration || oldReason != execution.Reason || oldMessage != execution.Message
	if !changed {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO application_execution
		(application_name, status, observed_generation, reason, message, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(application_name) DO UPDATE SET status = excluded.status,
		observed_generation = excluded.observed_generation, reason = excluded.reason,
		message = excluded.message, updated_at = excluded.updated_at`, name,
		execution.Status, execution.ObservedGeneration, execution.Reason, execution.Message,
		execution.UpdatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return false, fmt.Errorf("write application execution state: %w", err)
	}
	if changed && recordEvent {
		var clusterID string
		if err := tx.QueryRowContext(ctx, "SELECT id FROM clusters WHERE singleton = 1").Scan(&clusterID); err != nil {
			return false, fmt.Errorf("read cluster for execution event: %w", err)
		}
		payload, _ := json.Marshal(struct {
			Name               string `json:"name"`
			Status             string `json:"status"`
			ObservedGeneration uint64 `json:"observed_generation"`
			Reason             string `json:"reason"`
		}{name, execution.Status, execution.ObservedGeneration, execution.Reason})
		if _, err := tx.ExecContext(ctx, `INSERT INTO events
			(cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
			clusterID, model.EventApplicationStatusChanged, string(payload),
			execution.UpdatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return false, fmt.Errorf("record application execution event: %w", err)
		}
	}
	return changed, nil
}

func validExecutionStatus(status string) bool {
	switch status {
	case model.ExecutionPending, model.ExecutionBlocked, model.ExecutionStarting,
		model.ExecutionRunning, model.ExecutionDegraded, model.ExecutionFailed,
		model.ExecutionStopping, model.ExecutionRemoved:
		return true
	default:
		return false
	}
}

func (s *Store) DesiredAllocation(ctx context.Context, application, service string, replica uint32) (model.AllocationDesired, error) {
	var encoded string
	err := s.db.QueryRowContext(ctx, `SELECT spec FROM desired_allocations
		WHERE application_name = ? AND service_name = ? AND replica = ?`,
		application, service, replica).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return model.AllocationDesired{}, ErrAssignmentNotFound
	}
	if err != nil {
		return model.AllocationDesired{}, fmt.Errorf("read desired allocation: %w", err)
	}
	var allocation model.AllocationDesired
	if err := json.Unmarshal([]byte(encoded), &allocation); err != nil {
		return model.AllocationDesired{}, fmt.Errorf("decode desired allocation: %w", err)
	}
	return allocation, nil
}

func cloneAllocations(input []model.AllocationDesired) []model.AllocationDesired {
	result := append([]model.AllocationDesired(nil), input...)
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
