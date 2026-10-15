package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
	modernsqlite "modernc.org/sqlite"
)

var (
	ErrNotInitialized     = errors.New("cluster is not initialized")
	ErrAlreadyInitialized = errors.New("cluster is already initialized")
	ErrUnrecognized       = errors.New("database is not recognized as orch state")
)

// Store owns the single-controller SQLite connection pool.
type Store struct {
	db        *sql.DB
	path      string
	readOnly  bool
	closeOnce sync.Once
	closeErr  error
}

// Open creates or opens a controller database, configures WAL durability, and
// applies all embedded migrations in one transaction.
func Open(ctx context.Context, databasePath string) (*Store, error) {
	if strings.TrimSpace(databasePath) == "" {
		return nil, fmt.Errorf("database path is required")
	}
	absolute, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	created, err := prepareDatabaseFile(absolute)
	if err != nil {
		return nil, err
	}

	dsn := (&url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(absolute),
		RawQuery: "_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)",
	}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	closeOnError := func(openErr error) (*Store, error) {
		_ = db.Close()
		if created {
			for _, suffix := range []string{"-shm", "-wal", ""} {
				_ = os.Remove(absolute + suffix)
			}
		}
		return nil, openErr
	}
	if err := db.PingContext(ctx); err != nil {
		return closeOnError(fmt.Errorf("connect to database: %w", err))
	}
	if err := applyMigrations(ctx, db, embeddedMigrations, time.Now()); err != nil {
		return closeOnError(err)
	}
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		return closeOnError(fmt.Errorf("read journal mode: %w", err))
	}
	if !strings.EqualFold(journalMode, "wal") {
		return closeOnError(fmt.Errorf("database journal mode is %q, want WAL", journalMode))
	}
	var foreignKeys int
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return closeOnError(fmt.Errorf("read foreign key mode: %w", err))
	}
	if foreignKeys != 1 {
		return closeOnError(fmt.Errorf("database foreign keys are disabled"))
	}
	if err := os.Chmod(absolute, 0o600); err != nil {
		return closeOnError(fmt.Errorf("protect database file: %w", err))
	}
	return &Store{db: db, path: absolute}, nil
}

func prepareDatabaseFile(path string) (bool, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if closeErr := file.Close(); closeErr != nil {
			_ = os.Remove(path)
			return false, fmt.Errorf("close new database file: %w", closeErr)
		}
		return true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return false, fmt.Errorf("create database file: %w", err)
	}
	info, statErr := os.Lstat(path)
	if statErr != nil {
		return false, fmt.Errorf("inspect database file: %w", statErr)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("database path is not a regular file: %s", path)
	}
	return false, nil
}

// OpenExisting validates that a path is already an orch database before any
// writable connection is opened, then applies compatible forward migrations.
func OpenExisting(ctx context.Context, databasePath string) (*Store, error) {
	if strings.TrimSpace(databasePath) == "" {
		return nil, fmt.Errorf("database path is required")
	}
	absolute, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, fmt.Errorf("inspect existing database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrUnrecognized, absolute)
	}
	if err := validateExistingDatabase(ctx, absolute); err != nil {
		return nil, err
	}
	return Open(ctx, absolute)
}

// OpenInitialized validates an existing orch database and confirms that its
// singleton cluster can be read before opening the database for writes.
func OpenInitialized(ctx context.Context, databasePath string) (*Store, error) {
	if strings.TrimSpace(databasePath) == "" {
		return nil, fmt.Errorf("database path is required")
	}
	absolute, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, fmt.Errorf("inspect existing database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrUnrecognized, absolute)
	}
	if err := validateExistingDatabase(ctx, absolute); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute), RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("inspect initialized cluster: %w", err)
	}
	_, readErr := scanCluster(db.QueryRowContext(ctx, `SELECT id, name, endpoint, generation, created_at,
		workload_cidr, network_generation, network_frozen_at
		FROM clusters WHERE singleton = 1`))
	closeErr := db.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close cluster inspection database: %w", closeErr)
	}
	return Open(ctx, absolute)
}

// OpenReadOnly opens an existing orch database without applying migrations or
// checkpointing on close, so a running controller's schema is left untouched.
// Databases with a schema newer than this binary are refused.
func OpenReadOnly(ctx context.Context, databasePath string) (*Store, error) {
	if strings.TrimSpace(databasePath) == "" {
		return nil, fmt.Errorf("database path is required")
	}
	absolute, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, fmt.Errorf("inspect existing database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrUnrecognized, absolute)
	}
	if err := validateExistingDatabase(ctx, absolute); err != nil {
		return nil, err
	}
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(absolute),
		RawQuery: "mode=ro&_pragma=busy_timeout(5000)",
	}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return &Store{db: db, path: absolute, readOnly: true}, nil
}

func validateExistingDatabase(ctx context.Context, databasePath string) error {
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(databasePath), RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("%w: open database for validation: %v", ErrUnrecognized, err)
	}
	defer db.Close()
	var tableCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&tableCount); err != nil {
		return fmt.Errorf("%w: inspect migration table: %v", ErrUnrecognized, err)
	}
	if tableCount != 1 {
		return fmt.Errorf("%w: migration table is missing", ErrUnrecognized)
	}
	migrations, err := loadMigrations(embeddedMigrations)
	if err != nil {
		return err
	}
	applied, err := appliedMigrations(ctx, db)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnrecognized, err)
	}
	if len(applied) == 0 {
		return fmt.Errorf("%w: migration history is empty", ErrUnrecognized)
	}
	if err := validateMigrationHistory(migrations, applied); err != nil {
		return err
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if !s.readOnly {
			if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				s.closeErr = fmt.Errorf("checkpoint database: %w", err)
			}
		}
		if err := s.db.Close(); err != nil && s.closeErr == nil {
			s.closeErr = fmt.Errorf("close database: %w", err)
		}
	})
	return s.closeErr
}

func (s *Store) Path() string {
	return s.path
}

// InitializeCluster creates the singleton cluster and its first event in one
// transaction. If event recording fails, no cluster row is committed.
func (s *Store) InitializeCluster(ctx context.Context, cluster model.Cluster) error {
	if cluster.ID == "" || cluster.Name == "" || cluster.Endpoint == "" {
		return fmt.Errorf("cluster id, name, and endpoint are required")
	}
	if cluster.Generation == 0 {
		return fmt.Errorf("cluster generation must be positive")
	}
	if cluster.CreatedAt.IsZero() {
		return fmt.Errorf("cluster creation time is required")
	}
	if cluster.WorkloadCIDR == "" {
		cluster.WorkloadCIDR = model.DefaultWorkloadCIDR
	}
	if _, err := model.ParseWorkloadCIDR(cluster.WorkloadCIDR); err != nil {
		return err
	}
	if cluster.NetworkGeneration == 0 {
		cluster.NetworkGeneration = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin cluster initialization: %w", err)
	}
	defer tx.Rollback()

	var existing string
	err = tx.QueryRowContext(ctx, "SELECT id FROM clusters WHERE singleton = 1").Scan(&existing)
	if err == nil {
		return fmt.Errorf("%w: %s", ErrAlreadyInitialized, existing)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check cluster initialization: %w", err)
	}
	createdAt := cluster.CreatedAt.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO clusters
	        (singleton, id, name, endpoint, generation, created_at, workload_cidr, network_generation, network_updated_at)
	        VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cluster.ID, cluster.Name, cluster.Endpoint, cluster.Generation, createdAt,
		cluster.WorkloadCIDR, cluster.NetworkGeneration, createdAt,
	); err != nil {
		return fmt.Errorf("insert cluster: %w", err)
	}
	payload, err := json.Marshal(struct {
		Name     string `json:"name"`
		Endpoint string `json:"endpoint"`
	}{Name: cluster.Name, Endpoint: cluster.Endpoint})
	if err != nil {
		return fmt.Errorf("encode cluster event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events
        (cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
		cluster.ID, model.EventClusterInitialized, string(payload), createdAt,
	); err != nil {
		return fmt.Errorf("record cluster initialization: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit cluster initialization: %w", err)
	}
	return nil
}

func (s *Store) Cluster(ctx context.Context) (model.Cluster, error) {
	return scanCluster(s.db.QueryRowContext(ctx, `SELECT id, name, endpoint, generation, created_at,
		workload_cidr, network_generation, network_frozen_at
		FROM clusters WHERE singleton = 1`))
}

type rowScanner interface {
	Scan(...any) error
}

func scanCluster(row rowScanner) (model.Cluster, error) {
	var cluster model.Cluster
	var generation, networkGeneration int64
	var createdAt string
	var frozenAt sql.NullString
	err := row.Scan(
		&cluster.ID, &cluster.Name, &cluster.Endpoint, &generation, &createdAt,
		&cluster.WorkloadCIDR, &networkGeneration, &frozenAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Cluster{}, ErrNotInitialized
	}
	if err != nil {
		return model.Cluster{}, fmt.Errorf("read cluster: %w", err)
	}
	if generation < 1 || networkGeneration < 1 {
		return model.Cluster{}, fmt.Errorf("read cluster: invalid generation %d", generation)
	}
	cluster.Generation = uint64(generation)
	cluster.NetworkGeneration = uint64(networkGeneration)
	cluster.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return model.Cluster{}, fmt.Errorf("read cluster creation time: %w", err)
	}
	if frozenAt.Valid {
		stamp, err := time.Parse(time.RFC3339Nano, frozenAt.String)
		if err != nil {
			return model.Cluster{}, fmt.Errorf("read network freeze time: %w", err)
		}
		cluster.NetworkFrozenAt = &stamp
	}
	return cluster, nil
}

func (s *Store) Events(ctx context.Context, limit int) ([]model.Event, error) {
	return s.EventsAfter(ctx, 0, limit)
}

// EventsAfter returns events strictly after a durable sequence cursor.
func (s *Store) EventsAfter(ctx context.Context, after int64, limit int) ([]model.Event, error) {
	if after < 0 {
		return nil, fmt.Errorf("event cursor must not be negative")
	}
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("event limit must be between 1 and 1000")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence, cluster_id, kind, payload, created_at
		FROM events WHERE sequence > ? ORDER BY sequence ASC LIMIT ?`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer rows.Close()
	events := make([]model.Event, 0)
	for rows.Next() {
		var event model.Event
		var payload, createdAt string
		if err := rows.Scan(&event.Sequence, &event.ClusterID, &event.Kind, &payload, &createdAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		event.Payload = json.RawMessage(payload)
		event.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("read event creation time: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return events, nil
}

// RecentEvents returns the newest bounded event window in chronological order.
func (s *Store) RecentEvents(ctx context.Context, limit int) ([]model.Event, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("event limit must be between 1 and 1000")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence, cluster_id, kind, payload, created_at
		FROM events ORDER BY sequence DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("read recent events: %w", err)
	}
	defer rows.Close()
	events := make([]model.Event, 0, limit)
	for rows.Next() {
		var event model.Event
		var payload, createdAt string
		if err := rows.Scan(&event.Sequence, &event.ClusterID, &event.Kind, &payload, &createdAt); err != nil {
			return nil, fmt.Errorf("scan recent event: %w", err)
		}
		event.Payload = json.RawMessage(payload)
		event.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("read recent event creation time: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent events: %w", err)
	}
	for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
		events[left], events[right] = events[right], events[left]
	}
	return events, nil
}

type sqliteBackuper interface {
	NewBackup(string) (*modernsqlite.Backup, error)
}

// Backup writes a consistent online backup and refuses to replace an existing
// destination. A hard link makes publication atomic and no-clobber.
func (s *Store) Backup(ctx context.Context, destination string) (finalErr error) {
	return s.backup(ctx, destination, verifyBackup)
}

func (s *Store) backup(ctx context.Context, destination string, verify func(context.Context, string) error) (finalErr error) {
	if strings.TrimSpace(destination) == "" {
		return fmt.Errorf("backup destination is required")
	}
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("resolve backup destination: %w", err)
	}
	if absolute == s.path {
		return fmt.Errorf("backup destination must differ from the live database")
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return fmt.Errorf("create backup directory: %w", err)
	}
	if _, err := os.Stat(absolute); err == nil {
		return fmt.Errorf("backup destination already exists: %s", absolute)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect backup destination: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(absolute), ".orch-backup-*.db")
	if err != nil {
		return fmt.Errorf("create backup staging file: %w", err)
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("close backup staging file: %w", err)
	}
	defer func() {
		for _, suffix := range []string{"-shm", "-wal", "-journal", ""} {
			_ = os.Remove(temporaryPath + suffix)
		}
	}()

	connection, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve backup connection: %w", err)
	}
	defer connection.Close()
	if err := connection.Raw(func(driverConnection any) error {
		backuper, ok := driverConnection.(sqliteBackuper)
		if !ok {
			return fmt.Errorf("SQLite driver does not support online backup")
		}
		backup, err := backuper.NewBackup(temporaryPath)
		if err != nil {
			return err
		}
		more := true
		for more {
			if err := ctx.Err(); err != nil {
				_ = backup.Finish()
				return err
			}
			more, err = backup.Step(128)
			if err != nil {
				_ = backup.Finish()
				return err
			}
		}
		return backup.Finish()
	}); err != nil {
		return fmt.Errorf("back up database: %w", err)
	}
	if err := verify(ctx, temporaryPath); err != nil {
		return err
	}
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		return fmt.Errorf("protect backup: %w", err)
	}
	file, err := os.OpenFile(temporaryPath, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open backup for sync: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync backup: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close backup after sync: %w", err)
	}
	if err := os.Link(temporaryPath, absolute); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("backup destination already exists: %s", absolute)
		}
		return fmt.Errorf("publish backup: %w", err)
	}
	if err := syncDirectory(filepath.Dir(absolute)); err != nil {
		_ = os.Remove(absolute)
		return fmt.Errorf("sync backup directory: %w", err)
	}
	return nil
}

func syncDirectory(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func verifyBackup(ctx context.Context, databasePath string) error {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.ToSlash(databasePath), RawQuery: "mode=ro"}).String())
	if err != nil {
		return fmt.Errorf("open backup for verification: %w", err)
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return fmt.Errorf("verify backup integrity: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("verify backup integrity: %s", integrity)
	}
	rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("verify backup foreign keys: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("verify backup foreign keys: violation found")
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("verify backup foreign keys: %w", err)
	}
	return nil
}
