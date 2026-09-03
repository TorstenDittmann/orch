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

	"change.codes/torsten/better-swarm/internal/model"
)

const applyApplicationOperation = "application.apply"

var (
	ErrApplicationNotFound = errors.New("application was not found")
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.ApplyApplicationResult{}, fmt.Errorf("begin application apply: %w", err)
	}
	defer tx.Rollback()

	replayed, found, err := replayApplication(ctx, tx, input.IdempotencyKey, requestHash)
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
	stamp := input.CreatedAt.UTC()
	stampText := stamp.Format(time.RFC3339Nano)
	result := model.ApplyApplicationResult{
		Name: input.Name, Digest: input.Digest, Changed: true,
		RevisionID: input.RevisionID, ExecutionStatus: model.ExecutionPending,
		CreatedAt: stamp, UpdatedAt: stamp,
	}

	if errors.Is(err, ErrApplicationNotFound) {
		result.Generation = 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO applications
			(name, generation, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			input.Name, result.Generation, stampText, stampText); err != nil {
			return model.ApplyApplicationResult{}, fmt.Errorf("insert application: %w", err)
		}
	} else if current.Digest == input.Digest && current.DeletedAt == nil {
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO application_revisions
			(id, application_name, generation, digest, canonical_manifest, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, input.RevisionID, input.Name, result.Generation,
			input.Digest, string(input.CanonicalManifest), stampText); err != nil {
			return model.ApplyApplicationResult{}, fmt.Errorf("insert application revision: %w", err)
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

	response, err := json.Marshal(result)
	if err != nil {
		return model.ApplyApplicationResult{}, fmt.Errorf("encode idempotency response: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_keys
		(key, operation, request_hash, response, created_at) VALUES (?, ?, ?, ?, ?)`,
		input.IdempotencyKey, applyApplicationOperation, requestHash, string(response), stampText); err != nil {
		return model.ApplyApplicationResult{}, fmt.Errorf("record idempotency response: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.ApplyApplicationResult{}, fmt.Errorf("commit application apply: %w", err)
	}
	return result, nil
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

func replayApplication(ctx context.Context, tx *sql.Tx, key, requestHash string) (model.ApplyApplicationResult, bool, error) {
	var operation, storedHash, response string
	err := tx.QueryRowContext(ctx, `SELECT operation, request_hash, response
		FROM idempotency_keys WHERE key = ?`, key).Scan(&operation, &storedHash, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ApplyApplicationResult{}, false, nil
	}
	if err != nil {
		return model.ApplyApplicationResult{}, false, fmt.Errorf("read idempotency response: %w", err)
	}
	if operation != applyApplicationOperation || storedHash != requestHash {
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, application_name, service_name, replica,
		application_generation, spec_digest, phase, reason, message, exit_code,
		restart_count, updated_at, health_status, address, active_spec_digest,
		active_generation, active_revision_id, ready_at, declared_port
		FROM allocation_observations WHERE application_name = ?
		ORDER BY service_name, replica, id`, name)
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
			&item.ActiveSpecDigest, &activeGeneration, &item.ActiveRevisionID, &readyAt, &item.DeclaredPort); err != nil {
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

func (s *Store) applicationRoutes(ctx context.Context, name string) ([]model.RouteStatus, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT application_name, service_name, domain, https,
		state, certificate_state, upstream_count, reason, message, updated_at
		FROM route_observations WHERE application_name = ? ORDER BY domain`, name)
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
			&updatedAt); err != nil {
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
