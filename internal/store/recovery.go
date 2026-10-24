package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"

	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/security"
)

// RecoverySummary contains only aggregate metadata, never manifests or credentials.
type RecoverySummary struct {
	Cluster             model.Cluster `json:"cluster"`
	SchemaVersion       int           `json:"schema_version"`
	TargetSchemaVersion int           `json:"target_schema_version"`
	Nodes               int           `json:"nodes"`
	Applications        int           `json:"applications"`
	Deployments         int           `json:"deployments"`
	Jobs                int           `json:"jobs"`
	UnfinishedJobs      int           `json:"unfinished_jobs"`
	SecretKeyRequired   bool          `json:"secret_key_required"`
}

// OpenRecoveryCopy validates a private, standalone copy before migrating it.
// Callers must never pass an original backup or live database here.
func OpenRecoveryCopy(ctx context.Context, path string) (*Store, RecoverySummary, error) {
	var summary RecoverySummary
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=ro&immutable=1"}).String())
	if err != nil {
		return nil, summary, err
	}
	migrations, err := loadMigrations(embeddedMigrations)
	if err != nil {
		db.Close()
		return nil, summary, err
	}
	applied, err := appliedMigrations(ctx, db)
	db.Close()
	if err != nil {
		return nil, summary, fmt.Errorf("%w: missing or invalid migration history", ErrUnrecognized)
	}
	if len(applied) == 0 {
		return nil, summary, fmt.Errorf("%w: empty migration history", ErrUnrecognized)
	}
	if err := validateMigrationHistory(migrations, applied); err != nil {
		return nil, summary, err
	}
	if err := verifyBackup(ctx, path); err != nil {
		return nil, summary, err
	}
	summary.SchemaVersion, summary.TargetSchemaVersion = len(applied), len(migrations)
	s, err := OpenExisting(ctx, path)
	if err != nil {
		return nil, summary, err
	}
	fail := func(err error) (*Store, RecoverySummary, error) { s.Close(); return nil, summary, err }
	summary.Cluster, err = s.Cluster(ctx)
	if err != nil {
		return fail(err)
	}
	if err := verifyBackup(ctx, path); err != nil {
		return fail(err)
	}
	for _, item := range []struct {
		query string
		out   *int
	}{
		{"SELECT count(*) FROM nodes WHERE lifecycle != 'REMOVED'", &summary.Nodes},
		{"SELECT count(*) FROM applications WHERE deleted_at IS NULL", &summary.Applications},
		{"SELECT count(*) FROM application_revisions", &summary.Deployments},
		{"SELECT count(*) FROM one_off_jobs", &summary.Jobs},
		{"SELECT count(*) FROM one_off_jobs WHERE json_extract(status,'$.finished_at') IS NULL", &summary.UnfinishedJobs},
	} {
		if err := s.db.QueryRowContext(ctx, item.query).Scan(item.out); err != nil {
			return fail(fmt.Errorf("read recovery inventory: %w", err))
		}
	}
	fingerprint, err := s.SecretKeyFingerprint(ctx)
	if err != nil {
		return fail(err)
	}
	encrypted, err := s.HasEncryptedMaterial(ctx)
	if err != nil {
		return fail(err)
	}
	if encrypted && fingerprint == "" {
		return fail(fmt.Errorf("encrypted records have no secret key metadata"))
	}
	summary.SecretKeyRequired = fingerprint != ""
	return s, summary, nil
}

// VerifyRecoverySecrets authenticates all versions, including deleted secrets
// and old registry credentials still referenced by deployment/job history.
func (s *Store) VerifyRecoverySecrets(ctx context.Context, box *security.SecretBox) error {
	fingerprint, err := s.SecretKeyFingerprint(ctx)
	if err != nil {
		return err
	}
	if fingerprint != "" && (box == nil || box.Fingerprint() != fingerprint) {
		return ErrSecretKeyMismatch
	}
	cluster, err := s.Cluster(ctx)
	if err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT 'application_secret',application_name,name,id,algorithm,nonce,ciphertext,wrapped_key_nonce,wrapped_key FROM application_secret_versions
 UNION ALL SELECT 'registry_credential',host,id,'',algorithm,nonce,ciphertext,wrapped_key_nonce,wrapped_key FROM registry_credential_versions
 UNION ALL SELECT 'backup_destination',access_key_id,'','',algorithm,nonce,ciphertext,wrapped_key_nonce,wrapped_key FROM backup_destination`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, a, b, c string
		var value security.EncryptedValue
		if err := rows.Scan(&kind, &a, &b, &c, &value.Algorithm, &value.Nonce, &value.Ciphertext, &value.WrappedKeyNonce, &value.WrappedKey); err != nil {
			return fmt.Errorf("read encrypted recovery record")
		}
		parts := []string{"controller", cluster.ID, kind, a, b}
		if kind == "application_secret" {
			parts = append(parts, c)
		}
		plaintext, err := box.Open(security.SecretAssociatedData(parts...), value)
		if err != nil {
			return fmt.Errorf("authenticate encrypted recovery records: %w", err)
		}
		clear(plaintext)
	}
	return rows.Err()
}
