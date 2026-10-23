package control

import (
	"context"
	"log"
	"sync"
	"time"

	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/store"
)

const (
	// DefaultBackupKeep is how many daily controller backups are kept.
	DefaultBackupKeep  = 7
	backupInterval     = 24 * time.Hour
	backupCheckEvery   = time.Hour
	backupRetryBackoff = time.Hour
)

// backupScheduler writes a controller backup set once a day into
// DATA_DIR/backups and keeps the newest few. It needs no configuration: a
// fresh controller is backed up on its first start.
type backupScheduler struct {
	state   *store.Store
	dataDir string
	keep    int
	now     func() time.Time
	// written is called after each new backup set, to copy it off the host.
	written func()

	mu       sync.Mutex
	latest   clusterctl.BackupSet
	failedAt time.Time
	failure  string
}

type backupStatus struct {
	Latest   clusterctl.BackupSet
	FailedAt time.Time
	Failure  string
}

func newBackupScheduler(state *store.Store, dataDir string, keep int, now func() time.Time) *backupScheduler {
	scheduler := &backupScheduler{state: state, dataDir: dataDir, keep: keep, now: now}
	if sets, err := clusterctl.ListBackupSets(dataDir); err == nil && len(sets) > 0 {
		scheduler.latest = sets[0]
	}
	return scheduler
}

func (b *backupScheduler) status() backupStatus {
	if b == nil {
		return backupStatus{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return backupStatus{Latest: b.latest, FailedAt: b.failedAt, Failure: b.failure}
}

// due reports whether a backup should run now: none exists, the newest is a
// day old, and the last failure is not too recent to retry.
func (b *backupScheduler) due(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.failedAt.IsZero() && now.Sub(b.failedAt) < backupRetryBackoff {
		return false
	}
	return b.latest.CreatedAt.IsZero() || now.Sub(b.latest.CreatedAt) >= backupInterval
}

// runOnce takes a backup when one is due and prunes old sets.
func (b *backupScheduler) runOnce(ctx context.Context) {
	now := b.now()
	if !b.due(now) {
		return
	}
	set, err := clusterctl.WriteBackupSet(ctx, b.state, b.dataDir, now)
	b.mu.Lock()
	if err != nil {
		b.failedAt, b.failure = now.UTC(), err.Error()
	} else {
		b.latest, b.failedAt, b.failure = set, time.Time{}, ""
	}
	b.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("controller: automatic backup failed: %v", err)
		}
		return
	}
	if err := clusterctl.PruneBackupSets(b.dataDir, b.keep); err != nil {
		log.Printf("controller: prune old backups: %v", err)
	}
	if b.written != nil {
		b.written()
	}
}

func (b *backupScheduler) run(ctx context.Context) {
	b.runOnce(ctx)
	ticker := time.NewTicker(backupCheckEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.runOnce(ctx)
		}
	}
}
