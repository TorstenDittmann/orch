package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/TorstenDittmann/orch/internal/security"
)

// VolumeBackupKey returns the key pair volume backups in the bucket are
// encrypted to, creating it on first use. Nodes only ever get the public
// half, plus the key of a single backup when they restore it.
func (s *Store) VolumeBackupKey(ctx context.Context, box *security.SecretBox, now time.Time) (private, public string, err error) {
	if box == nil {
		return "", "", errors.New("volume backup key needs the controller secret key")
	}
	private, public, err = s.readVolumeBackupKey(ctx, box)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return private, public, err
	}
	private, public, err = security.NewRecoveryKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	aad, err := s.controllerSecretAAD(ctx, "volume_backup_key", public, "")
	if err != nil {
		return "", "", err
	}
	encrypted, err := box.Seal(aad, []byte(private))
	if err != nil {
		return "", "", err
	}
	// Two callers may race to create it; the first one wins and both read it.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO volume_backup_key
		(singleton, public_key, algorithm, nonce, ciphertext, wrapped_key_nonce, wrapped_key, created_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (singleton) DO NOTHING`,
		public, encrypted.Algorithm, encrypted.Nonce, encrypted.Ciphertext, encrypted.WrappedKeyNonce, encrypted.WrappedKey,
		now.UTC().Format(time.RFC3339Nano)); err != nil {
		return "", "", fmt.Errorf("store volume backup key: %w", err)
	}
	return s.readVolumeBackupKey(ctx, box)
}

func (s *Store) readVolumeBackupKey(ctx context.Context, box *security.SecretBox) (string, string, error) {
	var public string
	var encrypted security.EncryptedValue
	err := s.db.QueryRowContext(ctx, `SELECT public_key, algorithm, nonce, ciphertext, wrapped_key_nonce, wrapped_key
		FROM volume_backup_key WHERE singleton = 1`).Scan(&public, &encrypted.Algorithm, &encrypted.Nonce, &encrypted.Ciphertext,
		&encrypted.WrappedKeyNonce, &encrypted.WrappedKey)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	if err != nil {
		return "", "", fmt.Errorf("read volume backup key: %w", err)
	}
	aad, err := s.controllerSecretAAD(ctx, "volume_backup_key", public, "")
	if err != nil {
		return "", "", err
	}
	private, err := box.Open(aad, encrypted)
	if err != nil {
		return "", "", fmt.Errorf("decrypt volume backup key: %w", err)
	}
	defer clear(private)
	return string(private), public, nil
}
