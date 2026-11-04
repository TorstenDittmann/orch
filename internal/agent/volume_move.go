package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/TorstenDittmann/orch/internal/model"
)

// receive stores one window of a backup another node sends during a volume
// move. The first window starts a new partial file, each next one must
// continue where the file ends, and the last one publishes the archive as a
// backup of this node once it holds size bytes.
func (v *volumeBackups) receive(application, volume, name string, offset uint64, data []byte, size uint64, complete bool) (VolumeBackup, error) {
	backup, ok := parseVolumeBackupName(name + volumeBackupExtension)
	if !ok {
		return VolumeBackup{}, errors.New("invalid backup name")
	}
	if offset+uint64(len(data)) > size {
		return VolumeBackup{}, errors.New("data runs past the backup's size")
	}
	directory := v.archiveDir(application, volume)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return VolumeBackup{}, err
	}
	partial := filepath.Join(directory, ".incoming-"+name+volumeBackupExtension)
	flags := os.O_WRONLY | os.O_APPEND
	if offset == 0 {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	file, err := os.OpenFile(partial, flags, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return VolumeBackup{}, errors.New("the copy of this backup was not started")
	}
	if err != nil {
		return VolumeBackup{}, err
	}
	fail := func(err error) (VolumeBackup, error) {
		_ = file.Close()
		_ = os.Remove(partial)
		return VolumeBackup{}, err
	}
	info, err := file.Stat()
	if err != nil {
		return fail(err)
	}
	if uint64(info.Size()) != offset {
		return fail(fmt.Errorf("the copy holds %d bytes, not %d", info.Size(), offset))
	}
	if _, err := file.Write(data); err != nil {
		return fail(err)
	}
	if !complete {
		return VolumeBackup{}, file.Close()
	}
	if offset+uint64(len(data)) != size {
		return fail(fmt.Errorf("the copy ended after %d of %d bytes", offset+uint64(len(data)), size))
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(partial)
		return VolumeBackup{}, err
	}
	unlock := v.lock(application, volume)
	defer unlock()
	final := filepath.Join(directory, name+volumeBackupExtension)
	if err := os.Rename(partial, final); err != nil {
		_ = os.Remove(partial)
		return VolumeBackup{}, err
	}
	backup.Size = int64(size)
	return backup, nil
}

// stopForMove stops an allocation that has already left this node's
// assignment, so a final backup sees its volume at rest. Reconciliation does
// not start it again because the assignment no longer has it.
func (r *sessionRunner) stopForMove(ctx context.Context, application, allocationID string) (model.AllocationDesired, error) {
	allocation := model.AllocationDesired{Application: application}
	if allocationID == "" || r.supervisor == nil {
		return allocation, nil
	}
	if _, err := r.cachedAllocation(ctx, allocationID); err == nil {
		return allocation, errors.New("the allocation is still assigned to this node")
	}
	return allocation, r.supervisor.whileStopped(ctx, allocationID, nil)
}
