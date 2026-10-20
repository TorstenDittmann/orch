package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// NotificationCursor returns the sequence of the last event the notifier
// handled. A store that has never notified starts at its newest event, so
// history from before notifications existed is not sent.
func (s *Store) NotificationCursor(ctx context.Context) (int64, error) {
	var sequence int64
	err := s.db.QueryRowContext(ctx, `SELECT sequence FROM notification_cursor WHERE singleton = 1`).Scan(&sequence)
	if err == nil {
		return sequence, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("read notification cursor: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) FROM events`).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("read newest event: %w", err)
	}
	if err := s.SetNotificationCursor(ctx, sequence); err != nil {
		return 0, err
	}
	return sequence, nil
}

// SetNotificationCursor records that every event up to sequence is handled.
func (s *Store) SetNotificationCursor(ctx context.Context, sequence int64) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO notification_cursor (singleton, sequence) VALUES (1, ?)
		ON CONFLICT (singleton) DO UPDATE SET sequence = excluded.sequence`, sequence); err != nil {
		return fmt.Errorf("write notification cursor: %w", err)
	}
	return nil
}
