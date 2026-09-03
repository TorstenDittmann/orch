package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

var (
	ErrStaleAssignment       = errors.New("assignment generation is stale")
	ErrAssignmentConflict    = errors.New("assignment generation has a different digest")
	ErrNoAssignment          = errors.New("no assignment is cached")
	ErrAgentIdentityMismatch = errors.New("agent cache belongs to a different identity")
)

type Store struct {
	db        *sql.DB
	closeOnce sync.Once
	closeErr  error
}

type AllocationState struct {
	ID            string
	SpecDigest    string
	Phase         string
	RestartCount  uint32
	NextAttemptAt *time.Time
	ExitCode      *int32
	Reason        string
	Message       string
	UpdatedAt     time.Time
}

func Open(ctx context.Context, path, clusterID, nodeID string) (*Store, error) {
	if strings.TrimSpace(path) == "" || clusterID == "" || nodeID == "" {
		return nil, fmt.Errorf("database path, cluster id, and node id are required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	parent, err := os.Lstat(filepath.Dir(absolute))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("agent database parent must be a private non-symlink directory")
	}
	if info, err := os.Lstat(absolute); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("agent database must be a private regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else {
		file, err := os.OpenFile(absolute, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create private agent database: %w", err)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("close new agent database: %w", err)
		}
	}
	for _, sidecar := range []string{absolute + "-wal", absolute + "-shm"} {
		if err := validateOptionalPrivateFile(sidecar); err != nil {
			return nil, fmt.Errorf("validate agent database sidecar: %w", err)
		}
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute), RawQuery: "_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	fail := func(openErr error) (*Store, error) {
		_ = db.Close()
		return nil, openErr
	}
	if err := db.PingContext(ctx); err != nil {
		return fail(err)
	}
	if err := migrate(ctx, db); err != nil {
		return fail(err)
	}
	if err := os.Chmod(absolute, 0o600); err != nil {
		return fail(err)
	}
	for _, sidecar := range []string{absolute + "-wal", absolute + "-shm"} {
		if err := chmodOptionalPrivateFile(sidecar); err != nil {
			return fail(fmt.Errorf("protect agent database sidecar: %w", err))
		}
	}
	var storedCluster, storedNode string
	err = db.QueryRowContext(ctx, `SELECT cluster_id, node_id FROM agent_identity WHERE singleton = 1`).Scan(&storedCluster, &storedNode)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := db.ExecContext(ctx, `INSERT INTO agent_identity(singleton, cluster_id, node_id) VALUES (1, ?, ?)`, clusterID, nodeID); err != nil {
			return fail(fmt.Errorf("initialize agent cache identity: %w", err))
		}
	} else if err != nil {
		return fail(fmt.Errorf("read agent cache identity: %w", err))
	} else if storedCluster != clusterID || storedNode != nodeID {
		return fail(ErrAgentIdentityMismatch)
	}
	return &Store{db: db}, nil
}

func validateOptionalPrivateFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s must be a private regular file", path)
	}
	return nil
}

func chmodOptionalPrivateFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	return os.Chmod(path, 0o600)
}

func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL) STRICT`); err != nil {
		return fmt.Errorf("create agent migration table: %w", err)
	}
	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	var maximum int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(max(version), 0) FROM schema_migrations`).Scan(&maximum); err != nil {
		return err
	}
	if maximum > len(names) {
		return fmt.Errorf("agent database schema version %d is newer than this binary", maximum)
	}
	for index, name := range names {
		version := index + 1
		contents, err := fs.ReadFile(migrationFiles, name)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(contents)
		checksum := hex.EncodeToString(digest[:])
		var storedName, storedChecksum string
		err = db.QueryRowContext(ctx, `SELECT name, checksum FROM schema_migrations WHERE version = ?`, version).Scan(&storedName, &storedChecksum)
		if err == nil {
			if storedName != filepath.Base(name) || storedChecksum != checksum {
				return fmt.Errorf("agent migration %d changed after application", version)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(contents)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply agent migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, checksum) VALUES (?, ?, ?)`, version, filepath.Base(name), checksum); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// CommitAssignment validates and durably replaces the complete cached
// snapshot. Returning success is the boundary after which an ACK is safe.
func (s *Store) CommitAssignment(ctx context.Context, assignment model.Assignment) (bool, error) {
	if assignment.NodeID == "" || assignment.Generation == 0 || assignment.CreatedAt.IsZero() {
		return false, fmt.Errorf("assignment is incomplete")
	}
	payload, err := json.Marshal(assignment.Allocations)
	if err != nil {
		return false, fmt.Errorf("encode assignment: %w", err)
	}
	digest := sha256.Sum256(payload)
	if assignment.Digest != "sha256:"+hex.EncodeToString(digest[:]) {
		return false, fmt.Errorf("assignment digest does not match allocations")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var storedNode string
	if err := tx.QueryRowContext(ctx, `SELECT node_id FROM agent_identity WHERE singleton = 1`).Scan(&storedNode); err != nil {
		return false, err
	}
	if assignment.NodeID != storedNode {
		return false, ErrAgentIdentityMismatch
	}
	var generation int64
	var currentDigest string
	err = tx.QueryRowContext(ctx, `SELECT generation, digest FROM assignment WHERE singleton = 1`).Scan(&generation, &currentDigest)
	if err == nil {
		if assignment.Generation < uint64(generation) {
			return false, ErrStaleAssignment
		}
		if assignment.Generation == uint64(generation) {
			if assignment.Digest != currentDigest {
				return false, ErrAssignmentConflict
			}
			return false, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO assignment(singleton, generation, digest, payload, created_at)
		VALUES (1, ?, ?, ?, ?) ON CONFLICT(singleton) DO UPDATE SET generation = excluded.generation,
		digest = excluded.digest, payload = excluded.payload, created_at = excluded.created_at`,
		assignment.Generation, assignment.Digest, string(payload), assignment.CreatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return false, fmt.Errorf("write cached assignment: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM desired_allocations`); err != nil {
		return false, err
	}
	previousID := ""
	for _, allocation := range assignment.Allocations {
		if previousID != "" && allocation.ID <= previousID {
			return false, fmt.Errorf("cached allocations are not in strict canonical order")
		}
		previousID = allocation.ID
		specDigest, err := model.AllocationSpecDigest(allocation)
		if err != nil || specDigest != allocation.SpecDigest {
			return false, fmt.Errorf("allocation %q spec digest does not match", allocation.ID)
		}
		encoded, err := json.Marshal(allocation)
		if err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO desired_allocations(id, spec_digest, spec) VALUES (?, ?, ?)`, allocation.ID, allocation.SpecDigest, string(encoded)); err != nil {
			return false, fmt.Errorf("write cached allocation %q: %w", allocation.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit cached assignment: %w", err)
	}
	return true, nil
}

func (s *Store) Assignment(ctx context.Context) (model.Assignment, error) {
	var assignment model.Assignment
	var generation int64
	var payload, createdAt string
	err := s.db.QueryRowContext(ctx, `SELECT i.node_id, a.generation, a.digest, a.payload, a.created_at
		FROM assignment a CROSS JOIN agent_identity i WHERE a.singleton = 1 AND i.singleton = 1`).Scan(
		&assignment.NodeID, &generation, &assignment.Digest, &payload, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Assignment{}, ErrNoAssignment
	}
	if err != nil {
		return model.Assignment{}, err
	}
	assignment.Generation = uint64(generation)
	assignment.Payload = json.RawMessage(payload)
	if err := json.Unmarshal([]byte(payload), &assignment.Allocations); err != nil {
		return model.Assignment{}, fmt.Errorf("decode cached assignment: %w", err)
	}
	assignment.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	return assignment, err
}

func (s *Store) AllocationStates(ctx context.Context) ([]AllocationState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, spec_digest, phase, restart_count,
		next_attempt_at, exit_code, reason, message, updated_at FROM allocation_state ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AllocationState
	for rows.Next() {
		var item AllocationState
		var restart int64
		var nextAttempt sql.NullString
		var exit sql.NullInt64
		var updated string
		if err := rows.Scan(&item.ID, &item.SpecDigest, &item.Phase, &restart, &nextAttempt, &exit, &item.Reason, &item.Message, &updated); err != nil {
			return nil, err
		}
		item.RestartCount = uint32(restart)
		item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, err
		}
		if nextAttempt.Valid {
			value, err := time.Parse(time.RFC3339Nano, nextAttempt.String)
			if err != nil {
				return nil, err
			}
			item.NextAttemptAt = &value
		}
		if exit.Valid {
			value := int32(exit.Int64)
			item.ExitCode = &value
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) SaveAllocationState(ctx context.Context, item AllocationState) error {
	if item.ID == "" || item.SpecDigest == "" || item.Phase == "" || item.UpdatedAt.IsZero() {
		return fmt.Errorf("allocation state is incomplete")
	}
	var nextAttempt any
	if item.NextAttemptAt != nil {
		nextAttempt = item.NextAttemptAt.UTC().Format(time.RFC3339Nano)
	}
	var exitCode any
	if item.ExitCode != nil {
		exitCode = *item.ExitCode
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO allocation_state
		(id, spec_digest, phase, restart_count, next_attempt_at, exit_code, reason, message, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET
		spec_digest = excluded.spec_digest, phase = excluded.phase, restart_count = excluded.restart_count,
		next_attempt_at = excluded.next_attempt_at, exit_code = excluded.exit_code,
		reason = excluded.reason, message = excluded.message, updated_at = excluded.updated_at`,
		item.ID, item.SpecDigest, item.Phase, item.RestartCount, nextAttempt, exitCode,
		item.Reason, item.Message, item.UpdatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) DeleteAllocationState(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM allocation_state WHERE id = ?`, id)
	return err
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		_, checkpointErr := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		closeErr := s.db.Close()
		s.closeErr = errors.Join(checkpointErr, closeErr)
	})
	return s.closeErr
}
