package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
)

const configureNetworkOperation = "network.configure"

var ErrNetworkFrozen = errors.New("network is already initialized")

func (s *Store) ConfigureNetwork(ctx context.Context, input model.ConfigureNetwork) (model.ConfigureNetworkResult, error) {
	prefix, err := model.ParseWorkloadCIDR(input.CIDR)
	if err != nil {
		return model.ConfigureNetworkResult{}, err
	}
	if len(input.IdempotencyKey) < 16 || len(input.IdempotencyKey) > 128 || len(input.RequestHash) != 64 || input.UpdatedAt.IsZero() {
		return model.ConfigureNetworkResult{}, fmt.Errorf("network configuration idempotency data is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.ConfigureNetworkResult{}, fmt.Errorf("begin network configuration: %w", err)
	}
	defer tx.Rollback()

	var operation, storedHash, encodedResponse string
	err = tx.QueryRowContext(ctx, `SELECT operation, request_hash, response FROM idempotency_keys WHERE key = ?`, input.IdempotencyKey).
		Scan(&operation, &storedHash, &encodedResponse)
	if err == nil {
		if operation != configureNetworkOperation || storedHash != input.RequestHash {
			return model.ConfigureNetworkResult{}, ErrIdempotencyConflict
		}
		var replay model.ConfigureNetworkResult
		if err := json.Unmarshal([]byte(encodedResponse), &replay); err != nil {
			return model.ConfigureNetworkResult{}, fmt.Errorf("decode network configuration replay: %w", err)
		}
		return replay, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.ConfigureNetworkResult{}, fmt.Errorf("read network configuration replay: %w", err)
	}

	var clusterID, currentCIDR, updatedText string
	var generation int64
	var frozenAt sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT id, workload_cidr, network_generation, network_frozen_at, network_updated_at
		FROM clusters WHERE singleton = 1`).Scan(&clusterID, &currentCIDR, &generation, &frozenAt, &updatedText); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.ConfigureNetworkResult{}, ErrNotInitialized
		}
		return model.ConfigureNetworkResult{}, fmt.Errorf("read network configuration: %w", err)
	}
	if frozenAt.Valid && currentCIDR != prefix.String() {
		return model.ConfigureNetworkResult{}, ErrNetworkFrozen
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, updatedText)
	if err != nil {
		return model.ConfigureNetworkResult{}, fmt.Errorf("read network configuration time: %w", err)
	}
	result := model.ConfigureNetworkResult{
		CIDR: currentCIDR, Generation: uint64(generation), Frozen: frozenAt.Valid,
		Changed: false, UpdatedAt: updatedAt,
	}
	if currentCIDR != prefix.String() {
		result.CIDR = prefix.String()
		result.Generation++
		result.Changed = true
		result.UpdatedAt = input.UpdatedAt.UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE clusters SET workload_cidr = ?, network_generation = ?, network_updated_at = ? WHERE singleton = 1`,
			result.CIDR, result.Generation, result.UpdatedAt.Format(time.RFC3339Nano)); err != nil {
			return model.ConfigureNetworkResult{}, fmt.Errorf("update network configuration: %w", err)
		}
		payload, _ := json.Marshal(struct {
			CIDR       string `json:"cidr"`
			Generation uint64 `json:"generation"`
		}{result.CIDR, result.Generation})
		event, err := tx.ExecContext(ctx, `INSERT INTO events(cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
			clusterID, model.EventNetworkConfigured, string(payload), result.UpdatedAt.Format(time.RFC3339Nano))
		if err != nil {
			return model.ConfigureNetworkResult{}, fmt.Errorf("record network configuration event: %w", err)
		}
		sequence, err := event.LastInsertId()
		if err != nil || sequence < 1 {
			return model.ConfigureNetworkResult{}, fmt.Errorf("read network configuration event sequence")
		}
		result.EventSequence = uint64(sequence)
	}
	response, err := json.Marshal(result)
	if err != nil {
		return model.ConfigureNetworkResult{}, fmt.Errorf("encode network configuration response: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_keys(key, operation, request_hash, response, created_at)
		VALUES (?, ?, ?, ?, ?)`, input.IdempotencyKey, configureNetworkOperation, input.RequestHash,
		string(response), input.UpdatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return model.ConfigureNetworkResult{}, fmt.Errorf("record network configuration response: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.ConfigureNetworkResult{}, fmt.Errorf("commit network configuration: %w", err)
	}
	return result, nil
}

func (s *Store) FreezeNetwork(ctx context.Context, nodeID string, status model.NetworkStatus, now time.Time) (bool, error) {
	if strings.TrimSpace(nodeID) == "" || status.State != model.NetworkReady || status.Generation == 0 || now.IsZero() {
		return false, fmt.Errorf("ready network observation is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var clusterID, cidr string
	var generation int64
	var frozenAt sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT id, workload_cidr, network_generation, network_frozen_at FROM clusters WHERE singleton = 1`).
		Scan(&clusterID, &cidr, &generation, &frozenAt); err != nil {
		return false, err
	}
	if uint64(generation) != status.Generation || cidr != status.ClusterCIDR {
		return false, ErrStaleAgentReport
	}
	if frozenAt.Valid {
		return false, nil
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE clusters SET network_frozen_at = ? WHERE singleton = 1 AND network_frozen_at IS NULL`, stamp); err != nil {
		return false, err
	}
	payload, _ := json.Marshal(struct {
		CIDR       string `json:"cidr"`
		NodeSubnet string `json:"node_subnet"`
	}{cidr, status.NodeSubnet})
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(cluster_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
		clusterID, model.EventNetworkReady, string(payload), stamp); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
