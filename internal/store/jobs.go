package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

var ErrJobNotFound = errors.New("job was not found")
var ErrJobLimit = errors.New("active job limit or node resource capacity reached")
var ErrJobActive = errors.New("job has not finished; cancel it and wait for acknowledgement before retrying")

const jobSelect = `SELECT id,node_id,service_name,kind,retry_of,template,status,cancel_requested FROM one_off_jobs`

func scanJob(row applicationScanner) (model.Job, error) {
	var job model.Job
	var template, status string
	err := row.Scan(&job.ID, &job.NodeID, &job.Service, &job.Kind, &job.RetryOf, &template, &status, &job.CancelRequested)
	if errors.Is(err, sql.ErrNoRows) {
		return job, ErrJobNotFound
	}
	if err != nil {
		return job, err
	}
	if err := json.Unmarshal([]byte(template), &job.Template); err != nil {
		return job, err
	}
	if err := json.Unmarshal([]byte(status), &job.Status); err != nil {
		return job, err
	}
	job.Template.CancelRequested = job.CancelRequested
	return job, nil
}
func (s *Store) Job(ctx context.Context, id string) (model.Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, jobSelect+` WHERE id=?`, id))
}
func (s *Store) Jobs(ctx context.Context, application, before string, limit int, activeOnly bool) ([]model.Job, error) {
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("job limit must be 1-100")
	}
	query := jobSelect + ` WHERE (?='' OR application_name=?)`
	args := []any{application, application}
	if activeOnly {
		query += ` AND json_extract(status,'$.finished_at') IS NULL`
	}
	if before != "" {
		query += ` AND rowid < (SELECT rowid FROM one_off_jobs WHERE id=?)`
		args = append(args, before)
	}
	query += ` ORDER BY rowid DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.Job{}
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, job)
	}
	return result, rows.Err()
}
func (s *Store) HookJob(ctx context.Context, application, revision string) (model.Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, jobSelect+` WHERE application_name=? AND revision_id=? AND kind='pre_deploy' ORDER BY rowid DESC LIMIT 1`, application, revision))
}
func (s *Store) ReplayJob(ctx context.Context, key, hash string) (model.Job, bool, error) {
	id, found, err := replayJob(ctx, s.db, key, hash)
	if err != nil || !found {
		return model.Job{}, found, err
	}
	job, err := s.Job(ctx, id)
	return job, true, err
}
func replayJob(ctx context.Context, q applicationQuerier, key, hash string) (string, bool, error) {
	var operation, stored, response string
	err := q.QueryRowContext(ctx, `SELECT operation,request_hash,response FROM idempotency_keys WHERE key=?`, key).Scan(&operation, &stored, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if operation != "job.run" || stored != hash {
		return "", false, ErrIdempotencyConflict
	}
	var id string
	err = json.Unmarshal([]byte(response), &id)
	return id, true, err
}
func (s *Store) CreateJob(ctx context.Context, job model.Job, key, hash string, capacityCPU, capacityMemory uint64) (model.Job, error) {
	if job.ID == "" || job.Template.RunID != job.ID || job.Template.RunAt == nil || key == "" || hash == "" {
		return model.Job{}, fmt.Errorf("incomplete job intent")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Job{}, err
	}
	defer tx.Rollback()
	if id, found, err := replayJob(ctx, tx, key, hash); err != nil {
		return model.Job{}, err
	} else if found {
		return scanJob(tx.QueryRowContext(ctx, jobSelect+` WHERE id=?`, id))
	}
	var revision string
	if err := tx.QueryRowContext(ctx, `SELECT r.id FROM applications a JOIN application_revisions r ON r.application_name=a.name AND r.generation=a.generation WHERE a.name=? AND a.deleted_at IS NULL`, job.Template.Application).Scan(&revision); err != nil {
		return model.Job{}, ErrApplicationNotFound
	}
	if revision != job.Template.RevisionID {
		return model.Job{}, ErrGenerationConflict
	}
	if job.Kind == "pre_deploy" && job.RetryOf != "" {
		latest, err := scanJob(tx.QueryRowContext(ctx, jobSelect+` WHERE application_name=? AND revision_id=? AND kind='pre_deploy' ORDER BY rowid DESC LIMIT 1`, job.Template.Application, revision))
		if err != nil {
			return model.Job{}, err
		}
		if latest.ID != job.RetryOf {
			return model.Job{}, ErrGenerationConflict
		}
	}
	if job.RetryOf != "" {
		source, err := scanJob(tx.QueryRowContext(ctx, jobSelect+` WHERE id=?`, job.RetryOf))
		if err != nil {
			return model.Job{}, err
		}
		if source.Status.FinishedAt == nil {
			return model.Job{}, ErrJobActive
		}
		if source.Template.Application != job.Template.Application || source.Template.RevisionID != revision {
			return model.Job{}, ErrGenerationConflict
		}
	}
	if job.Kind == "pre_deploy" {
		var running bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM one_off_jobs WHERE application_name=? AND kind='pre_deploy' AND json_extract(status,'$.finished_at') IS NULL)`, job.Template.Application).Scan(&running); err != nil {
			return model.Job{}, err
		}
		if running {
			return model.Job{}, ErrJobActive
		}
	}
	node, err := nodeByIDInTransaction(ctx, tx, job.NodeID)
	if err != nil {
		return model.Job{}, err
	}
	if node.Lifecycle != model.NodeLifecycleActive {
		return model.Job{}, ErrJobLimit
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM one_off_jobs WHERE json_extract(status,'$.finished_at') IS NULL`).Scan(&active); err != nil {
		return model.Job{}, err
	}
	if active >= 32 {
		return model.Job{}, ErrJobLimit
	}
	var cpu, memory uint64
	// Reserve rollback processes the same way as the scheduler and
	// NodeAssignedResources: only a start-before-stop allocation can run its
	// rollback process next to the current one. A stop-first rollback replaces
	// the current process, so counting it would double the reservation.
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum(json_extract(spec,'$.cpu_millis')+CASE WHEN json_extract(spec,'$.start_before_stop') THEN COALESCE(json_extract(spec,'$.rollback.cpu_millis'),0) ELSE 0 END),0), COALESCE(sum(json_extract(spec,'$.memory_bytes')+CASE WHEN json_extract(spec,'$.start_before_stop') THEN COALESCE(json_extract(spec,'$.rollback.memory_bytes'),0) ELSE 0 END),0) FROM desired_allocations WHERE node_id=?`, job.NodeID).Scan(&cpu, &memory); err != nil {
		return model.Job{}, err
	}
	var cronCPU, cronMemory uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum(json_extract(spec,'$.cpu_millis')),0),COALESCE(sum(json_extract(spec,'$.memory_bytes')),0) FROM desired_cron_jobs WHERE node_id=? AND json_extract(spec,'$.run_id') IS NULL`, job.NodeID).Scan(&cronCPU, &cronMemory); err != nil {
		return model.Job{}, err
	}
	cpu += cronCPU
	memory += cronMemory
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum(json_extract(template,'$.cpu_millis')),0),COALESCE(sum(json_extract(template,'$.memory_bytes')),0) FROM one_off_jobs WHERE node_id=? AND json_extract(status,'$.finished_at') IS NULL`, job.NodeID).Scan(&cronCPU, &cronMemory); err != nil {
		return model.Job{}, err
	}
	cpu += cronCPU
	memory += cronMemory
	if cpu > capacityCPU || memory > capacityMemory || job.Template.CPUMillis > capacityCPU-cpu || job.Template.MemoryBytes > capacityMemory-memory {
		return model.Job{}, ErrJobLimit
	}
	template, err := json.Marshal(job.Template)
	if err != nil {
		return model.Job{}, err
	}
	status, err := json.Marshal(job.Status)
	if err != nil {
		return model.Job{}, err
	}
	stamp := job.Template.RunAt.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO one_off_jobs(id,application_name,revision_id,node_id,service_name,kind,retry_of,template,status,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, job.ID, job.Template.Application, revision, job.NodeID, job.Service, job.Kind, job.RetryOf, string(template), string(status), stamp); err != nil {
		return model.Job{}, err
	}
	response, _ := json.Marshal(job.ID)
	if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_keys(key,operation,request_hash,response,created_at) VALUES(?,'job.run',?,?,?)`, key, hash, string(response), stamp); err != nil {
		return model.Job{}, err
	}
	return job, tx.Commit()
}
func (s *Store) CancelJob(ctx context.Context, id string) (model.Job, error) {
	_, err := s.db.ExecContext(ctx, `UPDATE one_off_jobs SET cancel_requested=1 WHERE id=? AND json_extract(status,'$.finished_at') IS NULL`, id)
	if err != nil {
		return model.Job{}, err
	}
	return s.Job(ctx, id)
}
func (s *Store) CancelObsoleteJobs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE one_off_jobs SET cancel_requested=1 WHERE json_extract(status,'$.finished_at') IS NULL AND EXISTS(SELECT 1 FROM applications a JOIN application_revisions r ON r.application_name=a.name AND r.generation=a.generation WHERE a.name=one_off_jobs.application_name AND (a.deleted_at IS NOT NULL OR (one_off_jobs.kind='pre_deploy' AND r.id!=one_off_jobs.revision_id)))`)
	return err
}
func observeOneOffTx(ctx context.Context, tx *sql.Tx, node string, desired model.CronJobDesired, run model.JobRunStatus) error {
	if desired.RunID == "" {
		return nil
	}
	if run.ID != desired.RunID || desired.RunAt == nil || !run.ScheduledAt.Equal(*desired.RunAt) {
		return fmt.Errorf("one-off run identity is not authorized")
	}
	current, err := scanJob(tx.QueryRowContext(ctx, jobSelect+` WHERE id=? AND node_id=?`, run.ID, node))
	if err != nil {
		return err
	}
	if current.Template.SpecDigest != run.SpecDigest {
		return fmt.Errorf("one-off run spec is not authorized")
	}
	if current.Status.FinishedAt != nil {
		return nil
	}
	terminal := run.State != model.JobRunWaiting && run.State != model.JobRunRunning
	if terminal != (run.FinishedAt != nil) || (run.State == model.JobRunSucceeded && (run.ExitCode == nil || *run.ExitCode != 0)) {
		return fmt.Errorf("one-off terminal result is incomplete")
	}
	status, err := json.Marshal(run)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE one_off_jobs SET status=? WHERE id=?`, string(status), run.ID)
	return err
}
