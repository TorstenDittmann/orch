package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

const migrationTableSQL = `CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    checksum TEXT NOT NULL,
    applied_at TEXT NOT NULL
) STRICT`

type migration struct {
	version  int
	filename string
	checksum string
	sql      string
}

type appliedMigration struct {
	name     string
	checksum string
}

func applyMigrations(ctx context.Context, db *sql.DB, source fs.FS, now time.Time) error {
	migrations, err := loadMigrations(source)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, migrationTableSQL); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}
	applied, err := appliedMigrations(ctx, tx)
	if err != nil {
		return err
	}
	if err := validateMigrationHistory(migrations, applied); err != nil {
		return err
	}

	stamp := now.UTC().Format(time.RFC3339Nano)
	for _, item := range migrations {
		if _, ok := applied[item.version]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, item.sql); err != nil {
			return fmt.Errorf("apply migration %s: %w", item.filename, err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES (?, ?, ?, ?)",
			item.version, item.filename, item.checksum, stamp,
		); err != nil {
			return fmt.Errorf("record migration %s: %w", item.filename, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	return nil
}

func validateMigrationHistory(migrations []migration, applied map[int]appliedMigration) error {
	known := make(map[int]migration, len(migrations))
	for _, item := range migrations {
		known[item.version] = item
	}
	for version, record := range applied {
		item, ok := known[version]
		if !ok {
			return fmt.Errorf("database schema version %d is newer than this binary", version)
		}
		if record.name != item.filename {
			return fmt.Errorf("migration %03d filename changed from %q to %q", item.version, record.name, item.filename)
		}
		if record.checksum != item.checksum {
			return fmt.Errorf("migration %s checksum changed after it was applied", item.filename)
		}
	}
	for version := 1; version <= len(applied); version++ {
		if _, ok := applied[version]; !ok {
			return fmt.Errorf("database migration history is missing version %d", version)
		}
	}
	return nil
}

func loadMigrations(source fs.FS) ([]migration, error) {
	names, err := fs.Glob(source, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no embedded migrations")
	}
	sort.Strings(names)
	result := make([]migration, 0, len(names))
	for index, filename := range names {
		base := strings.TrimSuffix(path.Base(filename), ".sql")
		parts := strings.SplitN(base, "_", 2)
		if len(parts) != 2 || parts[1] == "" {
			return nil, fmt.Errorf("invalid migration filename %q", filename)
		}
		version, err := strconv.Atoi(parts[0])
		if err != nil || version != index+1 || parts[0] != fmt.Sprintf("%03d", index+1) {
			return nil, fmt.Errorf("migration %q must have contiguous version %03d", filename, index+1)
		}
		contents, err := fs.ReadFile(source, filename)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", filename, err)
		}
		digest := sha256.Sum256(contents)
		result = append(result, migration{
			version:  version,
			filename: path.Base(filename),
			checksum: hex.EncodeToString(digest[:]),
			sql:      string(contents),
		})
	}
	return result, nil
}

type migrationQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func appliedMigrations(ctx context.Context, query migrationQuerier) (map[int]appliedMigration, error) {
	rows, err := query.QueryContext(ctx, "SELECT version, name, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()
	result := make(map[int]appliedMigration)
	for rows.Next() {
		var version int
		var record appliedMigration
		if err := rows.Scan(&version, &record.name, &record.checksum); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		result[version] = record
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migrations: %w", err)
	}
	return result, nil
}
