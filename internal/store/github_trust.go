package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

// SetGitHubTrust creates or replaces the rule for one repository and
// application. It reports whether anything changed.
func (s *Store) SetGitHubTrust(ctx context.Context, trust model.GitHubTrust) (model.GitHubTrust, bool, error) {
	if trust.Repository == "" || trust.Application == "" || trust.Branch == "" || trust.CreatedAt.IsZero() {
		return model.GitHubTrust{}, false, fmt.Errorf("GitHub trust data is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.GitHubTrust{}, false, fmt.Errorf("begin GitHub trust update: %w", err)
	}
	defer tx.Rollback()
	current, err := scanGitHubTrust(tx.QueryRowContext(ctx, githubTrustSelect+` WHERE repository = ? AND application_name = ?`, trust.Repository, trust.Application))
	if err == nil && current.Branch == trust.Branch && current.Environment == trust.Environment {
		return current, false, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return model.GitHubTrust{}, false, err
	}
	trust.CreatedAt = trust.CreatedAt.UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO github_trust (repository, application_name, branch, environment, created_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (repository, application_name)
		DO UPDATE SET branch = excluded.branch, environment = excluded.environment, created_at = excluded.created_at`,
		trust.Repository, trust.Application, trust.Branch, trust.Environment, trust.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		return model.GitHubTrust{}, false, fmt.Errorf("store GitHub trust: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.GitHubTrust{}, false, fmt.Errorf("commit GitHub trust: %w", err)
	}
	return trust, true, nil
}

// GitHubTrusts lists every rule, ordered by repository and application.
func (s *Store) GitHubTrusts(ctx context.Context) ([]model.GitHubTrust, error) {
	rows, err := s.db.QueryContext(ctx, githubTrustSelect+` ORDER BY repository, application_name`)
	if err != nil {
		return nil, fmt.Errorf("list GitHub trust: %w", err)
	}
	defer rows.Close()
	var result []model.GitHubTrust
	for rows.Next() {
		trust, err := scanGitHubTrust(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, trust)
	}
	return result, rows.Err()
}

// DeleteGitHubTrust removes one rule and reports whether it existed.
func (s *Store) DeleteGitHubTrust(ctx context.Context, repository, application string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM github_trust WHERE repository = ? AND application_name = ?`, repository, application)
	if err != nil {
		return false, fmt.Errorf("delete GitHub trust: %w", err)
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

const githubTrustSelect = `SELECT repository, application_name, branch, environment, created_at FROM github_trust`

func scanGitHubTrust(row applicationScanner) (model.GitHubTrust, error) {
	var trust model.GitHubTrust
	var created string
	if err := row.Scan(&trust.Repository, &trust.Application, &trust.Branch, &trust.Environment, &created); err != nil {
		return trust, err
	}
	stamp, err := time.Parse(time.RFC3339Nano, created)
	trust.CreatedAt = stamp
	return trust, err
}
