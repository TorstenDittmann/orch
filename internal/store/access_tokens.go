package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

// ErrAccessTokenExists reports that a token with the same name exists.
var ErrAccessTokenExists = errors.New("an access token with this name already exists")

// maxAccessTokens bounds how many tokens a cluster keeps.
const maxAccessTokens = 200

// CreateAccessToken stores a new token. Names are unique.
func (s *Store) CreateAccessToken(ctx context.Context, token model.AccessToken) error {
	if token.ID == "" || token.Name == "" || token.Application == "" || len(token.SecretHash) != 64 ||
		token.CreatedAt.IsZero() || !token.ExpiresAt.After(token.CreatedAt) {
		return fmt.Errorf("access token data is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin access token creation: %w", err)
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM access_tokens`).Scan(&count); err != nil {
		return fmt.Errorf("count access tokens: %w", err)
	}
	if count >= maxAccessTokens {
		return fmt.Errorf("a cluster keeps at most %d access tokens; delete unused ones first", maxAccessTokens)
	}
	readOnly := 0
	if token.ReadOnly {
		readOnly = 1
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO access_tokens (id, name, application_name, read_only, secret_hash, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, token.ID, token.Name, token.Application, readOnly, token.SecretHash,
		token.CreatedAt.UTC().Format(time.RFC3339Nano), token.ExpiresAt.UTC().Format(time.RFC3339Nano)); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrAccessTokenExists
		}
		return fmt.Errorf("store access token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit access token: %w", err)
	}
	return nil
}

// AccessToken returns one token by ID; sql.ErrNoRows when it does not exist.
func (s *Store) AccessToken(ctx context.Context, id string) (model.AccessToken, error) {
	return scanAccessToken(s.db.QueryRowContext(ctx, accessTokenSelect+` WHERE id = ?`, id))
}

// AccessTokens lists every token by name.
func (s *Store) AccessTokens(ctx context.Context) ([]model.AccessToken, error) {
	rows, err := s.db.QueryContext(ctx, accessTokenSelect+` ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list access tokens: %w", err)
	}
	defer rows.Close()
	var result []model.AccessToken
	for rows.Next() {
		token, err := scanAccessToken(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, token)
	}
	return result, rows.Err()
}

// TouchAccessToken records when a token was last exchanged.
func (s *Store) TouchAccessToken(ctx context.Context, id string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE access_tokens SET last_used_at = ? WHERE id = ?`, at.UTC().Format(time.RFC3339Nano), id); err != nil {
		return fmt.Errorf("record access token use: %w", err)
	}
	return nil
}

// DeleteAccessToken removes a token by name and reports whether it existed.
func (s *Store) DeleteAccessToken(ctx context.Context, name string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM access_tokens WHERE name = ?`, name)
	if err != nil {
		return false, fmt.Errorf("delete access token: %w", err)
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

const accessTokenSelect = `SELECT id, name, application_name, read_only, secret_hash, created_at, expires_at, last_used_at FROM access_tokens`

func scanAccessToken(row applicationScanner) (model.AccessToken, error) {
	var token model.AccessToken
	var readOnly int
	var created, expires string
	var used sql.NullString
	if err := row.Scan(&token.ID, &token.Name, &token.Application, &readOnly, &token.SecretHash, &created, &expires, &used); err != nil {
		return token, err
	}
	token.ReadOnly = readOnly == 1
	var err error
	if token.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return token, err
	}
	if token.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires); err != nil {
		return token, err
	}
	if used.Valid {
		if token.LastUsedAt, err = time.Parse(time.RFC3339Nano, used.String); err != nil {
			return token, err
		}
	}
	return token, nil
}
