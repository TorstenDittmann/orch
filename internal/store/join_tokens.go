package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

const (
	createJoinTokenOperation = "node.join.create"
	revokeJoinTokenOperation = "node.join.revoke"
	maxPendingJoinTokens     = 100
	// The public CLI requires at least one minute. Allow five seconds for the
	// locally computed expiry to cross the authenticated network boundary.
	minimumRegisteredJoinLifetime = 55 * time.Second
)

func (s *Store) CreateJoinToken(ctx context.Context, input model.CreateJoinToken) (model.JoinTokenMutationResult, error) {
	if strings.TrimSpace(input.ID) == "" || strings.TrimSpace(input.NodeName) == "" || len(input.SecretHash) != 64 ||
		len(input.IdempotencyKey) < 16 || len(input.IdempotencyKey) > 128 || len(input.RequestHash) != 64 || input.CreatedAt.IsZero() ||
		input.ExpiresAt.Sub(input.CreatedAt) < minimumRegisteredJoinLifetime || input.ExpiresAt.Sub(input.CreatedAt) > 24*time.Hour {
		return model.JoinTokenMutationResult{}, fmt.Errorf("join credential data is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.JoinTokenMutationResult{}, fmt.Errorf("begin join credential creation: %w", err)
	}
	defer tx.Rollback()
	var replay model.JoinTokenMutationResult
	if found, err := replayNodeMutation(ctx, tx, input.IdempotencyKey, createJoinTokenOperation, input.RequestHash, &replay); err != nil || found {
		return replay, err
	}
	var clusterID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM clusters WHERE singleton = 1`).Scan(&clusterID); err != nil {
		return model.JoinTokenMutationResult{}, fmt.Errorf("read cluster for join credential: %w", err)
	}
	var existingNode int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM nodes WHERE name = ? AND lifecycle != 'REMOVED'`, input.NodeName).Scan(&existingNode); err != nil {
		return model.JoinTokenMutationResult{}, fmt.Errorf("check join node name: %w", err)
	}
	if existingNode != 0 {
		return model.JoinTokenMutationResult{}, ErrNodeAlreadyEnrolled
	}
	stamp := input.CreatedAt.UTC()
	var sameNode int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM join_tokens WHERE node_name = ?
		AND revoked_at IS NULL AND consumed_at IS NULL AND expires_at > ?`, input.NodeName, stamp.Format(time.RFC3339Nano)).Scan(&sameNode); err != nil {
		return model.JoinTokenMutationResult{}, fmt.Errorf("check pending join credential for node: %w", err)
	}
	if sameNode != 0 {
		return model.JoinTokenMutationResult{}, ErrJoinTokenUnavailable
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM join_tokens
		WHERE revoked_at IS NULL AND consumed_at IS NULL AND expires_at > ?`, stamp.Format(time.RFC3339Nano)).Scan(&pending); err != nil {
		return model.JoinTokenMutationResult{}, fmt.Errorf("count pending join credentials: %w", err)
	}
	if pending >= maxPendingJoinTokens {
		return model.JoinTokenMutationResult{}, ErrJoinTokenLimit
	}
	token := model.JoinToken{ID: input.ID, ClusterID: clusterID, NodeName: input.NodeName, SecretHash: input.SecretHash,
		CreatedAt: stamp, ExpiresAt: input.ExpiresAt.UTC(), State: model.JoinTokenPending}
	if _, err := tx.ExecContext(ctx, `INSERT INTO join_tokens
		(id, cluster_id, node_name, secret_hash, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		token.ID, token.ClusterID, token.NodeName, token.SecretHash, token.CreatedAt.Format(time.RFC3339Nano), token.ExpiresAt.Format(time.RFC3339Nano)); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return model.JoinTokenMutationResult{}, ErrJoinTokenUnavailable
		}
		return model.JoinTokenMutationResult{}, fmt.Errorf("insert join credential: %w", err)
	}
	payload, _ := json.Marshal(struct {
		ID        string    `json:"id"`
		NodeName  string    `json:"node_name"`
		ExpiresAt time.Time `json:"expires_at"`
	}{token.ID, token.NodeName, token.ExpiresAt})
	inserted, err := tx.ExecContext(ctx, `INSERT INTO events(cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
		clusterID, model.EventJoinTokenCreated, string(payload), stamp.Format(time.RFC3339Nano))
	if err != nil {
		return model.JoinTokenMutationResult{}, fmt.Errorf("record join credential creation: %w", err)
	}
	sequence, err := inserted.LastInsertId()
	if err != nil || sequence < 1 {
		return model.JoinTokenMutationResult{}, fmt.Errorf("read join credential event sequence")
	}
	token.SecretHash = ""
	result := model.JoinTokenMutationResult{Token: token, Changed: true, EventSequence: uint64(sequence)}
	if err := recordNodeMutation(ctx, tx, input.IdempotencyKey, createJoinTokenOperation, input.RequestHash, result, stamp.Format(time.RFC3339Nano)); err != nil {
		return model.JoinTokenMutationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.JoinTokenMutationResult{}, fmt.Errorf("commit join credential creation: %w", err)
	}
	return result, nil
}

func (s *Store) JoinTokens(ctx context.Context, now time.Time) ([]model.JoinToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, cluster_id, node_name, created_at, expires_at, revoked_at, consumed_at
		FROM join_tokens ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("list join credentials: %w", err)
	}
	defer rows.Close()
	var result []model.JoinToken
	for rows.Next() {
		token, err := scanJoinTokenMetadata(rows, now)
		if err != nil {
			return nil, err
		}
		result = append(result, token)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate join credentials: %w", err)
	}
	return result, nil
}

func (s *Store) ValidateJoinToken(ctx context.Context, joinID, presentedSecretHash, nodeName string, now time.Time) error {
	if joinID == "" || len(presentedSecretHash) != 64 || nodeName == "" || now.IsZero() {
		return ErrJoinTokenUnavailable
	}
	var storedHash, storedNodeName, expiresAt string
	var revokedAt, consumedAt sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT secret_hash, node_name, expires_at, revoked_at, consumed_at
		FROM join_tokens WHERE id = ?`, joinID).Scan(&storedHash, &storedNodeName, &expiresAt, &revokedAt, &consumedAt)
	if err != nil || subtle.ConstantTimeCompare([]byte(storedHash), []byte(presentedSecretHash)) != 1 || storedNodeName != nodeName || revokedAt.Valid {
		return ErrJoinTokenUnavailable
	}
	if consumedAt.Valid {
		return nil
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || !now.UTC().Before(expires) {
		return ErrJoinTokenUnavailable
	}
	return nil
}

// ValidateJoinTokenEnrollment repeats the credential check after the CSR has
// been hashed. It prevents a consumed token from reaching certificate signing
// unless the request is the exact replay whose response was frozen when the
// token was consumed.
func (s *Store) ValidateJoinTokenEnrollment(ctx context.Context, joinID, presentedSecretHash, nodeName, requestHash, idempotencyKey string, now time.Time) error {
	if joinID == "" || len(presentedSecretHash) != 64 || nodeName == "" || len(requestHash) != 64 || idempotencyKey == "" || now.IsZero() {
		return ErrJoinTokenUnavailable
	}
	var storedHash, storedNodeName, expiresAt, consumedRequestHash, consumedKey string
	var revokedAt, consumedAt sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT secret_hash, node_name, expires_at, revoked_at, consumed_at,
		consumed_request_hash, consumed_idempotency_key FROM join_tokens WHERE id = ?`, joinID).
		Scan(&storedHash, &storedNodeName, &expiresAt, &revokedAt, &consumedAt, &consumedRequestHash, &consumedKey)
	if err != nil || subtle.ConstantTimeCompare([]byte(storedHash), []byte(presentedSecretHash)) != 1 || storedNodeName != nodeName || revokedAt.Valid {
		return ErrJoinTokenUnavailable
	}
	if consumedAt.Valid {
		if consumedRequestHash != requestHash || consumedKey != idempotencyKey {
			return ErrJoinTokenUnavailable
		}
		return nil
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || !now.UTC().Before(expires) {
		return ErrJoinTokenUnavailable
	}
	return nil
}

func (s *Store) RevokeJoinToken(ctx context.Context, input model.RevokeJoinToken) (model.JoinTokenMutationResult, error) {
	if strings.TrimSpace(input.ID) == "" || len(input.IdempotencyKey) < 16 || len(input.IdempotencyKey) > 128 ||
		len(input.RequestHash) != 64 || input.RevokedAt.IsZero() {
		return model.JoinTokenMutationResult{}, fmt.Errorf("join credential revocation data is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.JoinTokenMutationResult{}, fmt.Errorf("begin join credential revocation: %w", err)
	}
	defer tx.Rollback()
	var replay model.JoinTokenMutationResult
	if found, err := replayNodeMutation(ctx, tx, input.IdempotencyKey, revokeJoinTokenOperation, input.RequestHash, &replay); err != nil || found {
		return replay, err
	}
	token, err := scanJoinTokenMetadata(tx.QueryRowContext(ctx, `SELECT id, cluster_id, node_name, created_at, expires_at, revoked_at, consumed_at
		FROM join_tokens WHERE id = ?`, input.ID), input.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.JoinTokenMutationResult{}, ErrJoinTokenUnavailable
	}
	if err != nil {
		return model.JoinTokenMutationResult{}, err
	}
	result := model.JoinTokenMutationResult{Token: token}
	stamp := input.RevokedAt.UTC()
	if token.State == model.JoinTokenPending {
		if _, err := tx.ExecContext(ctx, `UPDATE join_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL AND consumed_at IS NULL`,
			stamp.Format(time.RFC3339Nano), input.ID); err != nil {
			return model.JoinTokenMutationResult{}, fmt.Errorf("revoke join credential: %w", err)
		}
		token.RevokedAt, token.State, result.Changed = &stamp, model.JoinTokenRevoked, true
		payload, _ := json.Marshal(struct{ ID, NodeName string }{token.ID, token.NodeName})
		inserted, err := tx.ExecContext(ctx, `INSERT INTO events(cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
			token.ClusterID, model.EventJoinTokenRevoked, string(payload), stamp.Format(time.RFC3339Nano))
		if err != nil {
			return model.JoinTokenMutationResult{}, fmt.Errorf("record join credential revocation: %w", err)
		}
		sequence, err := inserted.LastInsertId()
		if err != nil || sequence < 1 {
			return model.JoinTokenMutationResult{}, fmt.Errorf("read join credential revocation event sequence")
		}
		result.EventSequence = uint64(sequence)
	}
	result.Token = token
	if err := recordNodeMutation(ctx, tx, input.IdempotencyKey, revokeJoinTokenOperation, input.RequestHash, result, stamp.Format(time.RFC3339Nano)); err != nil {
		return model.JoinTokenMutationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.JoinTokenMutationResult{}, fmt.Errorf("commit join credential revocation: %w", err)
	}
	return result, nil
}

func (s *Store) EnrollAgentWithJoin(ctx context.Context, joinID, presentedSecretHash string, input model.EnrollAgent) (model.EnrollAgentResult, error) {
	if err := validateEnrollAgent(input); err != nil || strings.TrimSpace(joinID) == "" || len(presentedSecretHash) != 64 {
		if err != nil {
			return model.EnrollAgentResult{}, err
		}
		return model.EnrollAgentResult{}, ErrJoinTokenUnavailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.EnrollAgentResult{}, fmt.Errorf("begin joined agent enrollment: %w", err)
	}
	defer tx.Rollback()
	var storedHash, nodeName, clusterID, expiresAt string
	var revokedAt, consumedAt sql.NullString
	var consumedRequestHash, consumedKey string
	err = tx.QueryRowContext(ctx, `SELECT secret_hash, node_name, cluster_id, expires_at, revoked_at, consumed_at,
		consumed_request_hash, consumed_idempotency_key FROM join_tokens WHERE id = ?`, joinID).
		Scan(&storedHash, &nodeName, &clusterID, &expiresAt, &revokedAt, &consumedAt, &consumedRequestHash, &consumedKey)
	if err != nil || subtle.ConstantTimeCompare([]byte(storedHash), []byte(presentedSecretHash)) != 1 || nodeName != input.Name || clusterID != input.ClusterID || revokedAt.Valid {
		return model.EnrollAgentResult{}, ErrJoinTokenUnavailable
	}
	if consumedAt.Valid {
		if consumedRequestHash != input.RequestHash || consumedKey != input.IdempotencyKey {
			return model.EnrollAgentResult{}, ErrJoinTokenUnavailable
		}
		result, found, err := replayEnrollment(ctx, tx, input.IdempotencyKey, input.RequestHash)
		if err != nil || !found {
			return model.EnrollAgentResult{}, ErrJoinTokenUnavailable
		}
		return result, nil
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || !input.CreatedAt.UTC().Before(expires) {
		return model.EnrollAgentResult{}, ErrJoinTokenUnavailable
	}
	result, err := enrollAgentInTransaction(ctx, tx, input)
	if err != nil {
		return model.EnrollAgentResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE join_tokens SET consumed_at = ?, consumed_request_hash = ?, consumed_idempotency_key = ?
		WHERE id = ? AND consumed_at IS NULL AND revoked_at IS NULL`, input.CreatedAt.UTC().Format(time.RFC3339Nano), input.RequestHash, input.IdempotencyKey, joinID); err != nil {
		return model.EnrollAgentResult{}, fmt.Errorf("consume join credential: %w", err)
	}
	payload, _ := json.Marshal(struct {
		ID       string `json:"id"`
		NodeName string `json:"node_name"`
	}{joinID, input.Name})
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
		input.ClusterID, model.EventJoinTokenConsumed, string(payload), input.CreatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return model.EnrollAgentResult{}, fmt.Errorf("record join credential consumption: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.EnrollAgentResult{}, fmt.Errorf("commit joined agent enrollment: %w", err)
	}
	return result, nil
}

type joinTokenScanner interface{ Scan(...any) error }

func scanJoinTokenMetadata(row joinTokenScanner, now time.Time) (model.JoinToken, error) {
	var token model.JoinToken
	var createdAt, expiresAt string
	var revokedAt, consumedAt sql.NullString
	if err := row.Scan(&token.ID, &token.ClusterID, &token.NodeName, &createdAt, &expiresAt, &revokedAt, &consumedAt); err != nil {
		return model.JoinToken{}, err
	}
	var err error
	token.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return model.JoinToken{}, fmt.Errorf("parse join credential creation time: %w", err)
	}
	token.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return model.JoinToken{}, fmt.Errorf("parse join credential expiry: %w", err)
	}
	if revokedAt.Valid {
		stamp, parseErr := time.Parse(time.RFC3339Nano, revokedAt.String)
		if parseErr != nil {
			return model.JoinToken{}, parseErr
		}
		token.RevokedAt, token.State = &stamp, model.JoinTokenRevoked
	} else if consumedAt.Valid {
		stamp, parseErr := time.Parse(time.RFC3339Nano, consumedAt.String)
		if parseErr != nil {
			return model.JoinToken{}, parseErr
		}
		token.ConsumedAt, token.State = &stamp, model.JoinTokenConsumed
	} else if !now.UTC().Before(token.ExpiresAt) {
		token.State = model.JoinTokenExpired
	} else {
		token.State = model.JoinTokenPending
	}
	return token, nil
}
