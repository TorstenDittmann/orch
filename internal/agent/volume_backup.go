package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TorstenDittmann/orch/internal/agent/state"
	"github.com/TorstenDittmann/orch/internal/model"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
	"github.com/TorstenDittmann/orch/internal/schedule"
)

const (
	// VolumeBackupsDirectory holds volume archives below the agent data
	// directory, as volume-backups/APP/VOLUME/NAME.tar.gz.
	VolumeBackupsDirectory = "volume-backups"
	volumeBackupLayout     = "20060102T150405Z"
	volumeBackupExtension  = ".tar.gz"
	volumeBackupStatusFile = "status.json"
	volumeBackupCheckEvery = time.Minute
	volumeBackupRetryAfter = time.Hour
	volumeBeforeTimeout    = 10 * time.Minute
	// Labels mark backups taken by hand or before a restore. Only unlabeled,
	// scheduled backups are pruned.
	volumeBackupManual        = "manual"
	volumeBackupBeforeRestore = "before-restore"
)

var errVolumeBackupNotFound = errors.New("volume backup was not found")

// VolumeBackup is one archive of a local volume.
type VolumeBackup struct {
	Name      string
	CreatedAt time.Time
	Size      int64
}

// VolumeBackupStatus is the last failure of a volume's scheduled backups. It
// is cleared by the next successful backup.
type VolumeBackupStatus struct {
	FailedAt time.Time `json:"failed_at"`
	Failure  string    `json:"failure"`
}

type volumeBackups struct {
	volumesDir string
	backupsDir string
	supervisor *Supervisor
	cache      *state.Store
	now        func() time.Time
	mu         sync.Mutex
	locks      map[string]*sync.Mutex
	wake       chan struct{}
}

func newVolumeBackups(dataDir string, supervisor *Supervisor, cache *state.Store, now func() time.Time) *volumeBackups {
	if now == nil {
		now = time.Now
	}
	return &volumeBackups{
		volumesDir: filepath.Join(dataDir, VolumesDirectory), backupsDir: filepath.Join(dataDir, VolumeBackupsDirectory),
		supervisor: supervisor, cache: cache, now: now, locks: make(map[string]*sync.Mutex), wake: make(chan struct{}, 1),
	}
}

func (v *volumeBackups) lock(application, volume string) func() {
	v.mu.Lock()
	lock := v.locks[application+"/"+volume]
	if lock == nil {
		lock = &sync.Mutex{}
		v.locks[application+"/"+volume] = lock
	}
	v.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func (v *volumeBackups) volumeDir(application, volume string) string {
	return filepath.Join(v.volumesDir, application, volume)
}

func (v *volumeBackups) archiveDir(application, volume string) string {
	return filepath.Join(v.backupsDir, application, volume)
}

// run takes the scheduled backups of the cached assignment until ctx ends.
// It checks every minute and whenever a new assignment arrives.
func (v *volumeBackups) run(ctx context.Context) {
	ticker := time.NewTicker(volumeBackupCheckEvery)
	defer ticker.Stop()
	for {
		v.runDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-v.wake:
		}
	}
}

// assignmentChanged asks run to check the new assignment's policies now.
func (v *volumeBackups) assignmentChanged() {
	if v == nil {
		return
	}
	select {
	case v.wake <- struct{}{}:
	default:
	}
}

func (v *volumeBackups) runDue(ctx context.Context) {
	assignment, err := v.cache.Assignment(ctx)
	if err != nil {
		return
	}
	for _, allocation := range assignment.Allocations {
		for _, policy := range allocation.VolumeBackups {
			if ctx.Err() != nil {
				return
			}
			due, err := v.due(allocation.Application, policy)
			if err != nil || !due {
				continue
			}
			if _, err := v.backup(ctx, allocation, policy.Volume, policy.Before, ""); err != nil && ctx.Err() == nil {
				log.Printf("agent: back up volume %s/%s: %v", allocation.Application, policy.Volume, err)
			}
			_ = v.prune(allocation.Application, policy.Volume, int(policy.Keep))
		}
	}
}

// due reports whether a scheduled backup should run now: right away when the
// volume has none yet, and otherwise once the schedule fires after the newest
// one. A failed attempt is retried after an hour.
func (v *volumeBackups) due(application string, policy model.VolumeBackupDesired) (bool, error) {
	now := v.now().UTC()
	if status, err := v.status(application, policy.Volume); err == nil && status.FailedAt.Add(volumeBackupRetryAfter).After(now) {
		return false, nil
	}
	backups, err := v.list(application, policy.Volume)
	if err != nil {
		return false, err
	}
	var newest time.Time
	for _, backup := range backups {
		if !strings.Contains(backup.Name, "-") && backup.CreatedAt.After(newest) {
			newest = backup.CreatedAt
		}
	}
	if newest.IsZero() {
		return true, nil
	}
	next, err := schedule.Next(policy.Schedule, newest)
	if err != nil {
		return false, err
	}
	return !now.Before(next), nil
}

// backup archives one volume. A before command runs first in the owning
// allocation's container when it is running; if it fails, there is no backup.
func (v *volumeBackups) backup(ctx context.Context, allocation model.AllocationDesired, volume string, before []string, label string) (VolumeBackup, error) {
	unlock := v.lock(allocation.Application, volume)
	defer unlock()
	result, err := v.backupLocked(ctx, allocation, volume, before, label)
	if label == "" {
		v.recordStatus(allocation.Application, volume, err)
	}
	return result, err
}

func (v *volumeBackups) backupLocked(ctx context.Context, allocation model.AllocationDesired, volume string, before []string, label string) (VolumeBackup, error) {
	source := v.volumeDir(allocation.Application, volume)
	if _, err := os.Lstat(source); err != nil {
		return VolumeBackup{}, fmt.Errorf("volume has no data on this node yet")
	}
	if len(before) != 0 && v.supervisor != nil {
		if err := v.runBefore(ctx, allocation, before); err != nil {
			return VolumeBackup{}, err
		}
	}
	return v.archive(ctx, allocation.Application, volume, source, label)
}

func (v *volumeBackups) runBefore(ctx context.Context, allocation model.AllocationDesired, command []string) error {
	ctx, cancel := context.WithTimeout(ctx, volumeBeforeTimeout)
	defer cancel()
	frames, err := v.supervisor.Exec(ctx, allocation, command)
	if errors.Is(err, orchruntime.ErrNotFound) {
		// A stopped service is not writing, so its volume is safe to copy.
		return nil
	}
	if err != nil {
		return fmt.Errorf("run the before command: %w", err)
	}
	var output []byte
	for frame := range frames {
		output = append(output, frame.Data...)
		if len(output) > 2048 {
			output = output[len(output)-2048:]
		}
		if frame.Exited {
			if frame.ExitCode != 0 {
				return fmt.Errorf("the before command exited with code %d: %s", frame.ExitCode, strings.TrimSpace(string(output)))
			}
			return nil
		}
	}
	if ctx.Err() != nil {
		return fmt.Errorf("the before command did not finish within %s", volumeBeforeTimeout)
	}
	return errors.New("the before command ended without an exit code")
}

func (v *volumeBackups) archive(ctx context.Context, application, volume, source, label string) (VolumeBackup, error) {
	directory := v.archiveDir(application, volume)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return VolumeBackup{}, err
	}
	// Names are unique per second; a second backup within one second takes
	// the next free second.
	createdAt := v.now().UTC().Truncate(time.Second)
	var name, final string
	for attempt := 0; ; attempt++ {
		name = createdAt.Format(volumeBackupLayout)
		if label != "" {
			name += "-" + label
		}
		final = filepath.Join(directory, name+volumeBackupExtension)
		if _, err := os.Lstat(final); errors.Is(err, os.ErrNotExist) {
			break
		}
		if attempt == 60 {
			return VolumeBackup{}, fmt.Errorf("backup %s already exists", name)
		}
		createdAt = createdAt.Add(time.Second)
	}
	scratch, err := os.MkdirTemp(directory, ".partial-")
	if err != nil {
		return VolumeBackup{}, err
	}
	defer os.RemoveAll(scratch)
	staged := filepath.Join(scratch, "archive"+volumeBackupExtension)
	if err := archiveVolume(ctx, source, staged, scratch); err != nil {
		return VolumeBackup{}, fmt.Errorf("archive volume: %w", err)
	}
	if err := os.Rename(staged, final); err != nil {
		return VolumeBackup{}, err
	}
	info, err := os.Stat(final)
	if err != nil {
		return VolumeBackup{}, err
	}
	return VolumeBackup{Name: name, CreatedAt: createdAt, Size: info.Size()}, nil
}

// list returns a volume's archives, newest first.
func (v *volumeBackups) list(application, volume string) ([]VolumeBackup, error) {
	entries, err := os.ReadDir(v.archiveDir(application, volume))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	backups := make([]VolumeBackup, 0, len(entries))
	for _, entry := range entries {
		backup, ok := parseVolumeBackupName(entry.Name())
		if !ok || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		backup.Size = info.Size()
		backups = append(backups, backup)
	}
	sort.Slice(backups, func(i, j int) bool {
		if backups[i].CreatedAt.Equal(backups[j].CreatedAt) {
			return backups[i].Name > backups[j].Name
		}
		return backups[i].CreatedAt.After(backups[j].CreatedAt)
	})
	return backups, nil
}

func parseVolumeBackupName(filename string) (VolumeBackup, bool) {
	name, ok := strings.CutSuffix(filename, volumeBackupExtension)
	if !ok || len(name) < len(volumeBackupLayout) {
		return VolumeBackup{}, false
	}
	createdAt, err := time.Parse(volumeBackupLayout, name[:len(volumeBackupLayout)])
	if err != nil {
		return VolumeBackup{}, false
	}
	switch name[len(volumeBackupLayout):] {
	case "", "-" + volumeBackupManual, "-" + volumeBackupBeforeRestore:
	default:
		return VolumeBackup{}, false
	}
	return VolumeBackup{Name: name, CreatedAt: createdAt.UTC()}, true
}

// path returns the archive of a listed backup.
func (v *volumeBackups) path(application, volume, name string) (string, error) {
	if _, ok := parseVolumeBackupName(name + volumeBackupExtension); !ok {
		return "", errVolumeBackupNotFound
	}
	archive := filepath.Join(v.archiveDir(application, volume), name+volumeBackupExtension)
	if info, err := os.Lstat(archive); err != nil || !info.Mode().IsRegular() {
		return "", errVolumeBackupNotFound
	}
	return archive, nil
}

// prune keeps the newest keep scheduled backups. Labeled backups stay.
func (v *volumeBackups) prune(application, volume string, keep int) error {
	if keep < 1 {
		return nil
	}
	backups, err := v.list(application, volume)
	if err != nil {
		return err
	}
	kept := 0
	for _, backup := range backups {
		if strings.Contains(backup.Name, "-") {
			continue
		}
		if kept < keep {
			kept++
			continue
		}
		if err := os.Remove(filepath.Join(v.archiveDir(application, volume), backup.Name+volumeBackupExtension)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (v *volumeBackups) status(application, volume string) (VolumeBackupStatus, error) {
	contents, err := os.ReadFile(filepath.Join(v.archiveDir(application, volume), volumeBackupStatusFile))
	if err != nil {
		return VolumeBackupStatus{}, err
	}
	var status VolumeBackupStatus
	err = json.Unmarshal(contents, &status)
	return status, err
}

// failures returns the volumes in the assignment whose last scheduled backup
// failed, in application and volume order.
func (v *volumeBackups) failures(assignment model.Assignment) []model.VolumeBackupFailure {
	if v == nil {
		return nil
	}
	var result []model.VolumeBackupFailure
	seen := make(map[string]bool)
	for _, allocation := range assignment.Allocations {
		for _, policy := range allocation.VolumeBackups {
			key := allocation.Application + "\x00" + policy.Volume
			if seen[key] {
				continue
			}
			seen[key] = true
			status, err := v.status(allocation.Application, policy.Volume)
			if err != nil || status.FailedAt.IsZero() {
				continue
			}
			result = append(result, model.VolumeBackupFailure{Application: allocation.Application, Volume: policy.Volume,
				FailedAt: status.FailedAt.UTC(), Message: status.Failure})
		}
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Application != result[right].Application {
			return result[left].Application < result[right].Application
		}
		return result[left].Volume < result[right].Volume
	})
	return result
}

func (v *volumeBackups) recordStatus(application, volume string, backupErr error) {
	path := filepath.Join(v.archiveDir(application, volume), volumeBackupStatusFile)
	if backupErr == nil {
		_ = os.Remove(path)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	contents, err := json.Marshal(VolumeBackupStatus{FailedAt: v.now().UTC(), Failure: boundedMessage(backupErr.Error())})
	if err != nil {
		return
	}
	_ = os.WriteFile(path, contents, 0o600)
}

// restore replaces a volume with one of its backups. The current contents
// are kept as a before-restore backup first, and the owning allocation is
// stopped only while the directories are swapped; reconciliation starts it
// again on the restored data.
func (v *volumeBackups) restore(ctx context.Context, application, volume, name, allocationID string) (VolumeBackup, error) {
	unlock := v.lock(application, volume)
	defer unlock()
	archive, err := v.path(application, volume, name)
	if err != nil {
		return VolumeBackup{}, err
	}
	applicationDir := filepath.Join(v.volumesDir, application)
	if err := os.MkdirAll(applicationDir, 0o700); err != nil {
		return VolumeBackup{}, err
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return VolumeBackup{}, err
	}
	staging := filepath.Join(applicationDir, "."+volume+".restore-"+hex.EncodeToString(suffix))
	replaced := filepath.Join(applicationDir, "."+volume+".replaced-"+hex.EncodeToString(suffix))
	if err := os.Mkdir(staging, 0o700); err != nil {
		return VolumeBackup{}, err
	}
	defer os.RemoveAll(staging)
	if err := extractVolume(ctx, archive, staging); err != nil {
		return VolumeBackup{}, fmt.Errorf("unpack backup %s: %w", name, err)
	}
	live := v.volumeDir(application, volume)
	_, statErr := os.Lstat(live)
	hadData := statErr == nil
	swap := func() error {
		if hadData {
			if err := os.Rename(live, replaced); err != nil {
				return err
			}
		}
		if err := os.Rename(staging, live); err != nil {
			if hadData {
				_ = os.Rename(replaced, live)
			}
			return err
		}
		return nil
	}
	if v.supervisor != nil && allocationID != "" {
		err = v.supervisor.whileStopped(ctx, allocationID, swap)
	} else {
		err = swap()
	}
	if err != nil {
		return VolumeBackup{}, fmt.Errorf("swap in the restored volume: %w", err)
	}
	if !hadData {
		return VolumeBackup{}, nil
	}
	// The swap already happened, so finish keeping the old contents even if
	// the caller goes away. If that fails, the old directory stays in place.
	previous, err := v.archive(context.WithoutCancel(ctx), application, volume, replaced, volumeBackupBeforeRestore)
	if err != nil {
		return VolumeBackup{}, fmt.Errorf("the volume was restored, but archiving its previous contents failed (they are kept in %s): %w", replaced, err)
	}
	if err := os.RemoveAll(replaced); err != nil {
		log.Printf("agent: remove replaced volume contents %s: %v", replaced, err)
	}
	return previous, nil
}
