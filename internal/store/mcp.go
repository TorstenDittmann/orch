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

// maxMCPGrants bounds how many remote MCP connections a cluster keeps.
const maxMCPGrants = 100

// MCPDomain returns the remote MCP server's domain, or "" when it is off.
func (s *Store) MCPDomain(ctx context.Context) (string, error) {
	var domain string
	err := s.db.QueryRowContext(ctx, `SELECT domain FROM mcp_settings WHERE id = 1`).Scan(&domain)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read MCP domain: %w", err)
	}
	return domain, nil
}

// SetMCPDomain stores the remote MCP server's domain; "" turns it off. It
// reports whether the domain changed.
func (s *Store) SetMCPDomain(ctx context.Context, domain string) (bool, error) {
	current, err := s.MCPDomain(ctx)
	if err != nil || current == domain {
		return false, err
	}
	if domain == "" {
		_, err = s.db.ExecContext(ctx, `DELETE FROM mcp_settings WHERE id = 1`)
	} else {
		_, err = s.db.ExecContext(ctx, `INSERT INTO mcp_settings (id, domain) VALUES (1, ?)
			ON CONFLICT (id) DO UPDATE SET domain = excluded.domain`, domain)
	}
	if err != nil {
		return false, fmt.Errorf("store MCP domain: %w", err)
	}
	return true, nil
}

// CreateMCPGrant stores a new approved connection. Names are unique among
// connections and access tokens, since orch token revoke takes either.
func (s *Store) CreateMCPGrant(ctx context.Context, grant model.MCPGrant) error {
	switch grant.Access {
	case model.MCPAccessRead, model.MCPAccessDeploy, model.MCPAccessDestructive:
	default:
		return fmt.Errorf("MCP connection access is invalid")
	}
	if grant.ID == "" || grant.Name == "" || grant.ClientID == "" || grant.CreatedAt.IsZero() || !grant.ExpiresAt.After(grant.CreatedAt) {
		return fmt.Errorf("MCP connection data is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin MCP connection creation: %w", err)
	}
	defer tx.Rollback()
	var count, taken int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mcp_grants`).Scan(&count); err != nil {
		return fmt.Errorf("count MCP connections: %w", err)
	}
	if count >= maxMCPGrants {
		return fmt.Errorf("a cluster keeps at most %d MCP connections; revoke unused ones with orch token revoke", maxMCPGrants)
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM access_tokens WHERE name = ?`, grant.Name).Scan(&taken); err != nil {
		return fmt.Errorf("check MCP connection name: %w", err)
	}
	if taken != 0 {
		return ErrAccessTokenExists
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_grants (id, name, client_name, client_id, application_name, access, refresh_hash, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, grant.ID, grant.Name, grant.ClientName, grant.ClientID, grant.Application, grant.Access,
		grant.RefreshHash, grant.CreatedAt.UTC().Format(time.RFC3339Nano), grant.ExpiresAt.UTC().Format(time.RFC3339Nano)); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrAccessTokenExists
		}
		return fmt.Errorf("store MCP connection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit MCP connection: %w", err)
	}
	return nil
}

// MCPGrant returns one connection by ID; sql.ErrNoRows when it does not exist.
func (s *Store) MCPGrant(ctx context.Context, id string) (model.MCPGrant, error) {
	return scanMCPGrant(s.db.QueryRowContext(ctx, mcpGrantSelect+` WHERE id = ?`, id))
}

// MCPGrants lists every connection by name.
func (s *Store) MCPGrants(ctx context.Context) ([]model.MCPGrant, error) {
	rows, err := s.db.QueryContext(ctx, mcpGrantSelect+` ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list MCP connections: %w", err)
	}
	defer rows.Close()
	var result []model.MCPGrant
	for rows.Next() {
		grant, err := scanMCPGrant(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, grant)
	}
	return result, rows.Err()
}

// RotateMCPRefresh replaces a connection's refresh token hash, but only while
// it is still previous, so a refresh token works once even under races.
func (s *Store) RotateMCPRefresh(ctx context.Context, id, previous, next string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE mcp_grants SET refresh_hash = ? WHERE id = ? AND refresh_hash = ?`, next, id, previous)
	if err != nil {
		return false, fmt.Errorf("rotate MCP refresh token: %w", err)
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

// TouchMCPGrant records when a connection was last used.
func (s *Store) TouchMCPGrant(ctx context.Context, id string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE mcp_grants SET last_used_at = ? WHERE id = ?`, at.UTC().Format(time.RFC3339Nano), id); err != nil {
		return fmt.Errorf("record MCP connection use: %w", err)
	}
	return nil
}

// DeleteMCPGrant removes a connection by name and reports whether it existed.
func (s *Store) DeleteMCPGrant(ctx context.Context, name string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM mcp_grants WHERE name = ?`, name)
	if err != nil {
		return false, fmt.Errorf("delete MCP connection: %w", err)
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

// DeleteExpiredMCPGrants removes connections that expired before now.
func (s *Store) DeleteExpiredMCPGrants(ctx context.Context, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM mcp_grants WHERE expires_at < ?`, now.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("delete expired MCP connections: %w", err)
	}
	return nil
}

const mcpGrantSelect = `SELECT id, name, client_name, client_id, application_name, access, refresh_hash, created_at, expires_at, last_used_at FROM mcp_grants`

func scanMCPGrant(row applicationScanner) (model.MCPGrant, error) {
	var grant model.MCPGrant
	var created, expires string
	var used sql.NullString
	if err := row.Scan(&grant.ID, &grant.Name, &grant.ClientName, &grant.ClientID, &grant.Application, &grant.Access,
		&grant.RefreshHash, &created, &expires, &used); err != nil {
		return grant, err
	}
	var err error
	if grant.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return grant, err
	}
	if grant.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires); err != nil {
		return grant, err
	}
	if used.Valid {
		if grant.LastUsedAt, err = time.Parse(time.RFC3339Nano, used.String); err != nil {
			return grant, err
		}
	}
	return grant, nil
}
