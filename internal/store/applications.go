package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

const applyApplicationOperation = "application.apply"

var (
	ErrApplicationNotFound = errors.New("application was not found")
	ErrApplicationLimit    = errors.New("cluster application limit of 100 was reached")
	ErrAllocationLimit     = errors.New("cluster desired allocation limit of 200 was reached")
	ErrIdempotencyConflict = errors.New("idempotency key was already used for a different request")
)

// Application returns the current immutable revision for one application.
func (s *Store) Application(ctx context.Context, name string) (model.Application, error) {
	if strings.TrimSpace(name) == "" {
		return model.Application{}, fmt.Errorf("application name is required")
	}
	application, err := scanApplication(s.db.QueryRowContext(ctx, `SELECT
		a.name, a.generation, r.id, r.digest, r.canonical_manifest,
		a.created_at, a.updated_at, a.deleted_at,
		COALESCE(x.status, 'PENDING'), COALESCE(x.observed_generation, 0),
		COALESCE(x.active_generation, 0), COALESCE(x.active_revision_id, ''),
		COALESCE(x.reason, 'agent_unavailable'),
		COALESCE(x.message, 'no agent is enrolled or connected'),
		COALESCE(x.updated_at, a.updated_at)
		FROM applications AS a
		LEFT JOIN application_revisions AS r
		  ON r.application_name = a.name AND r.generation = a.generation
		LEFT JOIN application_execution AS x ON x.application_name = a.name
		WHERE a.name = ?`, name))
	if err != nil {
		return model.Application{}, err
	}
	application.Execution.Allocations, err = s.applicationObservations(ctx, name)
	if err != nil {
		return model.Application{}, err
	}
	application.Execution.Routes, err = s.applicationRoutes(ctx, name)
	if err != nil {
		return model.Application{}, err
	}
	application.Execution.TCPRoutes, err = s.applicationTCPRoutes(ctx, name)
	if err != nil {
		return model.Application{}, err
	}
	application.Execution.NodePorts, err = s.applicationNodePorts(ctx, name)
	if err != nil {
		return model.Application{}, err
	}
	application.Execution.CronJobs, err = s.applicationCronJobs(ctx, name)
	if err != nil {
		return model.Application{}, err
	}
	application.Execution.JobRuns, err = s.CronRuns(ctx, name, "", 20)
	if err != nil {
		return model.Application{}, err
	}
	return application, nil
}

// Applications returns every current application in deterministic name order.
func (s *Store) Applications(ctx context.Context, includeDeleted bool) ([]model.Application, error) {
	query := `SELECT a.name, a.generation, r.id, r.digest, r.canonical_manifest,
		a.created_at, a.updated_at, a.deleted_at,
		COALESCE(x.status, 'PENDING'), COALESCE(x.observed_generation, 0),
		COALESCE(x.active_generation, 0), COALESCE(x.active_revision_id, ''),
		COALESCE(x.reason, 'agent_unavailable'),
		COALESCE(x.message, 'no agent is enrolled or connected'),
		COALESCE(x.updated_at, a.updated_at)
		FROM applications AS a
		LEFT JOIN application_revisions AS r
		  ON r.application_name = a.name AND r.generation = a.generation
		LEFT JOIN application_execution AS x ON x.application_name = a.name`
	if !includeDeleted {
		query += " WHERE a.deleted_at IS NULL"
	}
	query += " ORDER BY a.name"
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read applications: %w", err)
	}
	defer rows.Close()
	var applications []model.Application
	for rows.Next() {
		application, err := scanApplication(rows)
		if err != nil {
			return nil, err
		}
		applications = append(applications, application)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applications: %w", err)
	}
	return applications, nil
}

// ApplyApplication atomically advances desired state, records its event, and
// stores the exact response used for idempotent replay.
func (s *Store) ApplyApplication(ctx context.Context, input model.ApplyApplication) (model.ApplyApplicationResult, error) {
	if err := validateApplicationInput(input); err != nil {
		return model.ApplyApplicationResult{}, err
	}
	requestSum := sha256.Sum256(input.CanonicalManifest)
	requestHash := hex.EncodeToString(requestSum[:])
	if input.Digest != "sha256:"+requestHash {
		return model.ApplyApplicationResult{}, fmt.Errorf("application digest does not match canonical manifest")
	}

	requested := input.RequestedManifest
	if len(requested) == 0 {
		requested = input.CanonicalManifest
	}
	if len(requested) > 1<<20 || !json.Valid(requested) {
		return model.ApplyApplicationResult{}, fmt.Errorf("invalid requested manifest")
	}
	requestedSum := sha256.Sum256(requested)
	requestHash = hex.EncodeToString(requestedSum[:])
	operation := applyApplicationOperation
	if input.RollbackFromRevisionID != "" {
		if input.ExpectedGeneration == 0 {
			return model.ApplyApplicationResult{}, ErrGenerationConflict
		}
		operation = "application.rollback"
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s", input.Name, input.RollbackFromRevisionID, input.ExpectedGeneration, input.Digest)))
		requestHash = hex.EncodeToString(sum[:])
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.ApplyApplicationResult{}, fmt.Errorf("begin application apply: %w", err)
	}
	defer tx.Rollback()

	replayed, found, err := replayApplication(ctx, tx, input.IdempotencyKey, operation, requestHash)
	if err != nil {
		return model.ApplyApplicationResult{}, err
	}
	if found {
		return replayed, nil
	}

	current, err := applicationInTransaction(ctx, tx, input.Name)
	if err != nil && !errors.Is(err, ErrApplicationNotFound) {
		return model.ApplyApplicationResult{}, err
	}
	if input.RollbackFromRevisionID != "" {
		if errors.Is(err, ErrApplicationNotFound) || current.DeletedAt != nil {
			return model.ApplyApplicationResult{}, ErrApplicationNotFound
		}
		if current.Generation != input.ExpectedGeneration {
			return model.ApplyApplicationResult{}, ErrGenerationConflict
		}
		var sourceDigest string
		var sourceGeneration uint64
		if err := tx.QueryRowContext(ctx, `SELECT digest, generation FROM application_revisions WHERE application_name = ? AND id = ?`, input.Name, input.RollbackFromRevisionID).Scan(&sourceDigest, &sourceGeneration); errors.Is(err, sql.ErrNoRows) {
			return model.ApplyApplicationResult{}, ErrRevisionNotFound
		} else if err != nil {
			return model.ApplyApplicationResult{}, err
		}
		if sourceDigest != input.Digest || sourceGeneration >= current.Generation {
			return model.ApplyApplicationResult{}, ErrRollbackSource
		}
	}
	sameIntent := current.Digest == input.Digest && current.DeletedAt == nil && input.RollbackFromRevisionID == ""
	if sameIntent {
		sameIntent, err = requestedManifestMatches(ctx, tx, current.RevisionID, requested)
		if err != nil {
			return model.ApplyApplicationResult{}, fmt.Errorf("read requested manifest: %w", err)
		}
	}
	stamp := input.CreatedAt.UTC()
	stampText := stamp.Format(time.RFC3339Nano)
	result := model.ApplyApplicationResult{
		Name: input.Name, Digest: input.Digest, Changed: true,
		RevisionID: input.RevisionID, ExecutionStatus: model.ExecutionPending,
		CreatedAt: stamp, UpdatedAt: stamp,
	}
	creatingOrReactivating := errors.Is(err, ErrApplicationNotFound) || current.DeletedAt != nil
	if creatingOrReactivating {
		var activeApplications int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM applications WHERE deleted_at IS NULL`).Scan(&activeApplications); err != nil {
			return model.ApplyApplicationResult{}, fmt.Errorf("count active applications: %w", err)
		}
		if activeApplications >= 100 {
			return model.ApplyApplicationResult{}, ErrApplicationLimit
		}
	}

	if errors.Is(err, ErrApplicationNotFound) {
		result.Generation = 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO applications
			(name, generation, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			input.Name, result.Generation, stampText, stampText); err != nil {
			return model.ApplyApplicationResult{}, fmt.Errorf("insert application: %w", err)
		}
	} else if sameIntent {
		result = model.ApplyApplicationResult{
			Name: current.Name, Generation: current.Generation,
			RevisionID: current.RevisionID, Digest: current.Digest,
			Changed: false, ExecutionStatus: model.ExecutionPending,
			CreatedAt: current.CreatedAt, UpdatedAt: current.UpdatedAt,
		}
	} else {
		result.Generation = current.Generation + 1
		result.CreatedAt = current.CreatedAt
	}

	if result.Changed {
		requestedAllocations, err := manifestAllocationCount(input.CanonicalManifest)
		if err != nil {
			return model.ApplyApplicationResult{}, err
		}
		total, err := otherApplicationAllocationCount(ctx, tx, input.Name)
		if err != nil {
			return model.ApplyApplicationResult{}, err
		}
		if requestedAllocations > 200 || total > 200-requestedAllocations {
			return model.ApplyApplicationResult{}, ErrAllocationLimit
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO application_revisions
			(id, application_name, generation, digest, canonical_manifest, created_at, requested_manifest)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, input.RevisionID, input.Name, result.Generation,
			input.Digest, string(input.CanonicalManifest), stampText, string(requested)); err != nil {
			return model.ApplyApplicationResult{}, fmt.Errorf("insert application revision: %w", err)
		}
		if err := startDeploymentTx(ctx, tx, input, current.RevisionID, stamp); err != nil {
			return model.ApplyApplicationResult{}, err
		}
		if current.Name != "" {
			changed, err := tx.ExecContext(ctx, `UPDATE applications
				SET generation = ?, updated_at = ?, deleted_at = NULL WHERE name = ? AND generation = ?`,
				result.Generation, stampText, input.Name, current.Generation)
			if err != nil {
				return model.ApplyApplicationResult{}, fmt.Errorf("advance application: %w", err)
			}
			rows, err := changed.RowsAffected()
			if err != nil {
				return model.ApplyApplicationResult{}, fmt.Errorf("inspect application advance: %w", err)
			}
			if rows != 1 {
				return model.ApplyApplicationResult{}, fmt.Errorf("advance application: concurrent generation change")
			}
		}
		sequence, err := insertApplicationEvent(ctx, tx, result, stampText)
		if err != nil {
			return model.ApplyApplicationResult{}, err
		}
		result.EventSequence = sequence
		if _, err := tx.ExecContext(ctx, `INSERT INTO application_execution
			(application_name, status, observed_generation, reason, message, updated_at)
			VALUES (?, 'PENDING', 0, 'agent_unavailable', 'no agent is enrolled or connected', ?)
			ON CONFLICT(application_name) DO UPDATE SET
			status = excluded.status, observed_generation = excluded.observed_generation,
			reason = excluded.reason, message = excluded.message, updated_at = excluded.updated_at`,
			input.Name, stampText); err != nil {
			return model.ApplyApplicationResult{}, fmt.Errorf("reset application execution state: %w", err)
		}
	}

	// Apply receipts identify the submitted request; revision digests identify resolved intent.
	if operation == applyApplicationOperation {
		result.Digest = "sha256:" + requestHash
	}
	response, err := json.Marshal(result)
	if err != nil {
		return model.ApplyApplicationResult{}, fmt.Errorf("encode idempotency response: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_keys
		(key, operation, request_hash, response, created_at) VALUES (?, ?, ?, ?, ?)`,
		input.IdempotencyKey, operation, requestHash, string(response), stampText); err != nil {
		return model.ApplyApplicationResult{}, fmt.Errorf("record idempotency response: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.ApplyApplicationResult{}, fmt.Errorf("commit application apply: %w", err)
	}
	return result, nil
}

func manifestAllocationCount(canonical []byte) (uint64, error) {
	var document struct {
		Services map[string]struct {
			Replicas uint64 `json:"replicas"`
		} `json:"services"`
	}
	if err := json.Unmarshal(canonical, &document); err != nil {
		return 0, fmt.Errorf("decode canonical manifest allocation count: %w", err)
	}
	var total uint64
	for _, service := range document.Services {
		if service.Replicas > 200 || total > 200-service.Replicas {
			return 201, nil
		}
		total += service.Replicas
	}
	return total, nil
}

func otherApplicationAllocationCount(ctx context.Context, tx *sql.Tx, excluded string) (uint64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT r.canonical_manifest FROM applications a
		JOIN application_revisions r ON r.application_name = a.name AND r.generation = a.generation
		WHERE a.deleted_at IS NULL AND a.name != ?`, excluded)
	if err != nil {
		return 0, fmt.Errorf("read active application allocation counts: %w", err)
	}
	defer rows.Close()
	var total uint64
	for rows.Next() {
		var canonical string
		if err := rows.Scan(&canonical); err != nil {
			return 0, fmt.Errorf("scan active application allocation count: %w", err)
		}
		count, err := manifestAllocationCount([]byte(canonical))
		if err != nil {
			return 0, err
		}
		if count > 200 || total > 200-count {
			return 201, nil
		}
		total += count
	}
	return total, rows.Err()
}

func validateApplicationInput(input model.ApplyApplication) error {
	if strings.TrimSpace(input.Name) == "" || len(input.Name) > 63 {
		return fmt.Errorf("application name is required and must not exceed 63 bytes")
	}
	if strings.TrimSpace(input.RevisionID) == "" || len(input.RevisionID) > 128 {
		return fmt.Errorf("application revision id is required and must not exceed 128 bytes")
	}
	if len(input.IdempotencyKey) < 16 || len(input.IdempotencyKey) > 128 {
		return fmt.Errorf("idempotency key must contain between 16 and 128 bytes")
	}
	if len(input.CanonicalManifest) == 0 || len(input.CanonicalManifest) > 1<<20 || !json.Valid(input.CanonicalManifest) {
		return fmt.Errorf("canonical manifest must be valid JSON no larger than 1 MiB")
	}
	if input.CreatedAt.IsZero() {
		return fmt.Errorf("application creation time is required")
	}
	return nil
}

type applicationQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// ReplayApplication checks accepted requests before any registry access. The write
// transaction repeats this check to arbitrate concurrent requests atomically.
func (s *Store) ReplayApplication(ctx context.Context, key string, canonical []byte) (model.ApplyApplicationResult, bool, error) {
	sum := sha256.Sum256(canonical)
	return replayApplication(ctx, s.db, key, applyApplicationOperation, hex.EncodeToString(sum[:]))
}

func requestedManifestMatches(ctx context.Context, tx *sql.Tx, revision string, requested []byte) (bool, error) {
	var stored string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(requested_manifest, canonical_manifest) FROM application_revisions WHERE id = ?`, revision).Scan(&stored)
	return stored == string(requested), err
}

func replayApplication(ctx context.Context, tx applicationQuerier, key, expectedOperation, requestHash string) (model.ApplyApplicationResult, bool, error) {
	var operation, storedHash, response string
	err := tx.QueryRowContext(ctx, `SELECT operation, request_hash, response
		FROM idempotency_keys WHERE key = ?`, key).Scan(&operation, &storedHash, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ApplyApplicationResult{}, false, nil
	}
	if err != nil {
		return model.ApplyApplicationResult{}, false, fmt.Errorf("read idempotency response: %w", err)
	}
	if operation != expectedOperation || storedHash != requestHash {
		return model.ApplyApplicationResult{}, false, ErrIdempotencyConflict
	}
	var result model.ApplyApplicationResult
	if err := json.Unmarshal([]byte(response), &result); err != nil {
		return model.ApplyApplicationResult{}, false, fmt.Errorf("decode idempotency response: %w", err)
	}
	return result, true, nil
}

func applicationInTransaction(ctx context.Context, tx *sql.Tx, name string) (model.Application, error) {
	return scanApplication(tx.QueryRowContext(ctx, `SELECT
		a.name, a.generation, r.id, r.digest, r.canonical_manifest,
		a.created_at, a.updated_at, a.deleted_at,
		COALESCE(x.status, 'PENDING'), COALESCE(x.observed_generation, 0),
		COALESCE(x.active_generation, 0), COALESCE(x.active_revision_id, ''),
		COALESCE(x.reason, 'agent_unavailable'),
		COALESCE(x.message, 'no agent is enrolled or connected'),
		COALESCE(x.updated_at, a.updated_at)
		FROM applications AS a
		LEFT JOIN application_revisions AS r
		  ON r.application_name = a.name AND r.generation = a.generation
		LEFT JOIN application_execution AS x ON x.application_name = a.name
		WHERE a.name = ?`, name))
}

type applicationScanner interface {
	Scan(...any) error
}

func scanApplication(row applicationScanner) (model.Application, error) {
	var application model.Application
	var generation int64
	var observedGeneration, activeGeneration int64
	var manifest, createdAt, updatedAt, executionUpdatedAt string
	var deletedAt sql.NullString
	err := row.Scan(&application.Name, &generation, &application.RevisionID,
		&application.Digest, &manifest, &createdAt, &updatedAt, &deletedAt,
		&application.Execution.Status, &observedGeneration, &activeGeneration,
		&application.Execution.ActiveRevisionID,
		&application.Execution.Reason, &application.Execution.Message,
		&executionUpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Application{}, ErrApplicationNotFound
	}
	if err != nil {
		return model.Application{}, fmt.Errorf("read application: %w", err)
	}
	if generation < 1 {
		return model.Application{}, fmt.Errorf("read application: invalid generation %d", generation)
	}
	application.Generation = uint64(generation)
	if observedGeneration < 0 || activeGeneration < 0 {
		return model.Application{}, fmt.Errorf("read application: invalid observed generation %d", observedGeneration)
	}
	application.Execution.ObservedGeneration = uint64(observedGeneration)
	application.Execution.ActiveGeneration = uint64(activeGeneration)
	application.CanonicalManifest = json.RawMessage(manifest)
	application.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return model.Application{}, fmt.Errorf("read application creation time: %w", err)
	}
	application.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return model.Application{}, fmt.Errorf("read application update time: %w", err)
	}
	if deletedAt.Valid {
		stamp, parseErr := time.Parse(time.RFC3339Nano, deletedAt.String)
		if parseErr != nil {
			return model.Application{}, fmt.Errorf("read application deletion time: %w", parseErr)
		}
		application.DeletedAt = &stamp
	}
	application.Execution.UpdatedAt, err = time.Parse(time.RFC3339Nano, executionUpdatedAt)
	if err != nil {
		return model.Application{}, fmt.Errorf("read application execution time: %w", err)
	}
	return application, nil
}

func (s *Store) applicationObservations(ctx context.Context, name string) ([]model.AllocationStatus, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT o.id, o.application_name, o.service_name, o.replica,
		o.application_generation, o.spec_digest, o.phase, o.reason, o.message, o.exit_code,
		o.restart_count, o.updated_at, o.health_status, o.address, o.active_spec_digest,
		o.active_generation, o.active_revision_id, o.ready_at, o.declared_port,
		o.node_id, n.name, COALESCE(p.reason, '')
		FROM allocation_observations o JOIN nodes n ON n.id = o.node_id
		LEFT JOIN placement_attempts p ON p.id = o.id
		WHERE o.application_name = ? ORDER BY o.service_name, o.replica, o.id`, name)
	if err != nil {
		return nil, fmt.Errorf("read application allocations: %w", err)
	}
	defer rows.Close()
	var result []model.AllocationStatus
	for rows.Next() {
		var item model.AllocationStatus
		var replica, generation, restartCount, activeGeneration int64
		var exitCode sql.NullInt64
		var readyAt sql.NullString
		var updatedAt string
		if err := rows.Scan(&item.ID, &item.Application, &item.Service, &replica,
			&generation, &item.SpecDigest, &item.Phase, &item.Reason, &item.Message,
			&exitCode, &restartCount, &updatedAt, &item.HealthStatus, &item.Address,
			&item.ActiveSpecDigest, &activeGeneration, &item.ActiveRevisionID, &readyAt, &item.DeclaredPort,
			&item.NodeID, &item.NodeName, &item.PlacementReason); err != nil {
			return nil, fmt.Errorf("scan application allocation: %w", err)
		}
		if replica < 1 || generation < 1 || restartCount < 0 {
			return nil, fmt.Errorf("read application allocation: invalid numeric value")
		}
		item.Replica = uint32(replica)
		item.ApplicationGeneration = uint64(generation)
		item.RestartCount = uint32(restartCount)
		item.ActiveGeneration = uint64(activeGeneration)
		if exitCode.Valid {
			value := int32(exitCode.Int64)
			item.ExitCode = &value
		}
		item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
		if err != nil {
			return nil, fmt.Errorf("read application allocation time: %w", err)
		}
		if readyAt.Valid {
			stamp, parseErr := time.Parse(time.RFC3339Nano, readyAt.String)
			if parseErr != nil {
				return nil, fmt.Errorf("read allocation ready time: %w", parseErr)
			}
			item.ReadyAt = &stamp
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate application allocations: %w", err)
	}
	return result, nil
}

// ApplicationAllocationObservations returns the authenticated allocation
// observations used by distributed rollout admission without loading the
// application's routes, cron history, or other operator-facing detail.
func (s *Store) ApplicationAllocationObservations(ctx context.Context, name string) ([]model.AllocationStatus, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("application name is required")
	}
	return s.applicationObservations(ctx, name)
}

func (s *Store) CronRunNode(ctx context.Context, application, job, runID string) (model.Node, error) {
	var nodeID string
	err := s.db.QueryRowContext(ctx, `SELECT node_id FROM cron_run_observations
		WHERE application_name = ? AND job_name = ? AND id = ?`, application, job, runID).Scan(&nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Node{}, ErrAssignmentNotFound
	}
	if err != nil {
		return model.Node{}, fmt.Errorf("read cron run owner: %w", err)
	}
	return s.NodeByID(ctx, nodeID)
}

func (s *Store) applicationRoutes(ctx context.Context, name string) ([]model.RouteStatus, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.application_name, r.service_name, r.domain, r.https,
		r.state, r.certificate_state, r.upstream_count, r.reason, r.message, r.updated_at,
		r.node_id, n.name FROM route_observations r JOIN nodes n ON n.id = r.node_id
		WHERE r.application_name = ? ORDER BY r.domain, n.name`, name)
	if err != nil {
		return nil, fmt.Errorf("read application routes: %w", err)
	}
	defer rows.Close()
	var result []model.RouteStatus
	for rows.Next() {
		var item model.RouteStatus
		var https, upstreams int64
		var updatedAt string
		if err := rows.Scan(&item.Application, &item.Service, &item.Domain, &https,
			&item.State, &item.CertificateState, &upstreams, &item.Reason, &item.Message,
			&updatedAt, &item.NodeID, &item.NodeName); err != nil {
			return nil, fmt.Errorf("scan application route: %w", err)
		}
		if upstreams < 0 {
			return nil, fmt.Errorf("read application route: invalid upstream count")
		}
		item.HTTPS = https == 1
		item.UpstreamCount = uint32(upstreams)
		item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
		if err != nil {
			return nil, fmt.Errorf("read application route time: %w", err)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) applicationTCPRoutes(ctx context.Context, name string) ([]model.TCPRouteStatus, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.application_name, r.service_name, r.public_port, r.target_port,
		r.state, r.upstream_count, r.reason, r.message, r.updated_at, r.node_id, n.name
		FROM tcp_route_observations r JOIN nodes n ON n.id = r.node_id
		WHERE r.application_name=? ORDER BY r.public_port, n.name`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.TCPRouteStatus
	for rows.Next() {
		var item model.TCPRouteStatus
		var updated string
		if err := rows.Scan(&item.Application, &item.Service, &item.PublicPort, &item.TargetPort, &item.State,
			&item.UpstreamCount, &item.Reason, &item.Message, &updated, &item.NodeID, &item.NodeName); err != nil {
			return nil, err
		}
		item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) applicationNodePorts(ctx context.Context, name string) ([]model.NodePortStatus, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.application_name, p.service_name, p.allocation_id, p.protocol,
		p.node_port, p.target_port, p.state, p.reason, p.message, p.updated_at, p.node_id, n.name
		FROM node_port_observations p JOIN nodes n ON n.id = p.node_id
		WHERE p.application_name=? ORDER BY p.service_name, n.name, p.protocol, p.node_port`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.NodePortStatus
	for rows.Next() {
		var item model.NodePortStatus
		var updated string
		if err := rows.Scan(&item.Application, &item.Service, &item.AllocationID, &item.Protocol, &item.NodePort,
			&item.TargetPort, &item.State, &item.Reason, &item.Message, &updated, &item.NodeID, &item.NodeName); err != nil {
			return nil, err
		}
		item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) applicationCronJobs(ctx context.Context, name string) ([]model.CronJobStatus, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT application_name, job_name, spec_digest, next_run_at,
		active_run_id, last_run_id, last_state, reason, message, updated_at
		FROM cron_job_observations WHERE application_name=? ORDER BY job_name`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.CronJobStatus
	for rows.Next() {
		var item model.CronJobStatus
		var next sql.NullString
		var updated string
		if err := rows.Scan(&item.Application, &item.Job, &item.SpecDigest, &next, &item.ActiveRunID,
			&item.LastRunID, &item.LastState, &item.Reason, &item.Message, &updated); err != nil {
			return nil, err
		}
		item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, err
		}
		if next.Valid {
			value, err := time.Parse(time.RFC3339Nano, next.String)
			if err != nil {
				return nil, err
			}
			item.NextRunAt = &value
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) CronRuns(ctx context.Context, application, job string, limit uint32) ([]model.JobRunStatus, error) {
	if limit == 0 || limit > 100 {
		limit = 20
	}
	query := `SELECT id, application_name, job_name, spec_digest, scheduled_at, state, exit_code,
		started_at, finished_at, reason, message, updated_at FROM cron_run_observations WHERE application_name=?`
	args := []any{application}
	if job != "" {
		query += ` AND job_name=?`
		args = append(args, job)
	}
	query += ` ORDER BY scheduled_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.JobRunStatus
	for rows.Next() {
		var item model.JobRunStatus
		var scheduled, updated string
		var exit sql.NullInt64
		var started, finished sql.NullString
		if err := rows.Scan(&item.ID, &item.Application, &item.Job, &item.SpecDigest, &scheduled, &item.State,
			&exit, &started, &finished, &item.Reason, &item.Message, &updated); err != nil {
			return nil, err
		}
		item.ScheduledAt, err = time.Parse(time.RFC3339Nano, scheduled)
		if err != nil {
			return nil, err
		}
		item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, err
		}
		if exit.Valid {
			value := int32(exit.Int64)
			item.ExitCode = &value
		}
		if started.Valid {
			value, err := time.Parse(time.RFC3339Nano, started.String)
			if err != nil {
				return nil, err
			}
			item.StartedAt = &value
		}
		if finished.Valid {
			value, err := time.Parse(time.RFC3339Nano, finished.String)
			if err != nil {
				return nil, err
			}
			item.FinishedAt = &value
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func insertApplicationEvent(ctx context.Context, tx *sql.Tx, result model.ApplyApplicationResult, stamp string) (uint64, error) {
	payload, err := json.Marshal(struct {
		Name       string `json:"name"`
		Generation uint64 `json:"generation"`
		RevisionID string `json:"revision_id"`
		Digest     string `json:"digest"`
		Status     string `json:"status"`
	}{
		Name: result.Name, Generation: result.Generation, RevisionID: result.RevisionID,
		Digest: result.Digest, Status: result.ExecutionStatus,
	})
	if err != nil {
		return 0, fmt.Errorf("encode application event: %w", err)
	}
	var clusterID string
	if err := tx.QueryRowContext(ctx, "SELECT id FROM clusters WHERE singleton = 1").Scan(&clusterID); err != nil {
		return 0, fmt.Errorf("read cluster for application event: %w", err)
	}
	inserted, err := tx.ExecContext(ctx, `INSERT INTO events
		(cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
		clusterID, model.EventApplicationDesiredUpdated, string(payload), stamp)
	if err != nil {
		return 0, fmt.Errorf("record application event: %w", err)
	}
	sequence, err := inserted.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read application event sequence: %w", err)
	}
	if sequence < 1 {
		return 0, fmt.Errorf("read application event sequence: invalid sequence %d", sequence)
	}
	return uint64(sequence), nil
}
