package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

// ServiceScale is the replica count the autoscaler chose for one service. It
// lives outside application revisions so scaling never adds a deployment.
type ServiceScale struct {
	Application string
	Service     string
	Replicas    int
	UpdatedAt   time.Time
}

// ServiceScales returns every recorded autoscaler choice keyed by
// application and then service.
func (s *Store) ServiceScales(ctx context.Context) (map[string]map[string]ServiceScale, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT application_name, service_name, replicas, updated_at FROM service_scales`)
	if err != nil {
		return nil, fmt.Errorf("read service scales: %w", err)
	}
	defer rows.Close()
	scales := map[string]map[string]ServiceScale{}
	for rows.Next() {
		var scale ServiceScale
		var updatedAt string
		if err := rows.Scan(&scale.Application, &scale.Service, &scale.Replicas, &updatedAt); err != nil {
			return nil, fmt.Errorf("read service scales: %w", err)
		}
		if scale.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
			return nil, fmt.Errorf("read service scale time: %w", err)
		}
		if scales[scale.Application] == nil {
			scales[scale.Application] = map[string]ServiceScale{}
		}
		scales[scale.Application][scale.Service] = scale
	}
	return scales, rows.Err()
}

// SetServiceScale records the autoscaler's replica count for a service and,
// when the count changed from previous, a service.scaled event saying why.
func (s *Store) SetServiceScale(ctx context.Context, scale ServiceScale, previous int, reason string) error {
	if scale.Application == "" || scale.Service == "" || scale.Replicas < 1 || scale.Replicas > 100 {
		return fmt.Errorf("service scale needs an application, a service, and 1 to 100 replicas")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin service scale transaction: %w", err)
	}
	defer tx.Rollback()
	stamp := scale.UpdatedAt.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO service_scales (application_name, service_name, replicas, updated_at)
		VALUES (?, ?, ?, ?) ON CONFLICT (application_name, service_name) DO UPDATE SET replicas = excluded.replicas,
		updated_at = excluded.updated_at`, scale.Application, scale.Service, scale.Replicas, stamp); err != nil {
		return fmt.Errorf("record service scale: %w", err)
	}
	if previous != scale.Replicas {
		payload, _ := json.Marshal(struct {
			Application string `json:"application"`
			Service     string `json:"service"`
			From        int    `json:"from"`
			To          int    `json:"to"`
			Reason      string `json:"reason"`
		}{scale.Application, scale.Service, previous, scale.Replicas, reason})
		if _, err := tx.ExecContext(ctx, `INSERT INTO events (cluster_id, kind, payload, created_at)
			VALUES ((SELECT id FROM clusters WHERE singleton = 1), ?, ?, ?)`, model.EventServiceScaled, string(payload), stamp); err != nil {
			return fmt.Errorf("record service scale event: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit service scale: %w", err)
	}
	return nil
}

// DeleteServiceScale forgets a service's autoscaler choice, for services
// that were removed or no longer autoscale.
func (s *Store) DeleteServiceScale(ctx context.Context, application, service string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM service_scales WHERE application_name = ? AND service_name = ?`,
		application, service); err != nil {
		return fmt.Errorf("delete service scale: %w", err)
	}
	return nil
}
