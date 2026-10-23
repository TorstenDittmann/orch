package agent

import (
	"context"
	"errors"
	"io"
	"os"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/model"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// volumeReadWindow is how much of an archive one READ returns.
const volumeReadWindow = 1 << 20

func validVolumeName(value string) bool {
	if value == "" || len(value) > 63 || value[0] < 'a' || value[0] > 'z' || value[len(value)-1] == '-' {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

func (r *sessionRunner) runVolume(ctx context.Context, command *orchv1alpha1.VolumeCommand, outbound chan<- *orchv1alpha1.ConnectRequest) {
	application, volume := command.GetApplication(), command.GetVolume()
	send := func(response *orchv1alpha1.VolumeResponse) bool {
		response.RequestId = command.GetRequestId()
		select {
		case <-ctx.Done():
			return false
		case outbound <- operationResult(&orchv1alpha1.AgentOperationResult_Volume{Volume: response}):
			return true
		}
	}
	fail := func(err error) {
		code := "failed"
		if errors.Is(err, errVolumeBackupNotFound) {
			code = "not_found"
		}
		enqueueOperationError(ctx, outbound, command.GetRequestId(), code, boundedMessage(err.Error()))
	}
	switch command.GetAction() {
	case orchv1alpha1.VolumeAction_VOLUME_ACTION_LIST:
		backups, err := r.volumes.list(application, volume)
		if err != nil {
			fail(err)
			return
		}
		response := &orchv1alpha1.VolumeResponse{Complete: true}
		for _, backup := range backups {
			response.Backups = append(response.Backups, volumeBackupMessage(backup))
		}
		if status, err := r.volumes.status(application, volume); err == nil {
			response.FailedAt = timestamppb.New(status.FailedAt)
			response.Failure = status.Failure
		}
		send(response)
	case orchv1alpha1.VolumeAction_VOLUME_ACTION_BACKUP:
		allocation, before := r.volumeOwner(ctx, application, volume)
		backup, err := r.volumes.backup(ctx, allocation, volume, before, volumeBackupManual)
		if err != nil {
			fail(err)
			return
		}
		send(&orchv1alpha1.VolumeResponse{Backup: volumeBackupMessage(backup), Complete: true})
	case orchv1alpha1.VolumeAction_VOLUME_ACTION_RESTORE:
		owner, _ := r.volumeOwner(ctx, application, volume)
		previous, err := r.volumes.restore(ctx, application, volume, command.GetBackup(), owner.ID)
		if err != nil {
			fail(err)
			return
		}
		response := &orchv1alpha1.VolumeResponse{Complete: true}
		if previous.Name != "" {
			response.Backup = volumeBackupMessage(previous)
		}
		send(response)
	case orchv1alpha1.VolumeAction_VOLUME_ACTION_READ:
		r.readVolumeBackup(ctx, command, send, fail)
	case orchv1alpha1.VolumeAction_VOLUME_ACTION_UPLOAD:
		etags, err := r.volumes.upload(ctx, application, volume, command.GetBackup(), command.GetUrls(), command.GetPartSize())
		if err != nil {
			fail(err)
			return
		}
		send(&orchv1alpha1.VolumeResponse{Etags: etags, Complete: true})
	case orchv1alpha1.VolumeAction_VOLUME_ACTION_FETCH:
		if len(command.GetUrls()) != 1 {
			fail(errors.New("fetch needs one URL"))
			return
		}
		backup, err := r.volumes.fetch(ctx, application, volume, command.GetBackup(), command.GetUrls()[0])
		if err != nil {
			fail(err)
			return
		}
		send(&orchv1alpha1.VolumeResponse{Backup: volumeBackupMessage(backup), Complete: true})
	default:
		fail(errors.New("unsupported volume action"))
	}
}

func (r *sessionRunner) readVolumeBackup(ctx context.Context, command *orchv1alpha1.VolumeCommand,
	send func(*orchv1alpha1.VolumeResponse) bool, fail func(error),
) {
	path, err := r.volumes.path(command.GetApplication(), command.GetVolume(), command.GetBackup())
	if err != nil {
		fail(err)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		fail(errVolumeBackupNotFound)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		fail(err)
		return
	}
	size := uint64(info.Size())
	offset := command.GetOffset()
	if offset > size {
		fail(errors.New("read offset is past the end of the backup"))
		return
	}
	reader := io.NewSectionReader(file, int64(offset), int64(min(size-offset, volumeReadWindow)))
	buffer := make([]byte, maxOperationChunk)
	for {
		count, readErr := reader.Read(buffer)
		finished := errors.Is(readErr, io.EOF)
		if readErr != nil && !finished {
			fail(readErr)
			return
		}
		if count == 0 && !finished {
			continue
		}
		if !send(&orchv1alpha1.VolumeResponse{Data: append([]byte(nil), buffer[:count]...), SizeBytes: size, Complete: finished}) || finished {
			return
		}
	}
}

// volumeOwner returns the allocation that mounts a volume read-write and the
// volume's before command, or an empty allocation when none runs here.
func (r *sessionRunner) volumeOwner(ctx context.Context, application, volume string) (model.AllocationDesired, []string) {
	owner := model.AllocationDesired{Application: application}
	assignment, err := r.cache.Assignment(ctx)
	if err != nil {
		return owner, nil
	}
	for _, allocation := range assignment.Allocations {
		if allocation.Application != application {
			continue
		}
		for _, mount := range allocation.Mounts {
			if mount.Name == volume && !mount.ReadOnly {
				for _, policy := range allocation.VolumeBackups {
					if policy.Volume == volume {
						return allocation, policy.Before
					}
				}
				return allocation, nil
			}
		}
	}
	return owner, nil
}

func (r *sessionRunner) runRestart(ctx context.Context, command *orchv1alpha1.RestartCommand, outbound chan<- *orchv1alpha1.ConnectRequest) {
	if _, err := r.cachedAllocation(ctx, command.GetAllocationId()); err != nil {
		enqueueOperationError(ctx, outbound, command.GetRequestId(), "not_found", "allocation is not in the cached assignment")
		return
	}
	if err := r.supervisor.Restart(ctx, command.GetAllocationId()); err != nil {
		code := "failed"
		if errors.Is(err, orchruntime.ErrNotFound) {
			code = "not_found"
		}
		enqueueOperationError(ctx, outbound, command.GetRequestId(), code, boundedMessage(err.Error()))
		return
	}
	select {
	case <-ctx.Done():
	case outbound <- operationResult(&orchv1alpha1.AgentOperationResult_Restart{Restart: &orchv1alpha1.RestartResponse{
		RequestId: command.GetRequestId(), StoppedAt: timestamppb.New(r.supervisor.now().UTC()),
	}}):
	}
}

func volumeBackupMessage(backup VolumeBackup) *orchv1alpha1.VolumeBackupInfo {
	return &orchv1alpha1.VolumeBackupInfo{Name: backup.Name, CreatedAt: timestamppb.New(backup.CreatedAt), SizeBytes: uint64(backup.Size)}
}
