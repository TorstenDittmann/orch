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
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/network"
	"change.codes/torsten/better-swarm/internal/security"
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
	secretBox *security.SecretBox
	closeOnce sync.Once
	closeErr  error
}

func (s *Store) ConfigureSecretBox(ctx context.Context, box *security.SecretBox) error {
	if box == nil {
		return errors.New("agent secret cache key is required")
	}
	var fingerprint string
	err := s.db.QueryRowContext(ctx, `SELECT fingerprint FROM secret_cache_key_metadata WHERE singleton = 1`).Scan(&fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO secret_cache_key_metadata(singleton, fingerprint) VALUES (1, ?)`, box.Fingerprint()); err != nil {
			return fmt.Errorf("initialize agent secret cache key: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("read agent secret cache key metadata: %w", err)
	} else if fingerprint != box.Fingerprint() {
		return errors.New("agent secret cache key does not match encrypted cache")
	}
	s.secretBox = box
	return nil
}

type AllocationState struct {
	ID                   string
	SpecDigest           string
	Phase                string
	RestartCount         uint32
	NextAttemptAt        *time.Time
	ExitCode             *int32
	Reason               string
	Message              string
	UpdatedAt            time.Time
	HealthStatus         string
	Address              string
	ActiveSpecDigest     string
	ActiveGeneration     uint64
	ActiveRevisionID     string
	ReadyAt              *time.Time
	ConsecutiveSuccesses uint32
	ConsecutiveFailures  uint32
	LastProbeAt          *time.Time
}

type ServingEndpoint struct {
	AllocationID string
	Application  string
	Service      string
	Address      string
	SpecDigest   string
	UpdatedAt    time.Time
}

type Drain struct {
	ObjectID     string
	AllocationID string
	SpecDigest   string
	Until        time.Time
}

type CronJobState struct {
	Application     string
	Job             string
	SpecDigest      string
	LastScheduledAt *time.Time
	NextRunAt       *time.Time
	ActiveRunID     string
	UpdatedAt       time.Time
}

type CronRunState struct {
	model.JobRunStatus
	ObjectID string
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
	payload, digest, err := model.AssignmentPayloadWithJobs(assignment.Network, assignment.Allocations, assignment.CronJobs)
	if err != nil {
		return false, err
	}
	if assignment.Digest != digest {
		return false, fmt.Errorf("assignment digest does not match allocations")
	}
	encryptedMaterials, err := s.encryptAssignmentMaterials(assignment)
	if err != nil {
		return false, err
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
			if err := verifyStoredMaterialHashes(ctx, tx, encryptedMaterials); err != nil {
				return false, err
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
		digestMatches, err := model.AllocationSpecDigestMatches(allocation)
		if err != nil || !digestMatches {
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM desired_cron_jobs`); err != nil {
		return false, err
	}
	previousJob := ""
	for _, job := range assignment.CronJobs {
		key := job.Application + "\x00" + job.Job
		if previousJob != "" && key <= previousJob {
			return false, fmt.Errorf("cached cron jobs are not in strict canonical order")
		}
		previousJob = key
		digest, err := model.CronJobSpecDigest(job)
		if err != nil || digest != job.SpecDigest {
			return false, fmt.Errorf("cron job %q spec digest does not match", key)
		}
		encoded, err := json.Marshal(job)
		if err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO desired_cron_jobs(application_name, job_name, spec_digest, spec) VALUES (?, ?, ?, ?)`,
			job.Application, job.Job, job.SpecDigest, string(encoded)); err != nil {
			return false, fmt.Errorf("write cached cron job %q: %w", key, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM secret_materials`); err != nil {
		return false, err
	}
	for _, item := range encryptedMaterials {
		if _, err := tx.ExecContext(ctx, `INSERT INTO secret_materials
			(kind, application_name, secret_name, registry_host, username, version_id, algorithm, nonce,
			 ciphertext, wrapped_key_nonce, wrapped_key, value_hmac, assignment_generation)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, item.material.Kind, item.material.Application,
			item.material.Name, item.material.Host, item.material.Username, item.material.VersionID,
			item.encrypted.Algorithm, item.encrypted.Nonce, item.encrypted.Ciphertext,
			item.encrypted.WrappedKeyNonce, item.encrypted.WrappedKey, item.valueHMAC, assignment.Generation); err != nil {
			return false, fmt.Errorf("write encrypted agent secret cache: %w", err)
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
	assignment.Network, assignment.Allocations, assignment.CronJobs, err = model.DecodeAssignmentDocument([]byte(payload))
	if err != nil {
		return model.Assignment{}, fmt.Errorf("decode cached assignment: %w", err)
	}
	assignment.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return model.Assignment{}, err
	}
	assignment.Materials, err = s.loadAssignmentMaterials(ctx, assignment.NodeID, assignment.Generation)
	if err != nil {
		return model.Assignment{}, err
	}
	return assignment, nil
}

type encryptedAssignmentMaterial struct {
	material  model.SecretMaterial
	encrypted security.EncryptedValue
	valueHMAC string
}

func (s *Store) encryptAssignmentMaterials(assignment model.Assignment) ([]encryptedAssignmentMaterial, error) {
	expected := assignmentSecretReferences(assignment)
	if len(expected) == 0 && len(assignment.Materials) == 0 {
		return nil, nil
	}
	if s.secretBox == nil {
		return nil, errors.New("agent secret cache key is unavailable")
	}
	provided := make(map[string]model.SecretMaterial, len(assignment.Materials))
	for _, material := range assignment.Materials {
		key := secretMaterialKey(material)
		if _, duplicate := provided[key]; duplicate || len(material.Value) == 0 || len(material.Value) > 1<<20 {
			return nil, errors.New("assignment secret materials are invalid or duplicated")
		}
		provided[key] = material
	}
	if len(expected) != len(provided) {
		return nil, errors.New("assignment secret materials do not match its references")
	}
	keys := make([]string, 0, len(expected))
	for key, reference := range expected {
		material, ok := provided[key]
		if !ok || material.Username != reference.Username {
			return nil, errors.New("assignment secret materials do not match its references")
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]encryptedAssignmentMaterial, 0, len(keys))
	for _, key := range keys {
		material := provided[key]
		aad := agentSecretAAD(assignment.NodeID, assignment.Generation, material)
		encrypted, err := s.secretBox.Seal(aad, material.Value)
		if err != nil {
			return nil, fmt.Errorf("encrypt agent secret cache: %w", err)
		}
		valueHMAC := s.secretBox.RequestHMAC([]byte("agent-cache-material"), aad, material.Value)
		result = append(result, encryptedAssignmentMaterial{material: material, encrypted: encrypted, valueHMAC: valueHMAC})
	}
	return result, nil
}

func verifyStoredMaterialHashes(ctx context.Context, tx *sql.Tx, materials []encryptedAssignmentMaterial) error {
	rows, err := tx.QueryContext(ctx, `SELECT kind, application_name, secret_name, registry_host, version_id, value_hmac
		FROM secret_materials ORDER BY kind, application_name, secret_name, registry_host, version_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	stored := make(map[string]string)
	for rows.Next() {
		var material model.SecretMaterial
		var valueHMAC string
		if err := rows.Scan(&material.Kind, &material.Application, &material.Name, &material.Host, &material.VersionID, &valueHMAC); err != nil {
			return err
		}
		stored[secretMaterialKey(material)] = valueHMAC
	}
	if err := rows.Err(); err != nil || len(stored) != len(materials) {
		if err != nil {
			return err
		}
		return ErrAssignmentConflict
	}
	for _, material := range materials {
		if stored[secretMaterialKey(material.material)] != material.valueHMAC {
			return ErrAssignmentConflict
		}
	}
	return nil
}

func (s *Store) loadAssignmentMaterials(ctx context.Context, nodeID string, generation uint64) ([]model.SecretMaterial, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, application_name, secret_name, registry_host, username,
		version_id, algorithm, nonce, ciphertext, wrapped_key_nonce, wrapped_key
		FROM secret_materials WHERE assignment_generation = ?
		ORDER BY kind, application_name, secret_name, registry_host, version_id`, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.SecretMaterial
	for rows.Next() {
		var material model.SecretMaterial
		var encrypted security.EncryptedValue
		if err := rows.Scan(&material.Kind, &material.Application, &material.Name, &material.Host, &material.Username,
			&material.VersionID, &encrypted.Algorithm, &encrypted.Nonce, &encrypted.Ciphertext,
			&encrypted.WrappedKeyNonce, &encrypted.WrappedKey); err != nil {
			return nil, err
		}
		if s.secretBox == nil {
			return nil, errors.New("agent secret cache key is unavailable")
		}
		material.Value, err = s.secretBox.Open(agentSecretAAD(nodeID, generation, material), encrypted)
		if err != nil {
			return nil, fmt.Errorf("decrypt agent secret cache: %w", err)
		}
		result = append(result, material)
	}
	return result, rows.Err()
}

func assignmentSecretReferences(assignment model.Assignment) map[string]model.SecretMaterial {
	result := make(map[string]model.SecretMaterial)
	collect := func(application string, environment map[string]model.SecretReference, files []model.SecretFileDesired, registry *model.RegistryCredentialReference) {
		for _, reference := range environment {
			material := model.SecretMaterial{Kind: model.SecretMaterialApplication, Application: application, Name: reference.Name, VersionID: reference.VersionID}
			result[secretMaterialKey(material)] = material
		}
		for _, file := range files {
			material := model.SecretMaterial{Kind: model.SecretMaterialApplication, Application: application, Name: file.Name, VersionID: file.VersionID}
			result[secretMaterialKey(material)] = material
		}
		if registry != nil {
			material := model.SecretMaterial{Kind: model.SecretMaterialRegistry, Host: registry.Host, VersionID: registry.VersionID, Username: registry.Username}
			result[secretMaterialKey(material)] = material
		}
	}
	for _, allocation := range assignment.Allocations {
		collect(allocation.Application, allocation.SecretEnvironment, allocation.SecretFiles, allocation.RegistryCredential)
		if allocation.Rollback != nil {
			collect(allocation.Application, allocation.Rollback.SecretEnvironment, allocation.Rollback.SecretFiles, allocation.Rollback.RegistryCredential)
		}
	}
	for _, job := range assignment.CronJobs {
		collect(job.Application, job.SecretEnvironment, job.SecretFiles, job.RegistryCredential)
	}
	return result
}

func secretMaterialKey(material model.SecretMaterial) string {
	return material.Kind + "\x00" + material.Application + "\x00" + material.Name + "\x00" + material.Host + "\x00" + material.VersionID
}

func agentSecretAAD(nodeID string, generation uint64, material model.SecretMaterial) []byte {
	return security.SecretAssociatedData("agent-cache", nodeID, strconv.FormatUint(generation, 10), material.Kind,
		material.Application, material.Name, material.Host, material.Username, material.VersionID)
}

func (s *Store) AllocationStates(ctx context.Context) ([]AllocationState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, spec_digest, phase, restart_count,
		next_attempt_at, exit_code, reason, message, updated_at, health_status, address,
		active_spec_digest, active_generation, active_revision_id, ready_at,
		consecutive_successes, consecutive_failures, last_probe_at FROM allocation_state ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AllocationState
	for rows.Next() {
		var item AllocationState
		var restart, activeGeneration, successes, failures int64
		var nextAttempt, readyAt, lastProbeAt sql.NullString
		var exit sql.NullInt64
		var updated string
		if err := rows.Scan(&item.ID, &item.SpecDigest, &item.Phase, &restart, &nextAttempt, &exit,
			&item.Reason, &item.Message, &updated, &item.HealthStatus, &item.Address,
			&item.ActiveSpecDigest, &activeGeneration, &item.ActiveRevisionID, &readyAt,
			&successes, &failures, &lastProbeAt); err != nil {
			return nil, err
		}
		item.RestartCount = uint32(restart)
		item.ActiveGeneration = uint64(activeGeneration)
		item.ConsecutiveSuccesses = uint32(successes)
		item.ConsecutiveFailures = uint32(failures)
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
		if readyAt.Valid {
			value, err := time.Parse(time.RFC3339Nano, readyAt.String)
			if err != nil {
				return nil, err
			}
			item.ReadyAt = &value
		}
		if lastProbeAt.Valid {
			value, err := time.Parse(time.RFC3339Nano, lastProbeAt.String)
			if err != nil {
				return nil, err
			}
			item.LastProbeAt = &value
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
	if item.HealthStatus == "" {
		item.HealthStatus = model.HealthNotConfigured
	}
	var readyAt any
	if item.ReadyAt != nil {
		readyAt = item.ReadyAt.UTC().Format(time.RFC3339Nano)
	}
	var lastProbeAt any
	if item.LastProbeAt != nil {
		lastProbeAt = item.LastProbeAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO allocation_state
		(id, spec_digest, phase, restart_count, next_attempt_at, exit_code, reason, message, updated_at,
		 health_status, address, active_spec_digest, active_generation, active_revision_id, ready_at,
		 consecutive_successes, consecutive_failures, last_probe_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET
		spec_digest = excluded.spec_digest, phase = excluded.phase, restart_count = excluded.restart_count,
		next_attempt_at = excluded.next_attempt_at, exit_code = excluded.exit_code,
		reason = excluded.reason, message = excluded.message, updated_at = excluded.updated_at,
		health_status = excluded.health_status, address = excluded.address,
		active_spec_digest = excluded.active_spec_digest, active_generation = excluded.active_generation,
		active_revision_id = excluded.active_revision_id, ready_at = excluded.ready_at,
		consecutive_successes = excluded.consecutive_successes, consecutive_failures = excluded.consecutive_failures,
		last_probe_at = excluded.last_probe_at`,
		item.ID, item.SpecDigest, item.Phase, item.RestartCount, nextAttempt, exitCode,
		item.Reason, item.Message, item.UpdatedAt.UTC().Format(time.RFC3339Nano), item.HealthStatus,
		item.Address, item.ActiveSpecDigest, item.ActiveGeneration, item.ActiveRevisionID, readyAt,
		item.ConsecutiveSuccesses, item.ConsecutiveFailures, lastProbeAt)
	return err
}

func (s *Store) SaveNetworkStatus(ctx context.Context, item model.NetworkStatus, updatedAt time.Time) error {
	if item.Generation == 0 || item.ClusterCIDR == "" || item.NodeSubnet == "" || item.Gateway == "" || updatedAt.IsZero() {
		return fmt.Errorf("network status is incomplete")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO network_state
		(singleton, generation, cluster_cidr, node_subnet, gateway, state, reason, message, dns_state, ingress_state, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(singleton) DO UPDATE SET
		generation = excluded.generation, cluster_cidr = excluded.cluster_cidr,
		node_subnet = excluded.node_subnet, gateway = excluded.gateway, state = excluded.state,
		reason = excluded.reason, message = excluded.message, dns_state = excluded.dns_state,
		ingress_state = excluded.ingress_state, updated_at = excluded.updated_at`, item.Generation,
		item.ClusterCIDR, item.NodeSubnet, item.Gateway, item.State, item.Reason, item.Message,
		item.DNSState, item.IngressState, updatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) NetworkStatus(ctx context.Context) (model.NetworkStatus, error) {
	var item model.NetworkStatus
	err := s.db.QueryRowContext(ctx, `SELECT generation, cluster_cidr, node_subnet, gateway,
		state, reason, message, dns_state, ingress_state FROM network_state WHERE singleton = 1`).Scan(
		&item.Generation, &item.ClusterCIDR, &item.NodeSubnet, &item.Gateway, &item.State,
		&item.Reason, &item.Message, &item.DNSState, &item.IngressState)
	if errors.Is(err, sql.ErrNoRows) {
		return model.NetworkStatus{}, ErrNoAssignment
	}
	return item, err
}

func (s *Store) SaveAttachment(ctx context.Context, item network.Attachment) error {
	if item.AllocationID == "" || item.ObjectID == "" || item.SpecDigest == "" || item.NetworkGeneration == 0 ||
		!item.Address.IsValid() || !item.Address.Is4() || item.CreatedAt.IsZero() {
		return fmt.Errorf("network attachment is incomplete")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO network_attachments
		(allocation_id, object_id, spec_digest, network_generation, address, netns, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, item.AllocationID, item.ObjectID,
		item.SpecDigest, item.NetworkGeneration, item.Address.String(), item.NetNS,
		item.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	var allocationID, specDigest, address string
	var generation int64
	if err := tx.QueryRowContext(ctx, `SELECT allocation_id, spec_digest, network_generation, address
		FROM network_attachments WHERE object_id = ?`, item.ObjectID).
		Scan(&allocationID, &specDigest, &generation, &address); err != nil {
		return err
	}
	if allocationID != item.AllocationID || specDigest != item.SpecDigest || uint64(generation) != item.NetworkGeneration || address != item.Address.String() {
		return network.ErrOwnershipMismatch
	}
	return tx.Commit()
}

func (s *Store) Attachment(ctx context.Context, objectID string) (network.Attachment, error) {
	var item network.Attachment
	var address, createdAt string
	var generation int64
	err := s.db.QueryRowContext(ctx, `SELECT allocation_id, object_id, spec_digest,
		network_generation, address, netns, created_at FROM network_attachments WHERE object_id = ?`, objectID).
		Scan(&item.AllocationID, &item.ObjectID, &item.SpecDigest, &generation, &address, &item.NetNS, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return network.Attachment{}, network.ErrNotFound
	}
	if err != nil {
		return network.Attachment{}, err
	}
	if generation < 1 {
		return network.Attachment{}, fmt.Errorf("stored network attachment has an invalid generation")
	}
	item.NetworkGeneration = uint64(generation)
	item.Address, err = netip.ParseAddr(address)
	if err != nil || !item.Address.Is4() {
		return network.Attachment{}, fmt.Errorf("stored network attachment has an invalid address")
	}
	item.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return network.Attachment{}, err
	}
	return item, nil
}

func (s *Store) Attachments(ctx context.Context) ([]network.Attachment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT object_id FROM network_attachments ORDER BY object_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var objectIDs []string
	for rows.Next() {
		var objectID string
		if err := rows.Scan(&objectID); err != nil {
			return nil, err
		}
		objectIDs = append(objectIDs, objectID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]network.Attachment, 0, len(objectIDs))
	for _, objectID := range objectIDs {
		item, err := s.Attachment(ctx, objectID)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func (s *Store) DeleteAttachment(ctx context.Context, objectID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM network_attachments WHERE object_id = ?`, objectID)
	return err
}

func (s *Store) ServingEndpoints(ctx context.Context) ([]ServingEndpoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT allocation_id, application_name, service_name,
		address, spec_digest, updated_at FROM serving_state ORDER BY allocation_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ServingEndpoint
	for rows.Next() {
		var item ServingEndpoint
		var updatedAt string
		if err := rows.Scan(&item.AllocationID, &item.Application, &item.Service, &item.Address, &item.SpecDigest, &updatedAt); err != nil {
			return nil, err
		}
		address, parseErr := netip.ParseAddr(item.Address)
		if parseErr != nil || !address.Is4() || !address.IsPrivate() {
			return nil, fmt.Errorf("stored serving endpoint %q has an invalid address", item.AllocationID)
		}
		item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) ReplaceServingEndpoints(ctx context.Context, endpoints []ServingEndpoint) error {
	if err := s.StageServingEndpoints(ctx, endpoints); err != nil {
		return err
	}
	return s.CommitServingEndpoints(ctx)
}

// StageServingEndpoints durably records the complete candidate snapshot before
// an ingress controller can advertise it. The accepted snapshot is untouched
// until CommitServingEndpoints succeeds after route validation.
func (s *Store) StageServingEndpoints(ctx context.Context, endpoints []ServingEndpoint) error {
	endpoints = append([]ServingEndpoint(nil), endpoints...)
	sort.Slice(endpoints, func(i, j int) bool { return endpoints[i].AllocationID < endpoints[j].AllocationID })
	previous := ""
	for _, item := range endpoints {
		address, err := netip.ParseAddr(item.Address)
		if item.AllocationID == "" || item.AllocationID <= previous || item.Application == "" || item.Service == "" ||
			item.SpecDigest == "" || err != nil || !address.Is4() || !address.IsPrivate() || item.UpdatedAt.IsZero() {
			return fmt.Errorf("serving endpoint snapshot is invalid")
		}
		previous = item.AllocationID
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM candidate_serving_state`); err != nil {
		return err
	}
	for _, item := range endpoints {
		if _, err := tx.ExecContext(ctx, `INSERT INTO candidate_serving_state
			(allocation_id, application_name, service_name, address, spec_digest, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`, item.AllocationID, item.Application, item.Service,
			item.Address, item.SpecDigest, item.UpdatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CommitServingEndpoints atomically promotes the previously staged candidate
// after ingress validation. A crash before this transaction leaves the last
// accepted DNS snapshot intact and the candidate available for a safe retry.
func (s *Store) CommitServingEndpoints(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM serving_state`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO serving_state
		(allocation_id, application_name, service_name, address, spec_digest, updated_at)
		SELECT allocation_id, application_name, service_name, address, spec_digest, updated_at
		FROM candidate_serving_state ORDER BY allocation_id`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM candidate_serving_state`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) BeginDrain(ctx context.Context, item Drain) (time.Time, error) {
	if item.ObjectID == "" || item.AllocationID == "" || item.SpecDigest == "" || item.Until.IsZero() {
		return time.Time{}, fmt.Errorf("drain state is incomplete")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO drain_state(object_id, allocation_id, spec_digest, drain_until)
		VALUES (?, ?, ?, ?) ON CONFLICT(object_id) DO NOTHING`, item.ObjectID, item.AllocationID,
		item.SpecDigest, item.Until.UTC().Format(time.RFC3339Nano)); err != nil {
		return time.Time{}, err
	}
	var allocationID, specDigest, untilText string
	if err := tx.QueryRowContext(ctx, `SELECT allocation_id, spec_digest, drain_until FROM drain_state WHERE object_id = ?`, item.ObjectID).
		Scan(&allocationID, &specDigest, &untilText); err != nil {
		return time.Time{}, err
	}
	if allocationID != item.AllocationID || specDigest != item.SpecDigest {
		return time.Time{}, network.ErrOwnershipMismatch
	}
	until, err := time.Parse(time.RFC3339Nano, untilText)
	if err != nil {
		return time.Time{}, err
	}
	if err := tx.Commit(); err != nil {
		return time.Time{}, err
	}
	return until, nil
}

func (s *Store) Drains(ctx context.Context) ([]Drain, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT object_id, allocation_id, spec_digest, drain_until FROM drain_state ORDER BY object_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Drain
	for rows.Next() {
		var item Drain
		var until string
		if err := rows.Scan(&item.ObjectID, &item.AllocationID, &item.SpecDigest, &until); err != nil {
			return nil, err
		}
		item.Until, err = time.Parse(time.RFC3339Nano, until)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) DeleteDrain(ctx context.Context, objectID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM drain_state WHERE object_id = ?`, objectID)
	return err
}

func (s *Store) DeleteAllocationState(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM allocation_state WHERE id = ?`, id)
	return err
}

func (s *Store) CronJobStates(ctx context.Context) ([]CronJobState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT application_name, job_name, spec_digest,
		last_scheduled_at, next_run_at, active_run_id, updated_at
		FROM cron_job_state ORDER BY application_name, job_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CronJobState
	for rows.Next() {
		var item CronJobState
		var last, next sql.NullString
		var updated string
		if err := rows.Scan(&item.Application, &item.Job, &item.SpecDigest, &last, &next, &item.ActiveRunID, &updated); err != nil {
			return nil, err
		}
		item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, err
		}
		if last.Valid {
			value, err := time.Parse(time.RFC3339Nano, last.String)
			if err != nil {
				return nil, err
			}
			item.LastScheduledAt = &value
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

func (s *Store) SaveCronJobState(ctx context.Context, item CronJobState) error {
	if item.Application == "" || item.Job == "" || item.SpecDigest == "" || item.UpdatedAt.IsZero() {
		return fmt.Errorf("cron job state is incomplete")
	}
	var last, next any
	if item.LastScheduledAt != nil {
		last = item.LastScheduledAt.UTC().Format(time.RFC3339Nano)
	}
	if item.NextRunAt != nil {
		next = item.NextRunAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO cron_job_state
		(application_name, job_name, spec_digest, last_scheduled_at, next_run_at, active_run_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(application_name, job_name) DO UPDATE SET
		spec_digest=excluded.spec_digest, last_scheduled_at=excluded.last_scheduled_at,
		next_run_at=excluded.next_run_at, active_run_id=excluded.active_run_id, updated_at=excluded.updated_at`,
		item.Application, item.Job, item.SpecDigest, last, next, item.ActiveRunID, item.UpdatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) DeleteCronJobStatesExcept(ctx context.Context, keys map[string]struct{}) error {
	states, err := s.CronJobStates(ctx)
	if err != nil {
		return err
	}
	for _, item := range states {
		if _, ok := keys[item.Application+"\x00"+item.Job]; ok {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM cron_job_state WHERE application_name=? AND job_name=?`, item.Application, item.Job); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) CronRunStates(ctx context.Context) ([]CronRunState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, application_name, job_name, spec_digest, scheduled_at,
		state, object_id, exit_code, started_at, finished_at, reason, message, updated_at
		FROM cron_run_state ORDER BY application_name, job_name, scheduled_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CronRunState
	for rows.Next() {
		var item CronRunState
		var scheduled, updated string
		var started, finished sql.NullString
		var exit sql.NullInt64
		if err := rows.Scan(&item.ID, &item.Application, &item.Job, &item.SpecDigest, &scheduled,
			&item.State, &item.ObjectID, &exit, &started, &finished, &item.Reason, &item.Message, &updated); err != nil {
			return nil, err
		}
		var err error
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

func (s *Store) SaveCronRunState(ctx context.Context, item CronRunState) error {
	if item.ID == "" || item.Application == "" || item.Job == "" || item.SpecDigest == "" || item.ScheduledAt.IsZero() || item.UpdatedAt.IsZero() {
		return fmt.Errorf("cron run state is incomplete")
	}
	var exit, started, finished any
	if item.ExitCode != nil {
		exit = *item.ExitCode
	}
	if item.StartedAt != nil {
		started = item.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	if item.FinishedAt != nil {
		finished = item.FinishedAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO cron_run_state
		(id, application_name, job_name, spec_digest, scheduled_at, state, object_id, exit_code, started_at, finished_at, reason, message, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET state=excluded.state,
		object_id=excluded.object_id, exit_code=excluded.exit_code, started_at=excluded.started_at,
		finished_at=excluded.finished_at, reason=excluded.reason, message=excluded.message, updated_at=excluded.updated_at`,
		item.ID, item.Application, item.Job, item.SpecDigest, item.ScheduledAt.UTC().Format(time.RFC3339Nano), item.State,
		item.ObjectID, exit, started, finished, item.Reason, item.Message, item.UpdatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) CronRunsBeyond(ctx context.Context, application, job string, keep int) ([]CronRunState, error) {
	if keep < 1 {
		return nil, fmt.Errorf("cron run retention must be positive")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, object_id FROM cron_run_state
		WHERE application_name=? AND job_name=? AND state NOT IN ('WAITING', 'RUNNING')
		ORDER BY scheduled_at DESC LIMIT -1 OFFSET ?`, application, job, keep)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CronRunState
	for rows.Next() {
		var item CronRunState
		if err := rows.Scan(&item.ID, &item.ObjectID); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) DeleteCronRunState(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cron_run_state WHERE id=?`, id)
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
