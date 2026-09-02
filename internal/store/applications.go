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
	return scanApplication(s.db.QueryRowContext(ctx, `SELECT
		a.name, a.generation, r.id, r.digest, r.canonical_manifest,
		a.created_at, a.updated_at
		FROM applications AS a
		LEFT JOIN application_revisions AS r
		  ON r.application_name = a.name AND r.generation = a.generation
		WHERE a.name = ?`, name))
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
		RevisionID: input.RevisionID, CreatedAt: stamp, UpdatedAt: stamp,
	}

	if errors.Is(err, ErrApplicationNotFound) {
		result.Generation = 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO applications
			(name, generation, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			input.Name, result.Generation, stampText, stampText); err != nil {
			return model.ApplyApplicationResult{}, fmt.Errorf("insert application: %w", err)
		}
	} else if current.Digest == input.Digest {
		result = model.ApplyApplicationResult{
			Name: current.Name, Generation: current.Generation,
			RevisionID: current.RevisionID, Digest: current.Digest,
			Changed: false, CreatedAt: current.CreatedAt, UpdatedAt: current.UpdatedAt,
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
				SET generation = ?, updated_at = ? WHERE name = ? AND generation = ?`,
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
		a.created_at, a.updated_at
		FROM applications AS a
		LEFT JOIN application_revisions AS r
		  ON r.application_name = a.name AND r.generation = a.generation
		WHERE a.name = ?`, name))
}

type applicationScanner interface {
	Scan(...any) error
}

func scanApplication(row applicationScanner) (model.Application, error) {
	var application model.Application
	var generation int64
	var manifest, createdAt, updatedAt string
	err := row.Scan(&application.Name, &generation, &application.RevisionID,
		&application.Digest, &manifest, &createdAt, &updatedAt)
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
	application.CanonicalManifest = json.RawMessage(manifest)
	application.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return model.Application{}, fmt.Errorf("read application creation time: %w", err)
	}
	application.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return model.Application{}, fmt.Errorf("read application update time: %w", err)
	}
	return application, nil
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
		Digest: result.Digest, Status: model.ExecutionPending,
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
