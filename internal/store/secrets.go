package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/security"
)

const (
	setSecretOperation      = "secret.set"
	deleteSecretOperation   = "secret.delete"
	setRegistryOperation    = "registry.login"
	deleteRegistryOperation = "registry.logout"
)

var (
	ErrSecretNotFound             = errors.New("secret was not found")
	ErrSecretInUse                = errors.New("secret is referenced by current runtime intent")
	ErrRegistryCredentialNotFound = errors.New("registry credential was not found")
	ErrSecretKeyMismatch          = errors.New("secret key does not match encrypted controller state")
)

func (s *Store) EnsureSecretKeyFingerprint(ctx context.Context, fingerprint string, now time.Time) error {
	if len(fingerprint) != 64 || now.IsZero() {
		return fmt.Errorf("secret key fingerprint and creation time are required")
	}
	var stored string
	err := s.db.QueryRowContext(ctx, "SELECT fingerprint FROM secret_key_metadata WHERE singleton = 1").Scan(&stored)
	if err == nil {
		if stored != fingerprint {
			return ErrSecretKeyMismatch
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read secret key metadata: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO secret_key_metadata(singleton, fingerprint, created_at) VALUES (1, ?, ?)`,
		fingerprint, now.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("record secret key metadata: %w", err)
	}
	return nil
}

func (s *Store) SecretKeyFingerprint(ctx context.Context) (string, error) {
	var fingerprint string
	err := s.db.QueryRowContext(ctx, "SELECT fingerprint FROM secret_key_metadata WHERE singleton = 1").Scan(&fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return fingerprint, err
}

// HasEncryptedMaterial reports whether any table sealed with the secret key has
// rows. A read-only store may be on an older schema, as a live controller is
// when an upgrade backs it up, so tables it does not have yet count as empty.
func (s *Store) HasEncryptedMaterial(ctx context.Context) (bool, error) {
	for _, table := range []string{"application_secret_versions", "registry_credential_versions", "backup_destination"} {
		var exists bool
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) > 0 FROM sqlite_schema WHERE type = 'table' AND name = ?", table).Scan(&exists); err != nil {
			return false, fmt.Errorf("count encrypted material: %w", err)
		}
		if !exists {
			continue
		}
		var found bool
		if err := s.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM "+table+")").Scan(&found); err != nil {
			return false, fmt.Errorf("count encrypted material: %w", err)
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) SetApplicationSecret(ctx context.Context, box *security.SecretBox, application, name, versionID string,
	value []byte, idempotencyKey string, now time.Time,
) (model.SecretMutationResult, error) {
	if box == nil || application == "" || name == "" || versionID == "" || len(value) == 0 || len(value) > 1<<20 ||
		len(idempotencyKey) < 16 || len(idempotencyKey) > 128 || now.IsZero() {
		return model.SecretMutationResult{}, fmt.Errorf("secret mutation input is invalid")
	}
	requestHash := box.RequestHMAC([]byte(setSecretOperation), []byte(application), []byte(name), value)
	aad, err := s.controllerSecretAAD(ctx, "application_secret", application, name, versionID)
	if err != nil {
		return model.SecretMutationResult{}, err
	}
	encrypted, err := box.Seal(aad, value)
	if err != nil {
		return model.SecretMutationResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.SecretMutationResult{}, err
	}
	defer tx.Rollback()
	if replay, found, err := replaySecretMutation(ctx, tx, idempotencyKey, setSecretOperation, requestHash); err != nil || found {
		return replay, err
	}
	stamp := now.UTC()
	stampText := stamp.Format(time.RFC3339Nano)
	var createdAt string
	err = tx.QueryRowContext(ctx, `SELECT created_at FROM application_secrets WHERE application_name = ? AND name = ?`, application, name).Scan(&createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		createdAt = stampText
		_, err = tx.ExecContext(ctx, `INSERT INTO application_secrets
			(application_name, name, current_version_id, created_at, updated_at, deleted_at) VALUES (?, ?, ?, ?, ?, NULL)`,
			application, name, versionID, createdAt, stampText)
	} else if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE application_secrets SET current_version_id = ?, updated_at = ?, deleted_at = NULL
			WHERE application_name = ? AND name = ?`, versionID, stampText, application, name)
	}
	if err != nil {
		return model.SecretMutationResult{}, fmt.Errorf("store secret metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO application_secret_versions
		(id, application_name, name, algorithm, nonce, ciphertext, wrapped_key_nonce, wrapped_key, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, versionID, application, name, encrypted.Algorithm,
		encrypted.Nonce, encrypted.Ciphertext, encrypted.WrappedKeyNonce, encrypted.WrappedKey, stampText); err != nil {
		return model.SecretMutationResult{}, fmt.Errorf("store encrypted secret version: %w", err)
	}
	result := model.SecretMutationResult{Application: application, Name: name, VersionID: versionID, Changed: true, UpdatedAt: stamp}
	sequence, err := insertSecretEvent(ctx, tx, model.EventSecretVersionCreated, result, stampText)
	if err != nil {
		return model.SecretMutationResult{}, err
	}
	result.EventSequence = sequence
	if err := recordSecretMutation(ctx, tx, idempotencyKey, setSecretOperation, requestHash, result, stampText); err != nil {
		return model.SecretMutationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.SecretMutationResult{}, fmt.Errorf("commit secret version: %w", err)
	}
	return result, nil
}

func (s *Store) ResolveApplicationSecret(ctx context.Context, box *security.SecretBox, application, name string) (model.ResolvedSecret, error) {
	var versionID, algorithm string
	var nonce, ciphertext, wrappedNonce, wrappedKey []byte
	err := s.db.QueryRowContext(ctx, `SELECT s.current_version_id, v.algorithm, v.nonce, v.ciphertext, v.wrapped_key_nonce, v.wrapped_key
		FROM application_secrets s JOIN application_secret_versions v ON v.id = s.current_version_id
		WHERE s.application_name = ? AND s.name = ? AND s.deleted_at IS NULL`, application, name).
		Scan(&versionID, &algorithm, &nonce, &ciphertext, &wrappedNonce, &wrappedKey)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ResolvedSecret{}, ErrSecretNotFound
	}
	if err != nil {
		return model.ResolvedSecret{}, fmt.Errorf("read encrypted secret: %w", err)
	}
	aad, err := s.controllerSecretAAD(ctx, "application_secret", application, name, versionID)
	if err != nil {
		return model.ResolvedSecret{}, err
	}
	value, err := box.Open(aad, security.EncryptedValue{Algorithm: algorithm, Nonce: nonce, Ciphertext: ciphertext, WrappedKeyNonce: wrappedNonce, WrappedKey: wrappedKey})
	if err != nil {
		return model.ResolvedSecret{}, fmt.Errorf("decrypt secret %s/%s version %s: %w", application, name, versionID, err)
	}
	return model.ResolvedSecret{Application: application, Name: name, VersionID: versionID, Value: value}, nil
}

func (s *Store) ResolveApplicationSecretVersion(ctx context.Context, box *security.SecretBox, application, name, versionID string) (model.ResolvedSecret, error) {
	var algorithm string
	var nonce, ciphertext, wrappedNonce, wrappedKey []byte
	err := s.db.QueryRowContext(ctx, `SELECT algorithm, nonce, ciphertext, wrapped_key_nonce, wrapped_key
		FROM application_secret_versions WHERE application_name = ? AND name = ? AND id = ?`, application, name, versionID).
		Scan(&algorithm, &nonce, &ciphertext, &wrappedNonce, &wrappedKey)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ResolvedSecret{}, ErrSecretNotFound
	}
	if err != nil {
		return model.ResolvedSecret{}, err
	}
	aad, err := s.controllerSecretAAD(ctx, "application_secret", application, name, versionID)
	if err != nil {
		return model.ResolvedSecret{}, err
	}
	value, err := box.Open(aad, security.EncryptedValue{Algorithm: algorithm, Nonce: nonce, Ciphertext: ciphertext, WrappedKeyNonce: wrappedNonce, WrappedKey: wrappedKey})
	if err != nil {
		return model.ResolvedSecret{}, err
	}
	return model.ResolvedSecret{Application: application, Name: name, VersionID: versionID, Value: value}, nil
}

func (s *Store) ApplicationSecrets(ctx context.Context, application string) ([]model.SecretMetadata, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, current_version_id, created_at, updated_at
		FROM application_secrets WHERE application_name = ? AND deleted_at IS NULL ORDER BY name`, application)
	if err != nil {
		return nil, err
	}
	result := make([]model.SecretMetadata, 0)
	for rows.Next() {
		var item model.SecretMetadata
		var createdAt, updatedAt string
		item.Application = application
		if err := rows.Scan(&item.Name, &item.CurrentVersionID, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		item.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, err
		}
		item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range result {
		result[index].Referenced, err = s.ApplicationSecretReferenced(ctx, application, result[index].Name)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *Store) ApplicationSecretReferenced(ctx context.Context, application, name string) (bool, error) {
	app, err := s.Application(ctx, application)
	if err == nil && app.DeletedAt == nil {
		var manifest config.Manifest
		if err := json.Unmarshal(app.CanonicalManifest, &manifest); err != nil {
			return false, err
		}
		if manifestReferencesSecret(manifest, name) {
			return true, nil
		}
	} else if err != nil && !errors.Is(err, ErrApplicationNotFound) {
		return false, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM assignment_revisions a JOIN nodes n
		ON n.id = a.node_id AND n.assignment_generation = a.generation`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return false, err
		}
		_, allocations, jobs, err := model.DecodeAssignmentDocument([]byte(payload))
		if err != nil {
			return false, err
		}
		for _, allocation := range allocations {
			if allocation.Application == application && (bindingsReference(allocation.SecretEnvironment, allocation.SecretFiles, name) ||
				allocation.Rollback != nil && bindingsReference(allocation.Rollback.SecretEnvironment, allocation.Rollback.SecretFiles, name)) {
				return true, nil
			}
		}
		for _, job := range jobs {
			if job.Application == application && bindingsReference(job.SecretEnvironment, job.SecretFiles, name) {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}

func (s *Store) DeleteApplicationSecret(ctx context.Context, box *security.SecretBox, application, name, idempotencyKey string, now time.Time) (model.SecretMutationResult, error) {
	if box == nil || application == "" || name == "" || len(idempotencyKey) < 16 || len(idempotencyKey) > 128 || now.IsZero() {
		return model.SecretMutationResult{}, fmt.Errorf("secret deletion input is invalid")
	}
	requestHash := box.RequestHMAC([]byte(deleteSecretOperation), []byte(application), []byte(name))
	replayTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.SecretMutationResult{}, err
	}
	if replay, found, replayErr := replaySecretMutation(ctx, replayTx, idempotencyKey, deleteSecretOperation, requestHash); replayErr != nil || found {
		_ = replayTx.Rollback()
		return replay, replayErr
	}
	if err := replayTx.Rollback(); err != nil {
		return model.SecretMutationResult{}, err
	}
	if used, err := s.ApplicationSecretReferenced(ctx, application, name); err != nil {
		return model.SecretMutationResult{}, err
	} else if used {
		return model.SecretMutationResult{}, ErrSecretInUse
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.SecretMutationResult{}, err
	}
	defer tx.Rollback()
	if replay, found, err := replaySecretMutation(ctx, tx, idempotencyKey, deleteSecretOperation, requestHash); err != nil || found {
		return replay, err
	}
	stamp := now.UTC()
	result := model.SecretMutationResult{Application: application, Name: name, Changed: true, UpdatedAt: stamp}
	changed, err := tx.ExecContext(ctx, `UPDATE application_secrets SET deleted_at = ?, updated_at = ?
		WHERE application_name = ? AND name = ? AND deleted_at IS NULL`, stamp.Format(time.RFC3339Nano), stamp.Format(time.RFC3339Nano), application, name)
	if err != nil {
		return model.SecretMutationResult{}, err
	}
	count, _ := changed.RowsAffected()
	if count == 0 {
		return model.SecretMutationResult{}, ErrSecretNotFound
	}
	result.EventSequence, err = insertSecretEvent(ctx, tx, model.EventSecretDeleted, result, stamp.Format(time.RFC3339Nano))
	if err != nil {
		return model.SecretMutationResult{}, err
	}
	if err := recordSecretMutation(ctx, tx, idempotencyKey, deleteSecretOperation, requestHash, result, stamp.Format(time.RFC3339Nano)); err != nil {
		return model.SecretMutationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.SecretMutationResult{}, err
	}
	return result, nil
}

func (s *Store) SetRegistryCredential(ctx context.Context, box *security.SecretBox, host, username, versionID string,
	secret []byte, idempotencyKey string, now time.Time,
) (model.SecretMutationResult, error) {
	if box == nil || host == "" || username == "" || versionID == "" || len(secret) == 0 || len(secret) > 1<<20 ||
		len(idempotencyKey) < 16 || len(idempotencyKey) > 128 || now.IsZero() {
		return model.SecretMutationResult{}, fmt.Errorf("registry credential input is invalid")
	}
	requestHash := box.RequestHMAC([]byte(setRegistryOperation), []byte(host), []byte(username), secret)
	aad, err := s.controllerSecretAAD(ctx, "registry_credential", host, versionID)
	if err != nil {
		return model.SecretMutationResult{}, err
	}
	encrypted, err := box.Seal(aad, secret)
	if err != nil {
		return model.SecretMutationResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.SecretMutationResult{}, err
	}
	defer tx.Rollback()
	if replay, found, err := replaySecretMutation(ctx, tx, idempotencyKey, setRegistryOperation, requestHash); err != nil || found {
		return replay, err
	}
	stamp := now.UTC()
	stampText := stamp.Format(time.RFC3339Nano)
	var createdAt string
	err = tx.QueryRowContext(ctx, "SELECT created_at FROM registry_credentials WHERE host = ?", host).Scan(&createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		createdAt = stampText
		_, err = tx.ExecContext(ctx, `INSERT INTO registry_credentials(host, username, current_version_id, created_at, updated_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, NULL)`, host, username, versionID, createdAt, stampText)
	} else if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE registry_credentials SET username = ?, current_version_id = ?, updated_at = ?, deleted_at = NULL WHERE host = ?`,
			username, versionID, stampText, host)
	}
	if err != nil {
		return model.SecretMutationResult{}, fmt.Errorf("store registry credential metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO registry_credential_versions
		(id, host, username, algorithm, nonce, ciphertext, wrapped_key_nonce, wrapped_key, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, versionID, host, username, encrypted.Algorithm,
		encrypted.Nonce, encrypted.Ciphertext, encrypted.WrappedKeyNonce, encrypted.WrappedKey, stampText); err != nil {
		return model.SecretMutationResult{}, fmt.Errorf("store encrypted registry credential: %w", err)
	}
	result := model.SecretMutationResult{Host: host, Username: username, VersionID: versionID, Changed: true, UpdatedAt: stamp}
	result.EventSequence, err = insertSecretEvent(ctx, tx, model.EventRegistryCredentialUpdated, result, stampText)
	if err != nil {
		return model.SecretMutationResult{}, err
	}
	if err := recordSecretMutation(ctx, tx, idempotencyKey, setRegistryOperation, requestHash, result, stampText); err != nil {
		return model.SecretMutationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.SecretMutationResult{}, err
	}
	return result, nil
}

func (s *Store) ResolveRegistryCredential(ctx context.Context, box *security.SecretBox, host string) (model.ResolvedRegistryCredential, error) {
	var username, versionID, algorithm string
	var nonce, ciphertext, wrappedNonce, wrappedKey []byte
	err := s.db.QueryRowContext(ctx, `SELECT c.username, c.current_version_id, v.algorithm, v.nonce, v.ciphertext, v.wrapped_key_nonce, v.wrapped_key

		FROM registry_credentials c JOIN registry_credential_versions v ON v.id = c.current_version_id
		WHERE c.host = ? AND c.deleted_at IS NULL`, host).
		Scan(&username, &versionID, &algorithm, &nonce, &ciphertext, &wrappedNonce, &wrappedKey)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ResolvedRegistryCredential{}, ErrRegistryCredentialNotFound
	}
	if err != nil {
		return model.ResolvedRegistryCredential{}, err
	}
	aad, err := s.controllerSecretAAD(ctx, "registry_credential", host, versionID)
	if err != nil {
		return model.ResolvedRegistryCredential{}, err
	}
	value, err := box.Open(aad, security.EncryptedValue{Algorithm: algorithm, Nonce: nonce, Ciphertext: ciphertext, WrappedKeyNonce: wrappedNonce, WrappedKey: wrappedKey})
	if err != nil {
		return model.ResolvedRegistryCredential{}, err
	}
	return model.ResolvedRegistryCredential{Host: host, Username: username, VersionID: versionID, Secret: value}, nil
}

func (s *Store) ResolveRegistryCredentialVersion(ctx context.Context, box *security.SecretBox, host, versionID string) (model.ResolvedRegistryCredential, error) {
	var username, algorithm string
	var nonce, ciphertext, wrappedNonce, wrappedKey []byte
	err := s.db.QueryRowContext(ctx, `SELECT username, algorithm, nonce, ciphertext, wrapped_key_nonce, wrapped_key
		FROM registry_credential_versions WHERE host = ? AND id = ?`, host, versionID).
		Scan(&username, &algorithm, &nonce, &ciphertext, &wrappedNonce, &wrappedKey)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ResolvedRegistryCredential{}, ErrRegistryCredentialNotFound
	}
	if err != nil {
		return model.ResolvedRegistryCredential{}, err
	}
	aad, err := s.controllerSecretAAD(ctx, "registry_credential", host, versionID)
	if err != nil {
		return model.ResolvedRegistryCredential{}, err
	}
	value, err := box.Open(aad, security.EncryptedValue{Algorithm: algorithm, Nonce: nonce, Ciphertext: ciphertext, WrappedKeyNonce: wrappedNonce, WrappedKey: wrappedKey})
	if err != nil {
		return model.ResolvedRegistryCredential{}, err
	}
	return model.ResolvedRegistryCredential{Host: host, Username: username, VersionID: versionID, Secret: value}, nil
}

func (s *Store) RegistryCredentials(ctx context.Context) ([]model.RegistryCredentialMetadata, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT host, username, current_version_id, created_at, updated_at
		FROM registry_credentials WHERE deleted_at IS NULL ORDER BY host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]model.RegistryCredentialMetadata, 0)
	for rows.Next() {
		var item model.RegistryCredentialMetadata
		var createdAt, updatedAt string
		if err := rows.Scan(&item.Host, &item.Username, &item.CurrentVersionID, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		item.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, err
		}
		item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) DeleteRegistryCredential(ctx context.Context, box *security.SecretBox, host, idempotencyKey string, now time.Time) (model.SecretMutationResult, error) {
	if box == nil || host == "" || len(idempotencyKey) < 16 || len(idempotencyKey) > 128 || now.IsZero() {
		return model.SecretMutationResult{}, fmt.Errorf("registry credential deletion input is invalid")
	}
	requestHash := box.RequestHMAC([]byte(deleteRegistryOperation), []byte(host))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.SecretMutationResult{}, err
	}
	defer tx.Rollback()
	if replay, found, err := replaySecretMutation(ctx, tx, idempotencyKey, deleteRegistryOperation, requestHash); err != nil || found {
		return replay, err
	}
	stamp := now.UTC()
	stampText := stamp.Format(time.RFC3339Nano)
	changed, err := tx.ExecContext(ctx, `UPDATE registry_credentials SET deleted_at = ?, updated_at = ? WHERE host = ? AND deleted_at IS NULL`, stampText, stampText, host)
	if err != nil {
		return model.SecretMutationResult{}, err
	}
	count, _ := changed.RowsAffected()
	if count == 0 {
		return model.SecretMutationResult{}, ErrRegistryCredentialNotFound
	}
	result := model.SecretMutationResult{Host: host, Changed: true, UpdatedAt: stamp}
	result.EventSequence, err = insertSecretEvent(ctx, tx, model.EventRegistryCredentialDeleted, result, stampText)
	if err != nil {
		return model.SecretMutationResult{}, err
	}
	if err := recordSecretMutation(ctx, tx, idempotencyKey, deleteRegistryOperation, requestHash, result, stampText); err != nil {
		return model.SecretMutationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.SecretMutationResult{}, err
	}
	return result, nil
}

func replaySecretMutation(ctx context.Context, tx *sql.Tx, key, operation, requestHash string) (model.SecretMutationResult, bool, error) {
	var storedOperation, storedHash, response string
	err := tx.QueryRowContext(ctx, `SELECT operation, request_hash, response FROM idempotency_keys WHERE key = ?`, key).
		Scan(&storedOperation, &storedHash, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return model.SecretMutationResult{}, false, nil
	}
	if err != nil {
		return model.SecretMutationResult{}, false, err
	}
	if storedOperation != operation || storedHash != requestHash {
		return model.SecretMutationResult{}, false, ErrIdempotencyConflict
	}
	var result model.SecretMutationResult
	if err := json.Unmarshal([]byte(response), &result); err != nil {
		return model.SecretMutationResult{}, false, err
	}
	return result, true, nil
}

func recordSecretMutation(ctx context.Context, tx *sql.Tx, key, operation, requestHash string, result model.SecretMutationResult, stamp string) error {
	response, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO idempotency_keys(key, operation, request_hash, response, created_at) VALUES (?, ?, ?, ?, ?)`,
		key, operation, requestHash, string(response), stamp)
	return err
}

func insertSecretEvent(ctx context.Context, tx *sql.Tx, kind string, result model.SecretMutationResult, stamp string) (uint64, error) {
	var clusterID string
	if err := tx.QueryRowContext(ctx, "SELECT id FROM clusters WHERE singleton = 1").Scan(&clusterID); err != nil {
		return 0, err
	}
	payload, _ := json.Marshal(result)
	inserted, err := tx.ExecContext(ctx, `INSERT INTO events(cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`, clusterID, kind, string(payload), stamp)
	if err != nil {
		return 0, err
	}
	sequence, err := inserted.LastInsertId()
	return uint64(sequence), err
}

func manifestReferencesSecret(manifest config.Manifest, name string) bool {
	for _, service := range manifest.Services {
		for _, ref := range service.SecretEnv {
			if ref == name {
				return true
			}
		}
		for _, ref := range service.SecretFiles {
			if ref.Source == name {
				return true
			}
		}
		if service.HTTP != nil && service.HTTP.BasicAuth != nil && service.HTTP.BasicAuth.PasswordSecret == name {
			return true
		}
	}
	for _, job := range manifest.CronJobs {
		for _, ref := range job.SecretEnv {
			if ref == name {
				return true
			}
		}
		for _, ref := range job.SecretFiles {
			if ref.Source == name {
				return true
			}
		}
	}
	for _, notification := range manifest.Notifications {
		if notification.URLSecret == name {
			return true
		}
	}
	return false
}

func bindingsReference(environment map[string]model.SecretReference, files []model.SecretFileDesired, name string) bool {
	for _, ref := range environment {
		if ref.Name == name {
			return true
		}
	}
	for _, ref := range files {
		if ref.Name == name {
			return true
		}
	}
	return false
}

func NormalizeRegistryHost(raw string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(raw))
	if host == "" || strings.ContainsAny(host, "/?#@ \\") || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") || len(host) > 255 {
		return "", fmt.Errorf("registry host must be a normalized hostname with an optional port")
	}
	parsed, err := url.Parse("https://" + host)
	if err != nil || parsed.Host != host || parsed.Hostname() == "" || parsed.User != nil {
		return "", fmt.Errorf("registry host must be a normalized hostname with an optional port")
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return "", fmt.Errorf("registry port must be between 1 and 65535")
		}
	}
	hostname := parsed.Hostname()
	if net.ParseIP(hostname) == nil {
		for _, label := range strings.Split(hostname, ".") {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", fmt.Errorf("registry hostname contains an invalid label")
			}
			for _, character := range label {
				if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
					return "", fmt.Errorf("registry hostname contains invalid characters")
				}
			}
		}
	}
	return host, nil
}

func (s *Store) controllerSecretAAD(ctx context.Context, parts ...string) ([]byte, error) {
	var clusterID string
	if err := s.db.QueryRowContext(ctx, "SELECT id FROM clusters WHERE singleton = 1").Scan(&clusterID); err != nil {
		return nil, fmt.Errorf("read cluster identity for secret encryption: %w", err)
	}
	return security.SecretAssociatedData(append([]string{"controller", clusterID}, parts...)...), nil
}
