package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/security"
)

var ErrClusterNotificationNotFound = errors.New("notification was not found")

// SetClusterNotification creates or replaces the notification with this
// name, encrypting its URL.
func (s *Store) SetClusterNotification(ctx context.Context, box *security.SecretBox, notification model.ClusterNotification, url []byte) (model.ClusterNotification, error) {
	if box == nil || notification.Name == "" || notification.Kind == "" || len(notification.Events) == 0 ||
		len(url) == 0 || len(url) > 4096 || notification.CreatedAt.IsZero() {
		return model.ClusterNotification{}, fmt.Errorf("notification input is invalid")
	}
	aad, err := s.controllerSecretAAD(ctx, "cluster_notification", notification.Name)
	if err != nil {
		return model.ClusterNotification{}, err
	}
	encrypted, err := box.Seal(aad, url)
	if err != nil {
		return model.ClusterNotification{}, err
	}
	events, _ := json.Marshal(notification.Events)
	notification.CreatedAt = notification.CreatedAt.UTC()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO cluster_notifications
		(name, kind, events, algorithm, nonce, ciphertext, wrapped_key_nonce, wrapped_key, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (name) DO UPDATE SET kind = excluded.kind, events = excluded.events,
		algorithm = excluded.algorithm, nonce = excluded.nonce, ciphertext = excluded.ciphertext,
		wrapped_key_nonce = excluded.wrapped_key_nonce, wrapped_key = excluded.wrapped_key, created_at = excluded.created_at`,
		notification.Name, notification.Kind, string(events), encrypted.Algorithm, encrypted.Nonce, encrypted.Ciphertext,
		encrypted.WrappedKeyNonce, encrypted.WrappedKey, notification.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		return model.ClusterNotification{}, fmt.Errorf("store notification: %w", err)
	}
	return notification, nil
}

// ClusterNotifications lists every cluster notification by name.
func (s *Store) ClusterNotifications(ctx context.Context) ([]model.ClusterNotification, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, kind, events, created_at FROM cluster_notifications ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	defer rows.Close()
	var result []model.ClusterNotification
	for rows.Next() {
		var notification model.ClusterNotification
		var events, created string
		if err := rows.Scan(&notification.Name, &notification.Kind, &events, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(events), &notification.Events); err != nil {
			return nil, fmt.Errorf("decode notification events: %w", err)
		}
		if notification.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, err
		}
		result = append(result, notification)
	}
	return result, rows.Err()
}

// ResolveClusterNotificationURL decrypts the URL of one notification.
func (s *Store) ResolveClusterNotificationURL(ctx context.Context, box *security.SecretBox, name string) ([]byte, error) {
	var value security.EncryptedValue
	err := s.db.QueryRowContext(ctx, `SELECT algorithm, nonce, ciphertext, wrapped_key_nonce, wrapped_key
		FROM cluster_notifications WHERE name = ?`, name).
		Scan(&value.Algorithm, &value.Nonce, &value.Ciphertext, &value.WrappedKeyNonce, &value.WrappedKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrClusterNotificationNotFound
	}
	if err != nil {
		return nil, err
	}
	aad, err := s.controllerSecretAAD(ctx, "cluster_notification", name)
	if err != nil {
		return nil, err
	}
	return box.Open(aad, value)
}

// DeleteClusterNotification removes one notification and reports whether it
// existed.
func (s *Store) DeleteClusterNotification(ctx context.Context, name string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM cluster_notifications WHERE name = ?`, name)
	if err != nil {
		return false, fmt.Errorf("delete notification: %w", err)
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

// RaiseClusterAlert records the problem key and appends an event of kind
// for it, unless the problem is already raised. It reports whether it
// appended the event.
func (s *Store) RaiseClusterAlert(ctx context.Context, key, kind string, alert model.ClusterAlert, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(time.RFC3339Nano)
	inserted, err := tx.ExecContext(ctx, `INSERT INTO cluster_alerts (key, raised_at) VALUES (?, ?) ON CONFLICT (key) DO NOTHING`, key, stamp)
	if err != nil {
		return false, fmt.Errorf("record cluster alert: %w", err)
	}
	if count, _ := inserted.RowsAffected(); count == 0 {
		return false, nil
	}
	var clusterID string
	if err := tx.QueryRowContext(ctx, "SELECT id FROM clusters WHERE singleton = 1").Scan(&clusterID); err != nil {
		return false, err
	}
	payload, _ := json.Marshal(alert)
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`, clusterID, kind, string(payload), stamp); err != nil {
		return false, fmt.Errorf("append cluster alert event: %w", err)
	}
	return true, tx.Commit()
}

// ClusterAlerts lists the keys of every raised problem.
func (s *Store) ClusterAlerts(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key FROM cluster_alerts ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("list cluster alerts: %w", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// ClearClusterAlert forgets a problem that went away, so it is reported
// again if it comes back.
func (s *Store) ClearClusterAlert(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM cluster_alerts WHERE key = ?`, key); err != nil {
		return fmt.Errorf("clear cluster alert: %w", err)
	}
	return nil
}
