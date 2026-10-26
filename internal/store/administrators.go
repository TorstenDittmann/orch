package store

import (
	"context"
	"fmt"
	"time"
)

// SetAdministratorNotBefore makes the controller refuse administrator
// certificates that became valid before cutoff, from its next start.
func (s *Store) SetAdministratorNotBefore(ctx context.Context, cutoff time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE clusters SET administrator_not_before = ? WHERE singleton = 1`, cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("store administrator certificate cutoff: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return ErrNotInitialized
	}
	return nil
}

// AdministratorNotBefore returns the cutoff, or the zero time if none is set.
func (s *Store) AdministratorNotBefore(ctx context.Context) (time.Time, error) {
	var value string
	if err := s.db.QueryRowContext(ctx, `SELECT administrator_not_before FROM clusters WHERE singleton = 1`).Scan(&value); err != nil {
		return time.Time{}, fmt.Errorf("read administrator certificate cutoff: %w", err)
	}
	if value == "" {
		return time.Time{}, nil
	}
	cutoff, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("read administrator certificate cutoff: %w", err)
	}
	return cutoff, nil
}
