package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/objectstore"
	"github.com/TorstenDittmann/orch/internal/security"
)

// SetBackupDestination stores the bucket backups are copied to, replacing
// any previous one.
func (s *Store) SetBackupDestination(ctx context.Context, box *security.SecretBox, destination objectstore.Destination, now time.Time) error {
	if box == nil || destination.Bucket == "" || destination.Region == "" || destination.AccessKeyID == "" ||
		destination.SecretAccessKey == "" || now.IsZero() {
		return fmt.Errorf("backup destination input is invalid")
	}
	aad, err := s.controllerSecretAAD(ctx, "backup_destination", destination.AccessKeyID, "")
	if err != nil {
		return err
	}
	encrypted, err := box.Seal(aad, []byte(destination.SecretAccessKey))
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO backup_destination
		(singleton, bucket, prefix, endpoint, region, access_key_id, algorithm, nonce, ciphertext, wrapped_key_nonce, wrapped_key, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (singleton) DO UPDATE SET bucket = excluded.bucket, prefix = excluded.prefix, endpoint = excluded.endpoint,
		region = excluded.region, access_key_id = excluded.access_key_id, algorithm = excluded.algorithm, nonce = excluded.nonce,
		ciphertext = excluded.ciphertext, wrapped_key_nonce = excluded.wrapped_key_nonce, wrapped_key = excluded.wrapped_key,
		updated_at = excluded.updated_at`,
		destination.Bucket, destination.Prefix, destination.Endpoint, destination.Region, destination.AccessKeyID,
		encrypted.Algorithm, encrypted.Nonce, encrypted.Ciphertext, encrypted.WrappedKeyNonce, encrypted.WrappedKey,
		now.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("store backup destination: %w", err)
	}
	return nil
}

// BackupDestination returns the stored destination with its secret, or
// false when none is set.
func (s *Store) BackupDestination(ctx context.Context, box *security.SecretBox) (objectstore.Destination, bool, error) {
	var destination objectstore.Destination
	var encrypted security.EncryptedValue
	err := s.db.QueryRowContext(ctx, `SELECT bucket, prefix, endpoint, region, access_key_id, algorithm, nonce, ciphertext,
		wrapped_key_nonce, wrapped_key FROM backup_destination WHERE singleton = 1`).Scan(&destination.Bucket, &destination.Prefix,
		&destination.Endpoint, &destination.Region, &destination.AccessKeyID, &encrypted.Algorithm, &encrypted.Nonce,
		&encrypted.Ciphertext, &encrypted.WrappedKeyNonce, &encrypted.WrappedKey)
	if errors.Is(err, sql.ErrNoRows) {
		return objectstore.Destination{}, false, nil
	}
	if err != nil {
		return objectstore.Destination{}, false, fmt.Errorf("read backup destination: %w", err)
	}
	if box == nil {
		return objectstore.Destination{}, false, fmt.Errorf("backup destination needs the controller secret key")
	}
	aad, err := s.controllerSecretAAD(ctx, "backup_destination", destination.AccessKeyID, "")
	if err != nil {
		return objectstore.Destination{}, false, err
	}
	secret, err := box.Open(aad, encrypted)
	if err != nil {
		return objectstore.Destination{}, false, fmt.Errorf("decrypt backup destination: %w", err)
	}
	destination.SecretAccessKey = string(secret)
	clear(secret)
	return destination, true, nil
}

// DeleteBackupDestination forgets the destination and reports whether one
// was set. Objects already in the bucket stay there.
func (s *Store) DeleteBackupDestination(ctx context.Context) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM backup_destination WHERE singleton = 1`)
	if err != nil {
		return false, fmt.Errorf("delete backup destination: %w", err)
	}
	count, err := result.RowsAffected()
	return count != 0, err
}

// RecordVolumeBackupUploadFailed raises a volume.backup.failed event for a
// backup that could not be copied to the bucket, so the application's
// backup.failed notifications cover it.
func (s *Store) RecordVolumeBackupUploadFailed(ctx context.Context, application, volume, nodeID, message string, at time.Time) error {
	cluster, err := s.Cluster(ctx)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(model.VolumeBackupFailed{Application: application, Volume: volume, NodeID: nodeID,
		FailedAt: at.UTC(), Message: message})
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO events (cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
		cluster.ID, model.EventVolumeBackupFailed, string(payload), at.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("record backup upload failure: %w", err)
	}
	return nil
}
