package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SetNodeOSUpdateRestore records the lifecycle a node returns to once its
// agent reconnects with a boot id other than bootID, after the reboot of an
// OS update. An empty lifecycle clears the record.
func (s *Store) SetNodeOSUpdateRestore(ctx context.Context, nodeID, lifecycle, bootID string) error {
	if lifecycle == "" {
		bootID = ""
	}
	result, err := s.db.ExecContext(ctx, `UPDATE nodes SET os_update_lifecycle = ?, os_update_boot_id = ?
		WHERE id = ? AND lifecycle != 'REMOVED'`, lifecycle, bootID, nodeID)
	if err != nil {
		return fmt.Errorf("record node OS update: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return ErrNodeNotFound
	}
	return nil
}

// TakeNodeOSUpdateRestore returns and clears the lifecycle recorded by
// SetNodeOSUpdateRestore once the node's agent runs with a boot id other than
// the one that rebooted.
func (s *Store) TakeNodeOSUpdateRestore(ctx context.Context, nodeID, bootID string) (name, lifecycle string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", fmt.Errorf("begin node OS update: %w", err)
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT name, os_update_lifecycle FROM nodes
		WHERE id = ? AND lifecycle != 'REMOVED' AND os_update_lifecycle != '' AND os_update_boot_id != ?`, nodeID, bootID).Scan(&name, &lifecycle)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read node OS update: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET os_update_lifecycle = '', os_update_boot_id = '' WHERE id = ?`, nodeID); err != nil {
		return "", "", fmt.Errorf("clear node OS update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("commit node OS update: %w", err)
	}
	return name, lifecycle, nil
}
