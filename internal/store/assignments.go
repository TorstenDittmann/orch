package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"time"

	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
)

var ErrAssignmentNotFound = errors.New("assignment was not found")

// PublishAssignment atomically writes an immutable full assignment, replaces
// the desired-allocation view, and updates application execution decisions.
func (s *Store) PublishAssignment(ctx context.Context, input model.PublishAssignment) (model.Assignment, bool, error) {
	if input.NodeID == "" || input.CreatedAt.IsZero() || !json.Valid(input.Payload) {
		return model.Assignment{}, false, fmt.Errorf("node, creation time, and valid assignment payload are required")
	}
	canonical, digest, err := model.AssignmentPayloadWithJobs(input.Network, input.Allocations, input.CronJobs)
	if err != nil {
		return model.Assignment{}, false, err
	}
	if input.Digest != digest {
		return model.Assignment{}, false, fmt.Errorf("assignment digest does not match payload")
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
		digestMatches, err := model.AllocationSpecDigestMatches(allocation)
		if err != nil || !digestMatches {
			return model.Assignment{}, false, fmt.Errorf("desired allocation %q spec digest does not match", allocation.ID)
		}
	}
	previousJob := ""
	for _, job := range input.CronJobs {
		key := job.Application + "\x00" + job.Job
		if previousJob != "" && key <= previousJob {
			return model.Assignment{}, false, fmt.Errorf("desired cron jobs are not in strict canonical order")
		}
		previousJob = key
		if job.Application == "" || job.Job == "" || job.ApplicationGeneration == 0 || job.RevisionID == "" || job.SpecDigest == "" {
			return model.Assignment{}, false, fmt.Errorf("desired cron job is incomplete")
		}
		digest, err := model.CronJobSpecDigest(job)
		if err != nil || digest != job.SpecDigest {
			return model.Assignment{}, false, fmt.Errorf("desired cron job %q spec digest does not match", key)
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM desired_cron_jobs WHERE node_id = ?`, input.NodeID); err != nil {
		return model.Assignment{}, false, fmt.Errorf("replace desired cron jobs: %w", err)
	}
	for _, job := range input.CronJobs {
		encoded, err := json.Marshal(job)
		if err != nil {
			return model.Assignment{}, false, fmt.Errorf("encode desired cron job %s/%s: %w", job.Application, job.Job, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO desired_cron_jobs
			(node_id, application_name, job_name, application_generation, revision_id, spec_digest, spec, assignment_generation)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, input.NodeID, job.Application, job.Job,
			job.ApplicationGeneration, job.RevisionID, job.SpecDigest, string(encoded), generation); err != nil {
			return model.Assignment{}, false, fmt.Errorf("insert desired cron job %s/%s: %w", job.Application, job.Job, err)
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
		CronJobs    int    `json:"cron_jobs"`
	}{input.NodeID, generation, input.Digest, len(input.Allocations), len(input.CronJobs)})
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
		Payload: append([]byte(nil), input.Payload...), Allocations: cloneAllocations(input.Allocations), CronJobs: cloneCronJobs(input.CronJobs),
		Network:   input.Network,
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
	var err error
	assignment.Network, assignment.Allocations, assignment.CronJobs, err = model.DecodeAssignmentDocument([]byte(payload))
	if err != nil {
		return model.Assignment{}, err
	}
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
	var bootID, assignmentDigest, acknowledgedDigest, assignmentPayload string
	var lastSequence, assignmentGeneration, acknowledgedGeneration int64
	if err := tx.QueryRowContext(ctx, `SELECT last_boot_id, last_report_sequence,
		assignment_generation, acknowledged_generation, acknowledged_digest,
		COALESCE((SELECT digest FROM assignment_revisions
		WHERE node_id = nodes.id AND generation = nodes.assignment_generation), '')
		, COALESCE((SELECT payload FROM assignment_revisions
		WHERE node_id = nodes.id AND generation = nodes.assignment_generation), '')
		FROM nodes WHERE id = ?`, report.NodeID).Scan(&bootID, &lastSequence, &assignmentGeneration,
		&acknowledgedGeneration, &acknowledgedDigest, &assignmentDigest, &assignmentPayload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNodeNotFound
		}
		return false, fmt.Errorf("read node report cursor: %w", err)
	}
	if bootID != report.BootID {
		return false, ErrStaleAgentReport
	}
	// A report for the immediately superseded assignment can already be in the
	// agent's outbound queue when a new full snapshot arrives. It carries no
	// authority over the current desired rows, so ignore it without advancing
	// the report cursor or tearing down the otherwise healthy stream.
	if report.AssignmentGeneration < uint64(assignmentGeneration) {
		return false, nil
	}
	if report.AssignmentGeneration != uint64(assignmentGeneration) ||
		report.AssignmentGeneration != uint64(acknowledgedGeneration) ||
		report.AssignmentDigest != assignmentDigest || report.AssignmentDigest != acknowledgedDigest {
		return false, ErrStaleAgentReport
	}
	if report.Sequence <= uint64(lastSequence) {
		return false, nil
	}
	assignmentNetwork, assignmentAllocations, assignmentCronJobs, err := model.DecodeAssignmentDocument([]byte(assignmentPayload))
	if err != nil {
		return false, fmt.Errorf("decode acknowledged assignment network: %w", err)
	}
	if assignmentNetwork.Generation != 0 && report.Network.Generation == 0 {
		return false, fmt.Errorf("network observation is required for the acknowledged assignment")
	}
	if report.Network.Generation != 0 {
		if err := validateNetworkObservation(report.Network, assignmentNetwork); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE nodes SET node_subnet = ?, network_generation = ?,
			network_state = ?, network_reason = ?, network_message = ?, dns_state = ?, ingress_state = ?,
			draining_objects = ?
			WHERE id = ?`, report.Network.NodeSubnet, report.Network.Generation, report.Network.State,
			report.Network.Reason, report.Network.Message, report.Network.DNSState,
			report.Network.IngressState, report.Network.DrainingObjects, report.NodeID); err != nil {
			return false, fmt.Errorf("record node network observation: %w", err)
		}
		if report.Network.State == model.NetworkReady {
			if err := freezeNetworkFromReport(ctx, tx, report.Network, report.ReportedAt.UTC()); err != nil {
				return false, err
			}
		}
	}
	previousHealth, err := allocationHealthStates(ctx, tx, report.NodeID)
	if err != nil {
		return false, err
	}
	previousRoutes, err := routeTransitionStates(ctx, tx, report.NodeID)
	if err != nil {
		return false, err
	}
	var clusterID string
	if err := tx.QueryRowContext(ctx, `SELECT cluster_id FROM nodes WHERE id = ?`, report.NodeID).Scan(&clusterID); err != nil {
		return false, fmt.Errorf("read report cluster identity: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM allocation_observations WHERE node_id = ?`, report.NodeID); err != nil {
		return false, fmt.Errorf("replace allocation observations: %w", err)
	}
	for _, observation := range report.Allocations {
		if observation.HealthStatus == "" {
			observation.HealthStatus = model.HealthNotConfigured
		}
		if err := validateObservation(observation); err != nil {
			return false, err
		}
		var desiredApplication, desiredService, desiredSpec string
		var desiredReplica int64
		if err := tx.QueryRowContext(ctx, `SELECT application_name, service_name, replica, spec FROM desired_allocations
			WHERE id = ? AND node_id = ? AND application_generation = ? AND spec_digest = ?`,
			observation.ID, report.NodeID, observation.ApplicationGeneration, observation.SpecDigest).Scan(
			&desiredApplication, &desiredService, &desiredReplica, &desiredSpec); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, fmt.Errorf("allocation %q is not in the acknowledged assignment", observation.ID)
			}
			return false, fmt.Errorf("validate allocation report: %w", err)
		}
		if observation.Application != desiredApplication || observation.Service != desiredService ||
			uint64(observation.Replica) != uint64(desiredReplica) {
			return false, fmt.Errorf("allocation %q identity does not match the acknowledged assignment", observation.ID)
		}
		var desired model.AllocationDesired
		if err := json.Unmarshal([]byte(desiredSpec), &desired); err != nil {
			return false, fmt.Errorf("decode desired allocation %q: %w", observation.ID, err)
		}
		if observation.ActiveSpecDigest == "" && observation.Phase == model.AllocationRunning &&
			observation.HealthStatus == model.HealthNotConfigured {
			observation.ActiveSpecDigest = desired.SpecDigest
			observation.ActiveGeneration = desired.ApplicationGeneration
			observation.ActiveRevisionID = desired.RevisionID
		}
		if err := validateObservationAgainstDesired(observation, desired, assignmentNetwork); err != nil {
			return false, err
		}
		var exitCode any
		if observation.ExitCode != nil {
			exitCode = *observation.ExitCode
		}
		var readyAt any
		if observation.ReadyAt != nil {
			readyAt = observation.ReadyAt.UTC().Format(time.RFC3339Nano)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO allocation_observations
			(id, node_id, application_name, service_name, replica, application_generation,
			 spec_digest, phase, reason, message, exit_code, restart_count, updated_at,
			 health_status, address, active_spec_digest, active_generation, active_revision_id, ready_at, declared_port)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, observation.ID, report.NodeID,
			observation.Application, observation.Service, observation.Replica,
			observation.ApplicationGeneration, observation.SpecDigest, observation.Phase,
			observation.Reason, observation.Message, exitCode, observation.RestartCount,
			observation.UpdatedAt.UTC().Format(time.RFC3339Nano), observation.HealthStatus,
			observation.Address, observation.ActiveSpecDigest, observation.ActiveGeneration,
			observation.ActiveRevisionID, readyAt, observation.DeclaredPort); err != nil {
			return false, fmt.Errorf("record allocation observation %q: %w", observation.ID, err)
		}
		kind := ""
		if observation.HealthStatus == model.HealthReady && previousHealth[observation.ID] != model.HealthReady {
			kind = model.EventAllocationReady
		} else if observation.HealthStatus == model.HealthUnhealthy && previousHealth[observation.ID] != model.HealthUnhealthy {
			kind = model.EventAllocationUnhealthy
		}
		if kind != "" {
			payload, _ := json.Marshal(struct {
				Application string `json:"application"`
				Service     string `json:"service"`
				Replica     uint32 `json:"replica"`
				Generation  uint64 `json:"generation"`
			}{observation.Application, observation.Service, observation.Replica, observation.ApplicationGeneration})
			if err := insertObservedTransition(ctx, tx, clusterID, kind, payload, report.ReportedAt); err != nil {
				return false, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM route_observations WHERE node_id = ?`, report.NodeID); err != nil {
		return false, fmt.Errorf("replace route observations: %w", err)
	}
	desiredTCP := make(map[uint32]model.AllocationDesired)
	for _, allocation := range assignmentAllocations {
		if allocation.TCP != nil && allocation.TCP.PublicPort != 0 {
			desiredTCP[allocation.TCP.PublicPort] = allocation
		}
		if allocation.Rollback != nil && allocation.Rollback.TCP != nil && allocation.Rollback.TCP.PublicPort != 0 {
			desiredTCP[allocation.Rollback.TCP.PublicPort] = allocation
		}
	}
	for _, endpoint := range assignmentNetwork.Endpoints {
		if endpoint.TCPPublicPort == 0 {
			continue
		}
		desiredTCP[endpoint.TCPPublicPort] = model.AllocationDesired{
			Application: endpoint.Application, Service: endpoint.Service,
			TCP: &model.TCPDesired{Port: endpoint.Port, PublicPort: endpoint.TCPPublicPort},
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tcp_route_observations WHERE node_id = ?`, report.NodeID); err != nil {
		return false, fmt.Errorf("replace TCP route observations: %w", err)
	}
	previousPort := uint32(0)
	for _, route := range report.TCPRoutes {
		desired, ok := desiredTCP[route.PublicPort]
		contractMatches := ok && ((desired.TCP != nil && desired.TCP.PublicPort == route.PublicPort && desired.TCP.Port == route.TargetPort) ||
			(desired.Rollback != nil && desired.Rollback.TCP != nil && desired.Rollback.TCP.PublicPort == route.PublicPort && desired.Rollback.TCP.Port == route.TargetPort))
		if !contractMatches || desired.Application != route.Application || desired.Service != route.Service ||
			(previousPort != 0 && route.PublicPort <= previousPort) || route.UpdatedAt.IsZero() || !validRouteState(route.State) {
			return false, fmt.Errorf("TCP route observation for port %d is not authorized", route.PublicPort)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tcp_route_observations
			(node_id, application_name, service_name, public_port, target_port, state, upstream_count, reason, message, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, report.NodeID, route.Application, route.Service, route.PublicPort,
			route.TargetPort, route.State, route.UpstreamCount, route.Reason, route.Message, route.UpdatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return false, fmt.Errorf("record TCP route observation: %w", err)
		}
		previousPort = route.PublicPort
	}
	if err := recordNodePortObservations(ctx, tx, report, assignmentAllocations); err != nil {
		return false, err
	}
	desiredJobs := make(map[string]model.CronJobDesired, len(assignmentCronJobs))
	for _, job := range assignmentCronJobs {
		desiredJobs[job.Application+"\x00"+job.Job] = job
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM cron_job_observations WHERE node_id = ?`, report.NodeID); err != nil {
		return false, fmt.Errorf("replace cron job observations: %w", err)
	}
	previousJob := ""
	for _, observation := range report.CronJobs {
		key := observation.Application + "\x00" + observation.Job
		desired, ok := desiredJobs[key]
		if !ok || observation.SpecDigest != desired.SpecDigest || observation.UpdatedAt.IsZero() || (previousJob != "" && key <= previousJob) {
			return false, fmt.Errorf("cron job observation %q is not authorized or ordered", key)
		}
		var next any
		if observation.NextRunAt != nil {
			next = observation.NextRunAt.UTC().Format(time.RFC3339Nano)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO cron_job_observations
			(node_id, application_name, job_name, spec_digest, next_run_at, active_run_id, last_run_id, last_state, reason, message, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, report.NodeID, observation.Application, observation.Job,
			observation.SpecDigest, next, observation.ActiveRunID, observation.LastRunID, observation.LastState,
			observation.Reason, observation.Message, observation.UpdatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return false, fmt.Errorf("record cron job observation %q: %w", key, err)
		}
		previousJob = key
	}
	for _, run := range report.JobRuns {
		key := run.Application + "\x00" + run.Job
		desired, ok := desiredJobs[key]
		if !ok || run.SpecDigest != desired.SpecDigest || run.ID == "" || run.ScheduledAt.IsZero() || run.UpdatedAt.IsZero() || !validJobRunState(run.State) {
			return false, fmt.Errorf("cron run %q is not authorized", run.ID)
		}
		if err := observeOneOffTx(ctx, tx, report.NodeID, desired, run); err != nil {
			return false, err
		}
		var exit, started, finished any
		if run.ExitCode != nil {
			exit = *run.ExitCode
		}
		if run.StartedAt != nil {
			started = run.StartedAt.UTC().Format(time.RFC3339Nano)
		}
		if run.FinishedAt != nil {
			finished = run.FinishedAt.UTC().Format(time.RFC3339Nano)
		}
		var previousState string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM cron_run_observations WHERE id = ?`, run.ID).Scan(&previousState); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("read cron run %q: %w", run.ID, err)
		}
		if (run.State == model.JobRunFailed || run.State == model.JobRunTimedOut) && previousState != run.State {
			payload, _ := json.Marshal(model.CronRunFailed{Application: run.Application, Job: run.Job, RunID: run.ID,
				State: run.State, ExitCode: run.ExitCode, Reason: run.Reason, Message: run.Message})
			if err := insertObservedTransition(ctx, tx, clusterID, model.EventCronRunFailed, payload, report.ReportedAt); err != nil {
				return false, err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO cron_run_observations
			(id, node_id, application_name, job_name, spec_digest, scheduled_at, state, exit_code, started_at, finished_at, reason, message, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET state=excluded.state,
			exit_code=excluded.exit_code, started_at=excluded.started_at, finished_at=excluded.finished_at,
			reason=excluded.reason, message=excluded.message, updated_at=excluded.updated_at`, run.ID, report.NodeID,
			run.Application, run.Job, run.SpecDigest, run.ScheduledAt.UTC().Format(time.RFC3339Nano), run.State,
			exit, started, finished, run.Reason, run.Message, run.UpdatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return false, fmt.Errorf("record cron run %q: %w", run.ID, err)
		}
	}
	for _, job := range assignmentCronJobs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM cron_run_observations WHERE id IN (
			SELECT id FROM cron_run_observations WHERE node_id=? AND application_name=? AND job_name=?
			AND state NOT IN ('WAITING', 'RUNNING')
			ORDER BY scheduled_at DESC LIMIT -1 OFFSET 20)`, report.NodeID, job.Application, job.Job); err != nil {
			return false, fmt.Errorf("prune cron run observations: %w", err)
		}
	}
	previousDomain := ""
	for _, route := range report.Routes {
		if err := validateRouteObservation(ctx, tx, report.NodeID, assignmentNetwork, route); err != nil {
			return false, err
		}
		if previousDomain != "" && route.Domain <= previousDomain {
			return false, fmt.Errorf("route observations are not in strict canonical order")
		}
		previousDomain = route.Domain
		if _, err := tx.ExecContext(ctx, `INSERT INTO route_observations
			(domain, node_id, application_name, service_name, https, state, certificate_state,
			 upstream_count, reason, message, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			route.Domain, report.NodeID, route.Application, route.Service, boolInt(route.HTTPS),
			route.State, route.CertificateState, route.UpstreamCount, route.Reason, route.Message,
			route.UpdatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return false, fmt.Errorf("record route observation %q: %w", route.Domain, err)
		}
		previous := previousRoutes[route.Domain]
		kind := ""
		if route.State == model.RouteActive && previous.state != model.RouteActive {
			kind = model.EventRouteActive
		} else if route.State == model.RouteFailed && (previous.state != model.RouteFailed || previous.reason != route.Reason) {
			kind = model.EventRouteFailed
		}
		if kind != "" {
			payload, _ := json.Marshal(struct {
				Application string `json:"application"`
				Service     string `json:"service"`
				Domain      string `json:"domain"`
				Reason      string `json:"reason,omitempty"`
			}{route.Application, route.Service, route.Domain, route.Reason})
			if err := insertObservedTransition(ctx, tx, clusterID, kind, payload, report.ReportedAt); err != nil {
				return false, err
			}
		}
	}
	if err := recordVolumeBackupFailuresTx(ctx, tx, clusterID, report, assignmentAllocations); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET last_report_sequence = ?, last_seen_at = ?
		WHERE id = ? AND last_boot_id = ?`, report.Sequence,
		report.ReportedAt.UTC().Format(time.RFC3339Nano), report.NodeID, report.BootID); err != nil {
		return false, fmt.Errorf("advance agent report cursor: %w", err)
	}
	if err := deriveApplicationExecutions(ctx, tx, report.Network, report.ReportedAt.UTC()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit agent report: %w", err)
	}
	return true, nil
}

type routeTransitionState struct {
	state  string
	reason string
}

func allocationHealthStates(ctx context.Context, tx *sql.Tx, nodeID string) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, health_status FROM allocation_observations WHERE node_id = ?`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("read previous allocation health states: %w", err)
	}
	states := make(map[string]string)
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan previous allocation health state: %w", err)
		}
		states[id] = state
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close previous allocation health states: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read previous allocation health states: %w", err)
	}
	return states, nil
}

func routeTransitionStates(ctx context.Context, tx *sql.Tx, nodeID string) (map[string]routeTransitionState, error) {
	rows, err := tx.QueryContext(ctx, `SELECT domain, state, reason FROM route_observations WHERE node_id = ?`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("read previous route states: %w", err)
	}
	states := make(map[string]routeTransitionState)
	for rows.Next() {
		var domain string
		var state routeTransitionState
		if err := rows.Scan(&domain, &state.state, &state.reason); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan previous route state: %w", err)
		}
		states[domain] = state
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close previous route states: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read previous route states: %w", err)
	}
	return states, nil
}

func validRouteState(value string) bool {
	return value == model.RoutePending || value == model.RouteActive || value == model.RouteFailed
}

func validJobRunState(value string) bool {
	switch value {
	case model.JobRunWaiting, model.JobRunRunning, model.JobRunSucceeded, model.JobRunFailed, model.JobRunTimedOut, model.JobRunSkipped:
		return true
	default:
		return false
	}
}

// recordVolumeBackupFailuresTx raises a volume.backup.failed event for each
// backup failure the controller has not seen yet. Only volumes the node's
// assignment backs up may be reported.
func recordVolumeBackupFailuresTx(ctx context.Context, tx *sql.Tx, clusterID string, report model.AgentReport, allocations []model.AllocationDesired) error {
	if len(report.VolumeBackupFailures) == 0 {
		return nil
	}
	backedUp := make(map[string]bool)
	for _, allocation := range allocations {
		for _, policy := range allocation.VolumeBackups {
			backedUp[allocation.Application+"\x00"+policy.Volume] = true
		}
	}
	for _, failure := range report.VolumeBackupFailures {
		if !backedUp[failure.Application+"\x00"+failure.Volume] {
			return fmt.Errorf("volume backup failure %s/%s is not authorized", failure.Application, failure.Volume)
		}
		failedAt := failure.FailedAt.UTC().Format(time.RFC3339Nano)
		var previous string
		err := tx.QueryRowContext(ctx, `SELECT failed_at FROM volume_backup_failures WHERE application_name = ? AND volume_name = ?`,
			failure.Application, failure.Volume).Scan(&previous)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read volume backup failure: %w", err)
		}
		if previous == failedAt {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO volume_backup_failures (application_name, volume_name, failed_at) VALUES (?, ?, ?)
			ON CONFLICT (application_name, volume_name) DO UPDATE SET failed_at = excluded.failed_at`,
			failure.Application, failure.Volume, failedAt); err != nil {
			return fmt.Errorf("record volume backup failure: %w", err)
		}
		payload, _ := json.Marshal(model.VolumeBackupFailed{Application: failure.Application, Volume: failure.Volume,
			NodeID: report.NodeID, FailedAt: failure.FailedAt.UTC(), Message: failure.Message})
		if err := insertObservedTransition(ctx, tx, clusterID, model.EventVolumeBackupFailed, payload, report.ReportedAt); err != nil {
			return err
		}
	}
	return nil
}

func insertObservedTransition(ctx context.Context, tx *sql.Tx, clusterID, kind string, payload []byte, at time.Time) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO events
		(cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
		clusterID, kind, string(payload), at.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("record %s event: %w", kind, err)
	}
	return nil
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
	switch observation.HealthStatus {
	case model.HealthNotConfigured, model.HealthChecking, model.HealthReady, model.HealthUnhealthy:
	default:
		return fmt.Errorf("allocation %q has invalid health state %q", observation.ID, observation.HealthStatus)
	}
	if observation.HealthStatus == model.HealthReady && (observation.Address == "" || observation.ReadyAt == nil) {
		return fmt.Errorf("ready allocation %q is missing its address or ready time", observation.ID)
	}
	return nil
}

func validateObservationAgainstDesired(observation model.AllocationStatus, desired model.AllocationDesired, network model.NetworkConfig) error {
	declaredPort := uint32(0)
	if desired.HTTP != nil {
		declaredPort = desired.HTTP.Port
	} else if desired.TCP != nil {
		declaredPort = desired.TCP.Port
	}
	if observation.DeclaredPort != declaredPort {
		return fmt.Errorf("allocation %q declared port does not match the acknowledged assignment", observation.ID)
	}
	hasDigest := observation.ActiveSpecDigest != ""
	hasGeneration := observation.ActiveGeneration != 0
	hasRevision := observation.ActiveRevisionID != ""
	if hasDigest != hasGeneration || hasDigest != hasRevision {
		return fmt.Errorf("allocation %q has incomplete active revision metadata", observation.ID)
	}
	matchesDesired := false
	matchesRollback := false
	activeHTTP := desired.HTTP
	activeTCP := desired.TCP
	activeNetworkGeneration := desired.NetworkGeneration
	if hasDigest {
		matchesDesired = observation.ActiveSpecDigest == desired.SpecDigest &&
			observation.ActiveGeneration == desired.ApplicationGeneration &&
			observation.ActiveRevisionID == desired.RevisionID
		matchesRollback = desired.Rollback != nil &&
			observation.ActiveSpecDigest == desired.Rollback.SpecDigest &&
			observation.ActiveGeneration == desired.Rollback.ApplicationGeneration &&
			observation.ActiveRevisionID == desired.Rollback.RevisionID
		if !matchesDesired && !matchesRollback {
			return fmt.Errorf("allocation %q active revision is not authorized by the acknowledged assignment", observation.ID)
		}
		if matchesRollback {
			activeHTTP = desired.Rollback.HTTP
			activeTCP = desired.Rollback.TCP
			activeNetworkGeneration = desired.Rollback.NetworkGeneration
		}
	}
	if observation.Phase == model.AllocationRunning && !hasDigest {
		return fmt.Errorf("running allocation %q is missing active revision metadata", observation.ID)
	}
	if observation.HealthStatus == model.HealthReady && (observation.Phase != model.AllocationRunning || !hasDigest) {
		return fmt.Errorf("ready allocation %q is not a running authorized revision", observation.ID)
	}
	if activeHTTP == nil && activeTCP == nil && observation.HealthStatus != model.HealthNotConfigured {
		return fmt.Errorf("allocation %q reports health for a service without an endpoint contract", observation.ID)
	}
	if (activeHTTP != nil || activeTCP != nil) && observation.Phase == model.AllocationRunning && observation.HealthStatus == model.HealthNotConfigured {
		return fmt.Errorf("running endpoint allocation %q is missing its health state", observation.ID)
	}
	if observation.Address == "" {
		if observation.Phase == model.AllocationRunning && activeNetworkGeneration != 0 {
			return fmt.Errorf("networked running allocation %q is missing its address", observation.ID)
		}
		return nil
	}
	address, err := netip.ParseAddr(observation.Address)
	if err != nil || !address.Is4() || !address.IsPrivate() {
		return fmt.Errorf("allocation %q reports an invalid private IPv4 address", observation.ID)
	}
	if network.Generation != 0 {
		subnet, err := netip.ParsePrefix(network.NodeSubnet)
		if err != nil || !subnet.Contains(address) || address == subnet.Masked().Addr() ||
			address == lastPrefixAddress(subnet) || address.String() == network.Gateway {
			return fmt.Errorf("allocation %q address is outside its assigned node subnet", observation.ID)
		}
	}
	return nil
}

func lastPrefixAddress(prefix netip.Prefix) netip.Addr {
	bytes := prefix.Masked().Addr().As4()
	for bit := prefix.Bits(); bit < 32; bit++ {
		bytes[bit/8] |= 1 << uint(7-bit%8)
	}
	return netip.AddrFrom4(bytes)
}

func validateNetworkObservation(observation model.NetworkStatus, desired model.NetworkConfig) error {
	if desired.Generation == 0 || observation.Generation != desired.Generation ||
		observation.ClusterCIDR != desired.ClusterCIDR || observation.NodeSubnet != desired.NodeSubnet ||
		observation.Gateway != desired.Gateway {
		return fmt.Errorf("network observation does not match the acknowledged assignment")
	}
	switch observation.State {
	case model.NetworkPending, model.NetworkReady, model.NetworkFailed:
	default:
		return fmt.Errorf("network observation has invalid state %q", observation.State)
	}
	if !validSubsystemState(observation.DNSState) {
		return fmt.Errorf("network observation has invalid DNS state %q", observation.DNSState)
	}
	if !validSubsystemState(observation.IngressState) {
		return fmt.Errorf("network observation has invalid ingress state %q", observation.IngressState)
	}
	wantIngressState := model.SubsystemNotConfigured
	if desired.IngressEnabled {
		wantIngressState = model.SubsystemReady
	}
	if observation.State == model.NetworkReady &&
		(observation.DNSState != model.SubsystemReady || observation.IngressState != wantIngressState) {
		return fmt.Errorf("network observation cannot be ready while DNS is %q or ingress is %q",
			observation.DNSState, observation.IngressState)
	}
	if len(observation.Reason) > 128 || len(observation.Message) > 1024 {
		return fmt.Errorf("network observation is too large")
	}
	return nil
}

func validSubsystemState(value string) bool {
	switch value {
	case model.SubsystemPending, model.SubsystemReady, model.SubsystemFailed,
		model.SubsystemBlocked, model.SubsystemNotConfigured:
		return true
	default:
		return false
	}
}

func freezeNetworkFromReport(ctx context.Context, tx *sql.Tx, observation model.NetworkStatus, now time.Time) error {
	var clusterID, cidr string
	var generation int64
	var frozenAt sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT id, workload_cidr, network_generation, network_frozen_at
		FROM clusters WHERE singleton = 1`).Scan(&clusterID, &cidr, &generation, &frozenAt); err != nil {
		return fmt.Errorf("read network freeze state: %w", err)
	}
	if uint64(generation) != observation.Generation || cidr != observation.ClusterCIDR {
		return ErrStaleAgentReport
	}
	if frozenAt.Valid {
		return nil
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE clusters SET network_frozen_at = ? WHERE singleton = 1`, stamp); err != nil {
		return fmt.Errorf("freeze network configuration: %w", err)
	}
	payload, _ := json.Marshal(struct {
		CIDR       string `json:"cidr"`
		NodeSubnet string `json:"node_subnet"`
	}{cidr, observation.NodeSubnet})
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
		clusterID, model.EventNetworkReady, string(payload), stamp); err != nil {
		return fmt.Errorf("record network ready event: %w", err)
	}
	return nil
}

func validateRouteObservation(ctx context.Context, tx *sql.Tx, nodeID string, network model.NetworkConfig, route model.RouteStatus) error {
	if route.Application == "" || route.Service == "" || route.Domain == "" || route.UpdatedAt.IsZero() ||
		len(route.CertificateState) > 128 || len(route.Reason) > 128 || len(route.Message) > 1024 {
		return fmt.Errorf("route observation is incomplete")
	}
	switch route.State {
	case model.RoutePending, model.RouteActive, model.RouteFailed:
	default:
		return fmt.Errorf("route %q has invalid state %q", route.Domain, route.State)
	}
	if route.State == model.RouteActive {
		certificate := "disabled"
		if route.HTTPS {
			certificate = "managed"
			if config.IsWildcardDomain(route.Domain) {
				certificate = "on_demand"
			}
		}
		if route.UpstreamCount == 0 || route.CertificateState != certificate {
			return fmt.Errorf("active route %q has no upstream or valid certificate state", route.Domain)
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT d.spec, COALESCE(o.phase, ''),
		COALESCE(o.active_spec_digest, ''), COALESCE(o.active_generation, 0),
		COALESCE(o.active_revision_id, '')
		FROM desired_allocations d LEFT JOIN allocation_observations o ON o.id = d.id
		WHERE d.node_id = ? AND d.application_name = ? AND d.service_name = ? ORDER BY d.replica`,
		nodeID, route.Application, route.Service)
	if err != nil {
		return fmt.Errorf("validate route observation: %w", err)
	}
	defer rows.Close()
	allowed := false
	activeContract := false
	for _, endpoint := range network.Endpoints {
		if endpoint.Application != route.Application || endpoint.Service != route.Service {
			continue
		}
		if endpoint.HTTPNodeLocal && endpoint.NodeID != nodeID {
			continue
		}
		if route.Domain == model.PublicHTTPRoute && endpoint.HTTPPublic && !route.HTTPS {
			allowed = true
			activeContract = true
			continue
		}
		if endpoint.HTTPS != route.HTTPS {
			continue
		}
		for _, domain := range endpoint.Domains {
			if domain == route.Domain {
				allowed = true
				activeContract = true
				break
			}
		}
	}
	for rows.Next() {
		var encoded, phase, activeDigest, activeRevision string
		var activeGeneration uint64
		if err := rows.Scan(&encoded, &phase, &activeDigest, &activeGeneration, &activeRevision); err != nil {
			return err
		}
		var allocation model.AllocationDesired
		if err := json.Unmarshal([]byte(encoded), &allocation); err != nil {
			return err
		}
		intents := []*model.HTTPDesired{allocation.HTTP}
		activeIntent := allocation.HTTP
		if allocation.Rollback != nil {
			intents = append(intents, allocation.Rollback.HTTP)
			if activeDigest == allocation.Rollback.SpecDigest &&
				activeGeneration == allocation.Rollback.ApplicationGeneration &&
				activeRevision == allocation.Rollback.RevisionID {
				activeIntent = allocation.Rollback.HTTP
			}
		}
		for _, intent := range intents {
			if routeMatchesHTTPIntent(route, intent) {
				allowed = true
				break
			}
		}
		if phase == model.AllocationRunning && activeDigest != "" && routeMatchesHTTPIntent(route, activeIntent) {
			activeContract = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !allowed {
		return fmt.Errorf("route %q is not in the acknowledged assignment", route.Domain)
	}
	if route.State == model.RouteActive && !activeContract {
		return fmt.Errorf("active route %q does not match a running authorized revision", route.Domain)
	}
	return nil
}

func routeMatchesHTTPIntent(route model.RouteStatus, intent *model.HTTPDesired) bool {
	if intent == nil {
		return false
	}
	if route.Domain == model.PublicHTTPRoute {
		return intent.Public && !route.HTTPS
	}
	if intent.HTTPS != route.HTTPS {
		return false
	}
	for _, domain := range intent.Domains {
		if domain == route.Domain {
			return true
		}
	}
	return false
}

func deriveApplicationExecutions(ctx context.Context, tx *sql.Tx, network model.NetworkStatus, now time.Time) error {
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
		var desired, observed, running, failed, serving, endpointAllocations, activeFallbacks int
		var rollbackStates, terminalRollbacks, activeCount int
		var minimumDesiredGeneration, maximumDesiredGeneration int64
		var minimumActiveGeneration, maximumActiveGeneration int64
		var minimumActiveRevision, maximumActiveRevision string
		if err := tx.QueryRowContext(ctx, `SELECT count(*),
			COALESCE(min(d.application_generation), 0), COALESCE(max(d.application_generation), 0),
			count(o.id),
			COALESCE(sum(CASE WHEN o.phase = 'RUNNING' THEN 1 ELSE 0 END), 0),
			COALESCE(sum(CASE WHEN o.phase IN ('FAILED', 'BACKOFF', 'STOPPED') THEN 1 ELSE 0 END), 0),
			COALESCE(sum(CASE WHEN ((CASE
				WHEN o.active_generation != d.application_generation OR o.active_revision_id != d.revision_id
				THEN (json_type(d.spec, '$.rollback.http') IS NULL AND json_type(d.spec, '$.rollback.tcp') IS NULL)
				ELSE (json_type(d.spec, '$.http') IS NULL AND json_type(d.spec, '$.tcp') IS NULL) END)
				AND o.phase = 'RUNNING') OR o.health_status = 'READY' THEN 1 ELSE 0 END), 0),
			COALESCE(sum(CASE WHEN json_type(d.spec, '$.http') IS NOT NULL OR json_type(d.spec, '$.tcp') IS NOT NULL THEN 1 ELSE 0 END), 0),
			COALESCE(sum(CASE WHEN o.active_spec_digest != '' AND
				(o.active_spec_digest != d.spec_digest OR o.active_generation != d.application_generation OR
				o.active_revision_id != d.revision_id) THEN 1 ELSE 0 END), 0),
			COALESCE(sum(CASE WHEN o.reason IN ('health_gate_rollback', 'rollback_revalidating',
				'rollback_revalidation_failed', 'rollback_start_failed', 'rollback_budget_wait') THEN 1 ELSE 0 END), 0),
			COALESCE(sum(CASE WHEN o.reason = 'health_gate_rollback' AND o.active_spec_digest != '' AND
				(o.active_spec_digest != d.spec_digest OR o.active_generation != d.application_generation OR
				o.active_revision_id != d.revision_id) THEN 1 ELSE 0 END), 0),
			COALESCE(sum(CASE WHEN o.active_generation > 0 AND o.active_revision_id != '' THEN 1 ELSE 0 END), 0),
			COALESCE(min(NULLIF(o.active_generation, 0)), 0), COALESCE(max(o.active_generation), 0),
			COALESCE(min(NULLIF(o.active_revision_id, '')), ''), COALESCE(max(o.active_revision_id), '')
			FROM desired_allocations d LEFT JOIN allocation_observations o ON o.id = d.id
			WHERE d.application_name = ?`, application.name).Scan(&desired,
			&minimumDesiredGeneration, &maximumDesiredGeneration, &observed, &running, &failed,
			&serving, &endpointAllocations, &activeFallbacks, &rollbackStates, &terminalRollbacks,
			&activeCount, &minimumActiveGeneration,
			&maximumActiveGeneration, &minimumActiveRevision, &maximumActiveRevision); err != nil {
			return fmt.Errorf("summarize application execution: %w", err)
		}
		var requiredRoutes, activeRoutes int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum(
			CASE WHEN replica = 1 AND json_type(spec, '$.http.domains') = 'array'
			THEN json_array_length(spec, '$.http.domains') ELSE 0 END), 0) +
			COALESCE(sum(CASE WHEN replica = 1 AND json_extract(spec, '$.http.public') = 1 THEN 1 ELSE 0 END), 0) +
			COALESCE(sum(CASE WHEN replica = 1 AND json_extract(spec, '$.tcp.public_port') > 0 THEN 1 ELSE 0 END), 0)
			FROM desired_allocations WHERE application_name = ?`, application.name).Scan(&requiredRoutes); err != nil {
			return fmt.Errorf("summarize desired application routes: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(DISTINCT domain) FROM route_observations
			WHERE application_name = ? AND state = 'ACTIVE'`, application.name).Scan(&activeRoutes); err != nil {
			return fmt.Errorf("summarize active application routes: %w", err)
		}
		var activeTCPRoutes int
		if err := tx.QueryRowContext(ctx, `SELECT count(DISTINCT public_port) FROM tcp_route_observations
			WHERE application_name = ? AND state = 'ACTIVE'`, application.name).Scan(&activeTCPRoutes); err != nil {
			return fmt.Errorf("summarize active TCP routes: %w", err)
		}
		activeRoutes += activeTCPRoutes
		if desired > 0 && activeCount == desired && minimumActiveGeneration == maximumActiveGeneration &&
			minimumActiveGeneration > 0 && minimumActiveRevision == maximumActiveRevision {
			execution.ActiveGeneration = uint64(minimumActiveGeneration)
			execution.ActiveRevisionID = minimumActiveRevision
		}
		if application.deleted {
			if desired != 0 {
				continue
			}
			var pendingNodes int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM nodes
				WHERE lifecycle != 'REMOVED' AND
				(assignment_generation != acknowledged_generation OR draining_objects != 0)`).Scan(&pendingNodes); err != nil {
				return fmt.Errorf("summarize application removal fences: %w", err)
			}
			if pendingNodes != 0 || (network.Generation != 0 && (network.State != model.NetworkReady || network.DrainingObjects != 0)) {
				execution.Status = model.ExecutionStopping
				execution.ObservedGeneration = application.generation
				execution.Reason = "endpoint_drain_pending"
				execution.Message = "waiting for endpoint withdrawal and runtime cleanup"
				if _, err := upsertExecutionTx(ctx, tx, application.name, execution, true); err != nil {
					return err
				}
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
		var hookPending bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM one_off_jobs j JOIN application_revisions r ON r.id=j.revision_id WHERE j.application_name=? AND r.generation=? AND j.kind='pre_deploy' AND j.rowid=(SELECT max(k.rowid) FROM one_off_jobs k WHERE k.revision_id=j.revision_id AND k.kind='pre_deploy') AND json_extract(j.status,'$.state')!='SUCCEEDED')`, application.name, application.generation).Scan(&hookPending); err != nil {
			return err
		}
		if hookPending {
			continue
		}
		if application.status == model.ExecutionBlocked {
			continue
		}
		// A newer application revision can be waiting while the last safe full
		// assignment remains active. Reports for that preserved assignment must
		// not overwrite the controller's PENDING/BLOCKED decision or claim that
		// the new desired generation was observed.
		if desired > 0 && (minimumDesiredGeneration != int64(application.generation) ||
			maximumDesiredGeneration != int64(application.generation)) {
			continue
		}
		var requiredCron, readyCron, requiredServices int
		var desiredRevision string
		if err := tx.QueryRowContext(ctx, `SELECT r.id, (SELECT count(*) FROM json_each(r.canonical_manifest, '$.cron_jobs')),
            COALESCE((SELECT sum(CASE WHEN json_extract(value, '$.autoscale') IS NULL THEN json_extract(value, '$.replicas')
				ELSE min(max(COALESCE((SELECT c.replicas FROM service_scales c WHERE c.application_name = r.application_name
				AND c.service_name = key), 0), json_extract(value, '$.autoscale.min')), json_extract(value, '$.autoscale.max')) END)
				FROM json_each(r.canonical_manifest, '$.services')), 0)
			FROM application_revisions r WHERE r.application_name = ? AND r.generation = ?`, application.name, application.generation).Scan(&desiredRevision, &requiredCron, &requiredServices); err != nil {
			return fmt.Errorf("read desired cron count: %w", err)
		}
		if requiredCron > 0 {
			if err := tx.QueryRowContext(ctx, `SELECT count(DISTINCT d.job_name) FROM desired_cron_jobs d
				JOIN nodes n ON n.id = d.node_id
				JOIN cron_job_observations o ON o.node_id = d.node_id AND o.application_name = d.application_name
				AND o.job_name = d.job_name AND o.spec_digest = d.spec_digest
				WHERE d.application_name = ? AND d.application_generation = ? AND d.revision_id = ?
				AND json_extract(d.spec, '$.run_id') IS NULL
 AND n.acknowledged_generation = n.assignment_generation AND n.acknowledged_generation >= d.assignment_generation`,
				application.name, application.generation, desiredRevision).Scan(&readyCron); err != nil {
				return fmt.Errorf("summarize cron rollout readiness: %w", err)
			}
		}
		execution.ObservedGeneration = application.generation
		switch {
		case desired == 0 && requiredServices == 0 && requiredCron > 0 && readyCron == requiredCron:
			execution.Status = model.ExecutionRunning
			execution.ActiveGeneration = application.generation
			execution.ActiveRevisionID = desiredRevision
			execution.Reason = "cron_schedules_ready"
			execution.Message = "all desired cron schedules are acknowledged; this does not wait for a scheduled run"
		case desired == 0:
			execution.Status = model.ExecutionPending
			execution.Reason = "assignment_empty"
			execution.Message = "no executable allocations are assigned"
		case terminalRollbacks == desired && desired > 0 && serving == desired:
			execution.Status = model.ExecutionRolledBack
			execution.Reason = "health_gate_rollback"
			execution.Message = "the desired generation failed its health gate; the authorized previous generation remains active"
		case rollbackStates > 0 && (running > 0 || activeFallbacks > 0):
			execution.Status = model.ExecutionDegraded
			execution.Reason = "rollback_converging"
			execution.Message = "the desired generation failed its health gate; replicas are converging on the authorized previous generation"
		case requiredServices > 0 && desired != requiredServices:
			execution.Status = model.ExecutionPending
			execution.Reason = "allocations_pending"
			execution.Message = "waiting for all desired service replicas to be assigned"
		case readyCron != requiredCron && failed == 0:
			execution.Status = model.ExecutionPending
			execution.Reason = "cron_assignments_pending"
			execution.Message = "waiting for desired cron schedules to be acknowledged and observed"
		case observed == desired && running == desired && activeCount == desired &&
			activeFallbacks == 0 && endpointAllocations == 0:
			execution.Status = model.ExecutionRunning
			execution.Reason = "all_processes_running"
			execution.Message = "all desired container processes are running"
		case observed == desired && running == desired && serving == desired && activeCount == desired &&
			activeFallbacks == 0 && activeRoutes == requiredRoutes:
			execution.Status = model.ExecutionHealthy
			execution.Reason = "all_health_gates_ready"
			execution.Message = "all desired processes, readiness gates, and requested routes are active"
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
	var oldGeneration, oldActiveGeneration int64
	var oldActiveRevision string
	err := tx.QueryRowContext(ctx, `SELECT status, observed_generation, active_generation, active_revision_id, reason, message
		FROM application_execution WHERE application_name = ?`, name).Scan(&oldStatus, &oldGeneration,
		&oldActiveGeneration, &oldActiveRevision, &oldReason, &oldMessage)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("read application execution state: %w", err)
	}
	changed := errors.Is(err, sql.ErrNoRows) || oldStatus != execution.Status ||
		uint64(oldGeneration) != execution.ObservedGeneration || uint64(oldActiveGeneration) != execution.ActiveGeneration ||
		oldActiveRevision != execution.ActiveRevisionID || oldReason != execution.Reason || oldMessage != execution.Message
	if !changed {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO application_execution
		(application_name, status, observed_generation, active_generation, active_revision_id, reason, message, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(application_name) DO UPDATE SET status = excluded.status,
		observed_generation = excluded.observed_generation, active_generation = excluded.active_generation,
		active_revision_id = excluded.active_revision_id, reason = excluded.reason,
		message = excluded.message, updated_at = excluded.updated_at`, name,
		execution.Status, execution.ObservedGeneration, execution.ActiveGeneration, execution.ActiveRevisionID,
		execution.Reason, execution.Message,
		execution.UpdatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return false, fmt.Errorf("write application execution state: %w", err)
	}
	if err := observeDeploymentTx(ctx, tx, name, execution); err != nil {
		return false, err
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
		kind := model.EventApplicationStatusChanged
		switch execution.Status {
		case model.ExecutionHealthy:
			kind = model.EventApplicationHealthy
		case model.ExecutionRolledBack:
			kind = model.EventApplicationRolledBack
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO events
			(cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
			clusterID, kind, string(payload),
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
		model.ExecutionStopping, model.ExecutionRemoved, model.ExecutionHealthy,
		model.ExecutionRolledBack:
		return true
	default:
		return false
	}
}

func (s *Store) DesiredAllocation(ctx context.Context, application, service string, replica uint32) (model.AllocationDesired, error) {
	allocation, _, err := s.DesiredAllocationNode(ctx, application, service, replica)
	return allocation, err
}

func (s *Store) DesiredAllocationPlacements(ctx context.Context) ([]model.AllocationPlacement, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT spec, node_id FROM desired_allocations
		ORDER BY application_name, service_name, replica`)
	if err != nil {
		return nil, fmt.Errorf("read desired allocation inventory: %w", err)
	}
	type placementRow struct {
		allocation model.AllocationDesired
		nodeID     string
	}
	var pending []placementRow
	for rows.Next() {
		var encoded, nodeID string
		if err := rows.Scan(&encoded, &nodeID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan desired allocation inventory: %w", err)
		}
		var allocation model.AllocationDesired
		if err := json.Unmarshal([]byte(encoded), &allocation); err != nil {
			rows.Close()
			return nil, fmt.Errorf("decode desired allocation: %w", err)
		}
		pending = append(pending, placementRow{allocation: allocation, nodeID: nodeID})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate desired allocation inventory: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]model.AllocationPlacement, 0, len(pending))
	for _, item := range pending {
		node, err := s.NodeByID(ctx, item.nodeID)
		if err != nil {
			return nil, err
		}
		result = append(result, model.AllocationPlacement{Allocation: item.allocation, Node: node})
	}
	return result, nil
}

func (s *Store) DesiredAllocationNode(ctx context.Context, application, service string, replica uint32) (model.AllocationDesired, model.Node, error) {
	var encoded string
	var nodeID string
	err := s.db.QueryRowContext(ctx, `SELECT spec, node_id FROM desired_allocations
		WHERE application_name = ? AND service_name = ? AND replica = ?`,
		application, service, replica).Scan(&encoded, &nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return model.AllocationDesired{}, model.Node{}, ErrAssignmentNotFound
	}
	if err != nil {
		return model.AllocationDesired{}, model.Node{}, fmt.Errorf("read desired allocation: %w", err)
	}
	var allocation model.AllocationDesired
	if err := json.Unmarshal([]byte(encoded), &allocation); err != nil {
		return model.AllocationDesired{}, model.Node{}, fmt.Errorf("decode desired allocation: %w", err)
	}
	node, err := s.NodeByID(ctx, nodeID)
	if err != nil {
		return model.AllocationDesired{}, model.Node{}, err
	}
	return allocation, node, nil
}

func cloneAllocations(input []model.AllocationDesired) []model.AllocationDesired {
	result := append([]model.AllocationDesired(nil), input...)
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func cloneCronJobs(input []model.CronJobDesired) []model.CronJobDesired {
	result := append([]model.CronJobDesired(nil), input...)
	sort.Slice(result, func(i, j int) bool {
		if result[i].Application == result[j].Application {
			return result[i].Job < result[j].Job
		}
		return result[i].Application < result[j].Application
	})
	return result
}

// recordNodePortObservations replaces a node's published-port observations.
// Every observation must match a node port of the allocation's desired or
// rollback contract in the node's current assignment.
func recordNodePortObservations(ctx context.Context, tx *sql.Tx, report model.AgentReport, allocations []model.AllocationDesired) error {
	type contract struct{ application, service string }
	desired := make(map[string]contract)
	authorize := func(allocation model.AllocationDesired, tcp *model.TCPDesired, udp *model.UDPDesired) {
		if tcp != nil && tcp.NodePort != 0 {
			desired[fmt.Sprintf("%s\x00tcp\x00%d\x00%d", allocation.ID, tcp.NodePort, tcp.Port)] = contract{allocation.Application, allocation.Service}
		}
		if udp != nil {
			desired[fmt.Sprintf("%s\x00udp\x00%d\x00%d", allocation.ID, udp.NodePort, udp.Port)] = contract{allocation.Application, allocation.Service}
		}
	}
	for _, allocation := range allocations {
		authorize(allocation, allocation.TCP, allocation.UDP)
		if allocation.Rollback != nil {
			authorize(allocation, allocation.Rollback.TCP, allocation.Rollback.UDP)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM node_port_observations WHERE node_id = ?`, report.NodeID); err != nil {
		return fmt.Errorf("replace node port observations: %w", err)
	}
	previous := ""
	for _, port := range report.NodePorts {
		key := fmt.Sprintf("%s\x00%05d\x00%s", port.Protocol, port.NodePort, port.AllocationID)
		owner, ok := desired[fmt.Sprintf("%s\x00%s\x00%d\x00%d", port.AllocationID, port.Protocol, port.NodePort, port.TargetPort)]
		if !ok || owner.application != port.Application || owner.service != port.Service ||
			(previous != "" && key <= previous) || port.UpdatedAt.IsZero() || !validRouteState(port.State) {
			return fmt.Errorf("node port observation %s/%d is not authorized", port.Protocol, port.NodePort)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO node_port_observations
			(node_id, application_name, service_name, allocation_id, protocol, node_port, target_port, state, reason, message, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, report.NodeID, port.Application, port.Service, port.AllocationID,
			port.Protocol, port.NodePort, port.TargetPort, port.State, port.Reason, port.Message,
			port.UpdatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("record node port observation: %w", err)
		}
		previous = key
	}
	return nil
}
