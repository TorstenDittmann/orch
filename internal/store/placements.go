package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// EnsurePlacementAttempt returns the durable runtime identity for a logical
// replica on nodeID. Retaining a node retains the identity; every transfer,
// including a later transfer back to an earlier node, creates a new identity.
func (s *Store) EnsurePlacementAttempt(ctx context.Context, logicalID, nodeID, application, service string,
	replica uint32, preferredID, reason string, now time.Time,
) (string, error) {
	if logicalID == "" || nodeID == "" || application == "" || service == "" || replica == 0 || now.IsZero() {
		return "", fmt.Errorf("placement attempt identity is incomplete")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin placement attempt: %w", err)
	}
	defer tx.Rollback()
	var currentID, currentNode string
	err = tx.QueryRowContext(ctx, `SELECT id, node_id FROM placement_attempts
		WHERE logical_id = ? AND state = 'DESIRED' ORDER BY created_at DESC, id DESC LIMIT 1`, logicalID).Scan(&currentID, &currentNode)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read placement attempt: %w", err)
	}
	if err == nil && currentNode == nodeID {
		return currentID, nil
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	if err == nil {
		if _, err := tx.ExecContext(ctx, `UPDATE placement_attempts SET state = 'SUPERSEDED', updated_at = ?
			WHERE logical_id = ? AND state = 'DESIRED'`, stamp, logicalID); err != nil {
			return "", fmt.Errorf("supersede placement attempt: %w", err)
		}
	}
	var attempts uint64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM placement_attempts WHERE logical_id = ?`, logicalID).Scan(&attempts); err != nil {
		return "", fmt.Errorf("count placement attempts: %w", err)
	}
	id := preferredID
	if id == "" || attempts != 0 {
		digest := sha256.Sum256([]byte(logicalID + "\x00" + nodeID + "\x00" + fmt.Sprint(attempts+1)))
		id = "alloc_" + hex.EncodeToString(digest[:16])
	}
	if reason == "" {
		reason = "scheduled"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO placement_attempts
		(id, logical_id, node_id, application_name, service_name, replica, state, reason, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'DESIRED', ?, ?, ?)`, id, logicalID, nodeID, application, service, replica, reason, stamp, stamp); err != nil {
		return "", fmt.Errorf("create placement attempt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit placement attempt: %w", err)
	}
	return id, nil
}

// RetireInactivePlacementAttempts closes identities only after their complete
// replacement assignment set has been published successfully.
func (s *Store) RetireInactivePlacementAttempts(ctx context.Context, activeIDs []string, now time.Time) error {
	if now.IsZero() {
		return fmt.Errorf("placement retirement time is required")
	}
	active := make(map[string]struct{}, len(activeIDs))
	for _, id := range activeIDs {
		active[id] = struct{}{}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM placement_attempts WHERE state = 'DESIRED'`)
	if err != nil {
		return fmt.Errorf("read desired placement attempts: %w", err)
	}
	var retired []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan desired placement attempt: %w", err)
		}
		if _, ok := active[id]; !ok {
			retired = append(retired, id)
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close desired placement attempts: %w", err)
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	for _, id := range retired {
		if _, err := s.db.ExecContext(ctx, `UPDATE placement_attempts SET state = 'REMOVED', updated_at = ?
			WHERE id = ? AND state = 'DESIRED'`, stamp, id); err != nil {
			return fmt.Errorf("retire placement attempt: %w", err)
		}
	}
	return nil
}

// CronOwnership is the last authenticated scheduler observation from a node.
// A row deliberately survives assignment publication until that node reports
// against the replacement assignment, making it a durable handoff fence.
type CronOwnership struct {
	NodeID      string
	Application string
	Job         string
	SpecDigest  string
	ActiveRunID string
}

func (s *Store) CronOwnerships(ctx context.Context) ([]CronOwnership, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, application_name, job_name, spec_digest, active_run_id
		FROM cron_job_observations ORDER BY application_name, job_name, node_id`)
	if err != nil {
		return nil, fmt.Errorf("read cron ownership observations: %w", err)
	}
	defer rows.Close()
	var result []CronOwnership
	for rows.Next() {
		var item CronOwnership
		if err := rows.Scan(&item.NodeID, &item.Application, &item.Job, &item.SpecDigest, &item.ActiveRunID); err != nil {
			return nil, fmt.Errorf("scan cron ownership observation: %w", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cron ownership observations: %w", err)
	}
	return result, nil
}

func (s *Store) VolumeClaim(ctx context.Context, application, volume string) (string, error) {
	var nodeID string
	err := s.db.QueryRowContext(ctx, `SELECT node_id FROM volume_claims WHERE application_name = ? AND volume_name = ?`, application, volume).Scan(&nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read volume claim: %w", err)
	}
	return nodeID, nil
}

func (s *Store) ClaimVolume(ctx context.Context, application, volume, nodeID string, now time.Time) error {
	if application == "" || volume == "" || nodeID == "" || now.IsZero() {
		return fmt.Errorf("application, volume, node, and claim time are required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO volume_claims(application_name, volume_name, node_id, created_at)
		VALUES (?, ?, ?, ?) ON CONFLICT(application_name, volume_name) DO NOTHING`, application, volume, nodeID, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("claim local volume: %w", err)
	}
	claimed, err := s.VolumeClaim(ctx, application, volume)
	if err != nil {
		return err
	}
	if claimed != nodeID {
		return fmt.Errorf("local volume is already claimed by another node")
	}
	return nil
}
