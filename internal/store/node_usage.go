package store

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

// NodeUsageRetention is how long agent usage reports are kept.
const NodeUsageRetention = 24 * time.Hour

// RecordNodeUsage stores one usage report and drops the node's reports that
// are older than NodeUsageRetention.
func (s *Store) RecordNodeUsage(ctx context.Context, nodeID string, usage model.NodeUsage) error {
	if nodeID == "" || usage.ObservedAt.IsZero() {
		return fmt.Errorf("node and observation time are required")
	}
	for _, value := range []uint64{usage.CPUUsedMillis, usage.MemoryUsedBytes, usage.DiskUsedBytes, usage.DiskTotalBytes} {
		if value > math.MaxInt64 {
			return fmt.Errorf("node usage is out of range")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin node usage transaction: %w", err)
	}
	defer tx.Rollback()
	observed := usage.ObservedAt.UTC().Unix()
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO node_usage_samples
		(node_id, observed_at, cpu_used_millis, memory_used_bytes, disk_used_bytes, disk_total_bytes)
		VALUES (?, ?, ?, ?, ?, ?)`, nodeID, observed, int64(usage.CPUUsedMillis), int64(usage.MemoryUsedBytes),
		int64(usage.DiskUsedBytes), int64(usage.DiskTotalBytes)); err != nil {
		return fmt.Errorf("record node usage: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM node_usage_samples WHERE node_id = ? AND observed_at < ?`,
		nodeID, observed-int64(NodeUsageRetention/time.Second)); err != nil {
		return fmt.Errorf("prune node usage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit node usage: %w", err)
	}
	return nil
}

// LatestNodeUsage returns each node's newest usage report.
func (s *Store) LatestNodeUsage(ctx context.Context) (map[string]model.NodeUsage, error) {
	// SQLite takes the bare columns from the row holding the max.
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, max(observed_at), cpu_used_millis, memory_used_bytes,
		disk_used_bytes, disk_total_bytes FROM node_usage_samples GROUP BY node_id`)
	if err != nil {
		return nil, fmt.Errorf("read node usage: %w", err)
	}
	defer rows.Close()
	latest := map[string]model.NodeUsage{}
	for rows.Next() {
		var nodeID string
		var observed, cpu, memory, diskUsed, diskTotal int64
		if err := rows.Scan(&nodeID, &observed, &cpu, &memory, &diskUsed, &diskTotal); err != nil {
			return nil, fmt.Errorf("read node usage: %w", err)
		}
		latest[nodeID] = model.NodeUsage{
			ObservedAt: time.Unix(observed, 0).UTC(), CPUUsedMillis: uint64(cpu), MemoryUsedBytes: uint64(memory),
			DiskUsedBytes: uint64(diskUsed), DiskTotalBytes: uint64(diskTotal),
		}
	}
	return latest, rows.Err()
}

// NodeUsageHistory averages a node's reports since the given time into
// buckets of step, oldest first. Buckets without reports are left out.
func (s *Store) NodeUsageHistory(ctx context.Context, nodeID string, since time.Time, step time.Duration) ([]model.NodeUsage, error) {
	seconds := int64(step / time.Second)
	if nodeID == "" || seconds <= 0 {
		return nil, fmt.Errorf("node and a positive step are required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT observed_at / ? * ? AS bucket, avg(cpu_used_millis), avg(memory_used_bytes),
		avg(disk_used_bytes), max(disk_total_bytes) FROM node_usage_samples
		WHERE node_id = ? AND observed_at >= ? GROUP BY bucket ORDER BY bucket`,
		seconds, seconds, nodeID, since.UTC().Unix())
	if err != nil {
		return nil, fmt.Errorf("read node usage history: %w", err)
	}
	defer rows.Close()
	history := make([]model.NodeUsage, 0)
	for rows.Next() {
		var bucket, diskTotal int64
		var cpu, memory, diskUsed float64
		if err := rows.Scan(&bucket, &cpu, &memory, &diskUsed, &diskTotal); err != nil {
			return nil, fmt.Errorf("read node usage history: %w", err)
		}
		history = append(history, model.NodeUsage{
			ObservedAt: time.Unix(bucket, 0).UTC(), CPUUsedMillis: uint64(math.Round(cpu)),
			MemoryUsedBytes: uint64(math.Round(memory)), DiskUsedBytes: uint64(math.Round(diskUsed)), DiskTotalBytes: uint64(diskTotal),
		})
	}
	return history, rows.Err()
}
