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

const removeApplicationOperation = "application.remove"

// RemoveApplication tombstones desired state while retaining immutable
// revisions. Reapplying the application later advances from its last
// generation.
func (s *Store) RemoveApplication(ctx context.Context, name, idempotencyKey string, now time.Time) (model.RemoveApplicationResult, error) {
	if strings.TrimSpace(name) == "" || len(name) > 63 {
		return model.RemoveApplicationResult{}, fmt.Errorf("application name is required and must not exceed 63 bytes")
	}
	if len(idempotencyKey) < 16 || len(idempotencyKey) > 128 {
		return model.RemoveApplicationResult{}, fmt.Errorf("idempotency key must contain between 16 and 128 bytes")
	}
	if now.IsZero() {
		return model.RemoveApplicationResult{}, fmt.Errorf("removal time is required")
	}
	requestSum := sha256.Sum256([]byte(name))
	requestHash := hex.EncodeToString(requestSum[:])
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.RemoveApplicationResult{}, fmt.Errorf("begin application removal: %w", err)
	}
	defer tx.Rollback()

	var operation, storedHash, response string
	err = tx.QueryRowContext(ctx, `SELECT operation, request_hash, response
		FROM idempotency_keys WHERE key = ?`, idempotencyKey).Scan(&operation, &storedHash, &response)
	if err == nil {
		if operation != removeApplicationOperation || storedHash != requestHash {
			return model.RemoveApplicationResult{}, ErrIdempotencyConflict
		}
		var result model.RemoveApplicationResult
		if err := json.Unmarshal([]byte(response), &result); err != nil {
			return model.RemoveApplicationResult{}, fmt.Errorf("decode removal replay: %w", err)
		}
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.RemoveApplicationResult{}, fmt.Errorf("read removal replay: %w", err)
	}

	application, err := applicationInTransaction(ctx, tx, name)
	if err != nil {
		return model.RemoveApplicationResult{}, err
	}
	stamp := now.UTC()
	result := model.RemoveApplicationResult{
		Name: name, Generation: application.Generation, UpdatedAt: stamp,
		ExecutionStatus: model.ExecutionStopping,
	}
	if application.DeletedAt == nil {
		result.Changed = true
		changed, err := tx.ExecContext(ctx, `UPDATE applications
			SET deleted_at = ?, updated_at = ? WHERE name = ? AND deleted_at IS NULL`,
			stamp.Format(time.RFC3339Nano), stamp.Format(time.RFC3339Nano), name)
		if err != nil {
			return model.RemoveApplicationResult{}, fmt.Errorf("tombstone application: %w", err)
		}
		rows, err := changed.RowsAffected()
		if err != nil || rows != 1 {
			return model.RemoveApplicationResult{}, fmt.Errorf("tombstone application: concurrent state change")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO application_execution
			(application_name, status, observed_generation, reason, message, updated_at)
			VALUES (?, 'STOPPING', 0, 'removal_pending', 'waiting for the agent to remove runtime objects', ?)
			ON CONFLICT(application_name) DO UPDATE SET
			status = excluded.status, observed_generation = excluded.observed_generation,
			reason = excluded.reason, message = excluded.message, updated_at = excluded.updated_at`,
			name, stamp.Format(time.RFC3339Nano)); err != nil {
			return model.RemoveApplicationResult{}, fmt.Errorf("record removal execution state: %w", err)
		}
		if err := finishDeploymentTx(ctx, tx, application.RevisionID, "REMOVED", stamp); err != nil {
			return model.RemoveApplicationResult{}, err
		}
		payload, _ := json.Marshal(struct {
			Name       string `json:"name"`
			Generation uint64 `json:"generation"`
		}{name, application.Generation})
		var clusterID string
		if err := tx.QueryRowContext(ctx, "SELECT id FROM clusters WHERE singleton = 1").Scan(&clusterID); err != nil {
			return model.RemoveApplicationResult{}, fmt.Errorf("read cluster for removal event: %w", err)
		}
		inserted, err := tx.ExecContext(ctx, `INSERT INTO events
			(cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
			clusterID, model.EventApplicationDesiredRemoved, string(payload), stamp.Format(time.RFC3339Nano))
		if err != nil {
			return model.RemoveApplicationResult{}, fmt.Errorf("record application removal: %w", err)
		}
		sequence, err := inserted.LastInsertId()
		if err != nil || sequence < 1 {
			return model.RemoveApplicationResult{}, fmt.Errorf("read application removal sequence")
		}
		result.EventSequence = uint64(sequence)
	} else {
		result.ExecutionStatus = application.Execution.Status
		result.UpdatedAt = application.UpdatedAt
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return model.RemoveApplicationResult{}, fmt.Errorf("encode removal response: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_keys
		(key, operation, request_hash, response, created_at) VALUES (?, ?, ?, ?, ?)`,
		idempotencyKey, removeApplicationOperation, requestHash, string(encoded), stamp.Format(time.RFC3339Nano)); err != nil {
		return model.RemoveApplicationResult{}, fmt.Errorf("record removal response: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.RemoveApplicationResult{}, fmt.Errorf("commit application removal: %w", err)
	}
	return result, nil
}
