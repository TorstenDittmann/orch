package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

var (
	ErrRevisionNotFound   = errors.New("application revision was not found")
	ErrGenerationConflict = errors.New("application changed; refresh its current generation before rolling back")
	ErrRollbackSource     = errors.New("rollback source must be an older revision of this application")
)

// Deployment retains immutable intent and the last observation before supersession.
// Outcome records the first terminal rollout result, separately from later health.
type Deployment struct {
	Application            string
	RevisionID             string
	Generation             uint64
	Digest                 string
	CanonicalManifest      json.RawMessage
	RequestedManifest      json.RawMessage
	CreatedAt              time.Time
	RollbackFromRevisionID string
	Outcome                string
	Status                 string
	Reason                 string
	Message                string
	UpdatedAt              time.Time
	FinishedAt             *time.Time
	Current                bool
	Active                 bool
}

const deploymentSelect = `SELECT r.application_name, r.id, r.generation, r.digest,
 r.canonical_manifest, COALESCE(r.requested_manifest, r.canonical_manifest), r.created_at, COALESCE(h.rollback_from_revision_id, ''),
 h.outcome, h.status, h.reason, h.message, h.updated_at, h.finished_at,
 (r.generation = a.generation AND a.deleted_at IS NULL),
 (r.id = COALESCE(x.active_revision_id, '') AND a.deleted_at IS NULL)
 FROM application_revisions r JOIN deployment_records h ON h.revision_id = r.id
 JOIN applications a ON a.name = r.application_name
 LEFT JOIN application_execution x ON x.application_name = a.name`

func (s *Store) Deployment(ctx context.Context, name, revision string) (Deployment, error) {
	return scanDeployment(s.db.QueryRowContext(ctx, deploymentSelect+` WHERE r.application_name = ? AND r.id = ?`, name, revision))
}

// Deployments uses an exclusive generation cursor, stable while new revisions arrive.
func (s *Store) Deployments(ctx context.Context, name string, before uint64, limit int) ([]Deployment, uint64, error) {
	if limit < 1 || limit > 50 || before > math.MaxInt64 {
		return nil, 0, fmt.Errorf("invalid history bounds")
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM applications WHERE name = ?)`, name).Scan(&exists); err != nil {
		return nil, 0, err
	}
	if !exists {
		return nil, 0, ErrApplicationNotFound
	}
	rows, err := s.db.QueryContext(ctx, deploymentSelect+` WHERE r.application_name = ? AND (? = 0 OR r.generation < ?) ORDER BY r.generation DESC LIMIT ?`, name, before, before, limit+1)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := make([]Deployment, 0, limit)
	for rows.Next() {
		item, err := scanDeployment(rows)
		if err != nil {
			return nil, 0, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var next uint64
	if len(result) > limit {
		result = result[:limit]
		next = result[len(result)-1].Generation
	}
	return result, next, nil
}

func scanDeployment(row applicationScanner) (Deployment, error) {
	var d Deployment
	var canonical, requested, created, updated string
	var finished sql.NullString
	err := row.Scan(&d.Application, &d.RevisionID, &d.Generation, &d.Digest, &canonical, &requested, &created,
		&d.RollbackFromRevisionID, &d.Outcome, &d.Status, &d.Reason, &d.Message, &updated, &finished, &d.Current, &d.Active)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrRevisionNotFound
	}
	if err != nil {
		return d, err
	}
	d.CanonicalManifest = json.RawMessage(canonical)
	d.RequestedManifest = json.RawMessage(requested)
	if d.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return d, err
	}
	if d.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return d, err
	}
	if finished.Valid {
		stamp, err := time.Parse(time.RFC3339Nano, finished.String)
		if err != nil {
			return d, err
		}
		d.FinishedAt = &stamp
	}
	return d, nil
}

func startDeploymentTx(ctx context.Context, tx *sql.Tx, input model.ApplyApplication, previous string, now time.Time) error {
	if previous != "" {
		if err := finishDeploymentTx(ctx, tx, previous, "SUPERSEDED", now); err != nil {
			return err
		}
	}
	var source any
	if input.RollbackFromRevisionID != "" {
		source = input.RollbackFromRevisionID
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO deployment_records
 (revision_id, rollback_from_revision_id, outcome, status, reason, message, updated_at)
 VALUES (?, ?, 'IN_PROGRESS', 'PENDING', 'accepted', 'desired state accepted', ?)`, input.RevisionID, source, now.UTC().Format(time.RFC3339Nano))
	return err
}

func finishDeploymentTx(ctx context.Context, tx *sql.Tx, revision, outcome string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE deployment_records SET outcome = ?, finished_at = ?
 WHERE revision_id = ? AND outcome = 'IN_PROGRESS'`, outcome, now.UTC().Format(time.RFC3339Nano), revision)
	return err
}

func observeDeploymentTx(ctx context.Context, tx *sql.Tx, name string, execution model.ApplicationExecution) error {
	var revision string
	var generation uint64
	var deleted sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT r.id, a.generation, a.deleted_at FROM applications a
 JOIN application_revisions r ON r.application_name = a.name AND r.generation = a.generation
 WHERE a.name = ?`, name).Scan(&revision, &generation, &deleted); err != nil {
		return err
	}
	if deleted.Valid || (execution.ObservedGeneration != 0 && execution.ObservedGeneration != generation) {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE deployment_records SET status = ?, reason = ?, message = ?, updated_at = ? WHERE revision_id = ?`,
		execution.Status, execution.Reason, execution.Message, execution.UpdatedAt.UTC().Format(time.RFC3339Nano), revision)
	if err != nil {
		return err
	}
	outcome := ""
	if execution.ObservedGeneration == generation {
		switch execution.Status {
		case model.ExecutionHealthy, model.ExecutionRunning:
			if execution.ActiveGeneration == generation && execution.ActiveRevisionID == revision {
				outcome = "SUCCEEDED"
			}
		case model.ExecutionFailed, model.ExecutionRolledBack:
			outcome = execution.Status
		}
	}
	if outcome != "" {
		return finishDeploymentTx(ctx, tx, revision, outcome, execution.UpdatedAt)
	}
	return nil
}
