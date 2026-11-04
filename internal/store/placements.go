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

// VolumeClaimedAt returns when a volume was claimed on its current node, or
// zero when it is not claimed.
func (s *Store) VolumeClaimedAt(ctx context.Context, application, volume string) (time.Time, error) {
	var createdAt string
	err := s.db.QueryRowContext(ctx, `SELECT created_at FROM volume_claims WHERE application_name = ? AND volume_name = ?`, application, volume).Scan(&createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("read volume claim: %w", err)
	}
	return time.Parse(time.RFC3339Nano, createdAt)
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

// VolumeMove is a service whose local volumes are being moved to another
// node. The reconciler keeps the service off every node meanwhile.
type VolumeMove struct {
	Application string
	Service     string
	ToNodeID    string
	CreatedAt   time.Time
}

// ErrVolumeMoveInProgress reports that a service's volumes are already
// being moved.
var ErrVolumeMoveInProgress = errors.New("the service's volumes are already being moved")

// BeginVolumeMove holds a service off every node until FinishVolumeMove or
// CancelVolumeMove.
func (s *Store) BeginVolumeMove(ctx context.Context, application, service, toNodeID string, now time.Time) error {
	if application == "" || service == "" || toNodeID == "" || now.IsZero() {
		return fmt.Errorf("application, service, node, and time are required")
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO volume_moves(application_name, service_name, to_node_id, created_at)
		VALUES (?, ?, ?, ?) ON CONFLICT(application_name, service_name) DO NOTHING`,
		application, service, toNodeID, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("begin volume move: %w", err)
	}
	if count, err := result.RowsAffected(); err == nil && count == 0 {
		return ErrVolumeMoveInProgress
	}
	return nil
}

// VolumeMoves returns every service being moved, keyed by application and
// service name joined with a slash.
func (s *Store) VolumeMoves(ctx context.Context) (map[string]VolumeMove, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT application_name, service_name, to_node_id, created_at FROM volume_moves`)
	if err != nil {
		return nil, fmt.Errorf("read volume moves: %w", err)
	}
	defer rows.Close()
	result := make(map[string]VolumeMove)
	for rows.Next() {
		var item VolumeMove
		var createdAt string
		if err := rows.Scan(&item.Application, &item.Service, &item.ToNodeID, &createdAt); err != nil {
			return nil, fmt.Errorf("scan volume move: %w", err)
		}
		if item.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
			return nil, fmt.Errorf("parse volume move time: %w", err)
		}
		result[item.Application+"/"+item.Service] = item
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate volume moves: %w", err)
	}
	return result, nil
}

// FinishVolumeMove claims the given volumes on the move's node and releases
// the service, in one transaction, so it starts there next. A claim keeps
// its time: the data moved with it, so backups from the old node are not
// those of a lost node.
func (s *Store) FinishVolumeMove(ctx context.Context, application, service string, volumes []string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin volume move commit: %w", err)
	}
	defer tx.Rollback()
	var toNodeID string
	if err := tx.QueryRowContext(ctx, `SELECT to_node_id FROM volume_moves WHERE application_name = ? AND service_name = ?`,
		application, service).Scan(&toNodeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("the volume move was cancelled")
		}
		return fmt.Errorf("read volume move: %w", err)
	}
	for _, volume := range volumes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO volume_claims(application_name, volume_name, node_id, created_at)
			VALUES (?, ?, ?, ?) ON CONFLICT(application_name, volume_name) DO UPDATE SET node_id = excluded.node_id`,
			application, volume, toNodeID, now.UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("move volume claim: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM volume_moves WHERE application_name = ? AND service_name = ?`, application, service); err != nil {
		return fmt.Errorf("finish volume move: %w", err)
	}
	return tx.Commit()
}

// CancelVolumeMove releases a service without moving its volumes, so it
// starts on its old node again.
func (s *Store) CancelVolumeMove(ctx context.Context, application, service string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM volume_moves WHERE application_name = ? AND service_name = ?`, application, service); err != nil {
		return fmt.Errorf("cancel volume move: %w", err)
	}
	return nil
}

// CancelVolumeMoves releases every held service. A move runs inside one
// controller request, so after a controller restart none is still running.
func (s *Store) CancelVolumeMoves(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM volume_moves`); err != nil {
		return fmt.Errorf("cancel volume moves: %w", err)
	}
	return nil
}
