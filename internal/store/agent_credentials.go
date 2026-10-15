package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

const renewAgentCertificateOperation = "node.certificate.renew"

func (s *Store) IssueAgentCertificateRenewal(ctx context.Context, input model.RenewAgentCertificate) (model.RenewAgentCertificateResult, error) {
	if input.NodeID == "" || input.CurrentSerial == "" || len(input.PublicKeyHash) != 64 || input.CertificateSerial == "" ||
		len(input.CertificatePEM) == 0 || input.CertificateNotAfter.IsZero() || len(input.IdempotencyKey) < 16 ||
		len(input.IdempotencyKey) > 128 || len(input.RequestHash) != 64 || input.CreatedAt.IsZero() {
		return model.RenewAgentCertificateResult{}, fmt.Errorf("agent certificate renewal data is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.RenewAgentCertificateResult{}, fmt.Errorf("begin agent certificate renewal: %w", err)
	}
	defer tx.Rollback()
	var replay model.RenewAgentCertificateResult
	if found, err := replayNodeMutation(ctx, tx, input.IdempotencyKey, renewAgentCertificateOperation, input.RequestHash, &replay); err != nil || found {
		return replay, err
	}
	node, err := nodeByIDInTransaction(ctx, tx, input.NodeID)
	if err != nil {
		return model.RenewAgentCertificateResult{}, err
	}
	if node.Lifecycle == model.NodeLifecycleRemoved || node.CertificateSerial != input.CurrentSerial || node.PublicKeyHash != input.PublicKeyHash {
		return model.RenewAgentCertificateResult{}, ErrNodeRemoved
	}
	if node.PendingCertificateSerial != "" {
		return model.RenewAgentCertificateResult{}, ErrCertificateRenewalPending
	}
	stamp := input.CreatedAt.UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET pending_certificate_serial = ?, pending_certificate_pem = ?,
		pending_certificate_not_after = ?, pending_certificate_request_hash = ?, pending_certificate_issued_at = ?
		WHERE id = ? AND certificate_serial = ? AND lifecycle != 'REMOVED'`, input.CertificateSerial, input.CertificatePEM,
		input.CertificateNotAfter.UTC().Format(time.RFC3339Nano), input.RequestHash, stamp.Format(time.RFC3339Nano),
		input.NodeID, input.CurrentSerial); err != nil {
		return model.RenewAgentCertificateResult{}, fmt.Errorf("store pending agent certificate: %w", err)
	}
	payload, _ := json.Marshal(struct {
		NodeID    string    `json:"node_id"`
		ExpiresAt time.Time `json:"expires_at"`
	}{input.NodeID, input.CertificateNotAfter.UTC()})
	inserted, err := tx.ExecContext(ctx, `INSERT INTO events(cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
		node.ClusterID, model.EventCertificateRenewalIssued, string(payload), stamp.Format(time.RFC3339Nano))
	if err != nil {
		return model.RenewAgentCertificateResult{}, fmt.Errorf("record agent certificate renewal: %w", err)
	}
	sequence, err := inserted.LastInsertId()
	if err != nil || sequence < 1 {
		return model.RenewAgentCertificateResult{}, fmt.Errorf("read agent certificate renewal event sequence")
	}
	result := model.RenewAgentCertificateResult{NodeID: input.NodeID, CertificatePEM: append([]byte(nil), input.CertificatePEM...),
		ExpiresAt: input.CertificateNotAfter.UTC(), Changed: true, EventSequence: uint64(sequence)}
	if err := recordNodeMutation(ctx, tx, input.IdempotencyKey, renewAgentCertificateOperation, input.RequestHash, result, stamp.Format(time.RFC3339Nano)); err != nil {
		return model.RenewAgentCertificateResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.RenewAgentCertificateResult{}, fmt.Errorf("commit agent certificate renewal: %w", err)
	}
	return result, nil
}

func (s *Store) PromoteAgentCertificate(ctx context.Context, nodeID, serial string, now time.Time) (bool, error) {
	if nodeID == "" || serial == "" || now.IsZero() {
		return false, fmt.Errorf("node, certificate serial, and activation time are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin agent certificate activation: %w", err)
	}
	defer tx.Rollback()
	node, err := nodeByIDInTransaction(ctx, tx, nodeID)
	if err != nil {
		return false, err
	}
	if node.CertificateSerial == serial {
		return false, nil
	}
	if node.PendingCertificateSerial != serial || len(node.PendingCertificatePEM) == 0 || node.PendingCertificateNotAfter == nil {
		return false, errors.New("agent certificate is not pending activation")
	}
	stamp := now.UTC()
	result, err := tx.ExecContext(ctx, `UPDATE nodes SET certificate_serial = pending_certificate_serial,
		certificate_pem = pending_certificate_pem, certificate_not_after = pending_certificate_not_after,
		pending_certificate_serial = '', pending_certificate_pem = X'', pending_certificate_not_after = '',
		pending_certificate_request_hash = '', pending_certificate_issued_at = ''
		WHERE id = ? AND pending_certificate_serial = ? AND lifecycle != 'REMOVED'`, nodeID, serial)
	if err != nil {
		return false, fmt.Errorf("activate pending agent certificate: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return false, errors.New("pending agent certificate changed before activation")
	}
	payload, _ := json.Marshal(struct {
		NodeID string `json:"node_id"`
	}{nodeID})
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
		node.ClusterID, model.EventCertificateActivated, string(payload), stamp.Format(time.RFC3339Nano)); err != nil {
		return false, fmt.Errorf("record agent certificate activation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit agent certificate activation: %w", err)
	}
	return true, nil
}

func (s *Store) AgentCertificateSlot(ctx context.Context, nodeID, serial, publicKeyHash string) (string, error) {
	node, err := s.NodeByID(ctx, nodeID)
	if err != nil {
		return "", err
	}
	if node.Lifecycle == model.NodeLifecycleRemoved || node.PublicKeyHash != publicKeyHash {
		return "", ErrNodeRemoved
	}
	switch serial {
	case node.CertificateSerial:
		return "CURRENT", nil
	case node.PendingCertificateSerial:
		if serial != "" {
			return "PENDING", nil
		}
	}
	return "", errors.New("agent certificate does not match enrolled identity")
}

func certificateState(node model.Node, now time.Time) (string, string) {
	if node.PendingCertificateSerial != "" {
		return model.CertificateRenewalPending, "pending certificate must reconnect to activate"
	}
	if !now.UTC().Before(node.CertificateNotAfter) {
		return model.CertificateRenewalFailed, "agent certificate has expired"
	}
	if node.CertificateNotAfter.Sub(now.UTC()) <= 90*24*time.Hour {
		return model.CertificateRenewalDue, "agent certificate is within the renewal window"
	}
	return model.CertificateHealthy, ""
}

func DecorateCertificateState(node *model.Node, now time.Time) {
	if node == nil {
		return
	}
	node.CertificateState, node.CertificateReason = certificateState(*node, now)
}
