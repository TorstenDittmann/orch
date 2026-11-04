package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/store"
)

const (
	// volumeMoveReleaseTimeout bounds waiting for the old node to stop the
	// service before its final backup.
	volumeMoveReleaseTimeout = 5 * time.Minute
	volumeMovePollInterval   = 500 * time.Millisecond
	volumeCopyWindowTimeout  = 2 * time.Minute
)

// MoveVolume moves a local volume, with every other volume its service
// mounts, to another node. It holds the service off every node, takes a
// final backup of each volume on the old node once the service stopped,
// copies it to the new node through the controller, restores it there and
// claims the volumes on the new node, where the service then starts. When a
// step fails the hold is lifted and the service starts on its old node
// again with its volumes untouched.
func (s *service) MoveVolume(ctx context.Context, request *connect.Request[orchv1alpha1.MoveVolumeRequest]) (*connect.Response[orchv1alpha1.MoveVolumeResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	if request.Msg.GetNode() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a destination node is required"))
	}
	source, err := s.volumeTarget(ctx, request.Msg.GetApplication(), request.Msg.GetVolume())
	if err != nil {
		return nil, err
	}
	if source.service == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("no service mounts volume %q", source.volume))
	}
	application, err := s.state.Application(ctx, source.application)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("read application state"))
	}
	var manifest config.Manifest
	if err := json.Unmarshal(application.CanonicalManifest, &manifest); err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("decode stored manifest"))
	}
	serviceSpec := manifest.Services[source.service]
	destination, err := s.state.NodeByName(ctx, request.Msg.GetNode())
	if errors.Is(err, store.ErrNodeNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("node %q was not found", request.Msg.GetNode()))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("read node"))
	}
	if err := volumeMoveDestinationProblem(source.node, destination, serviceSpec.Constraints); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if !s.sessions.Connected(destination.ID) {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("node %q is not connected", destination.Name))
	}
	for _, node := range []model.Node{source.node, destination} {
		if !s.sessions.volumeMoves(node.ID) {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("the agent on node %q is too old to move volumes; upgrade it with orch node upgrade", node.Name))
		}
	}
	// Every volume of the service moves, since its volumes must share a
	// node. One that was never claimed has no data and is claimed fresh.
	var volumes []string
	for _, mount := range serviceSpec.Mounts {
		claim, err := s.state.VolumeClaim(ctx, source.application, mount.Source)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("read volume claim"))
		}
		if claim == "" {
			continue
		}
		if claim != source.node.ID {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("volumes of service %q are claimed by different nodes", source.service))
		}
		volumes = append(volumes, mount.Source)
	}
	sort.Strings(volumes)
	allocationID := ""
	if allocation, node, err := s.state.DesiredAllocationNode(ctx, source.application, source.service, 1); err == nil {
		if node.ID != source.node.ID {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("service %q is being placed on node %q; try again once it runs", source.service, node.Name))
		}
		allocationID = allocation.ID
	} else if !errors.Is(err, store.ErrAssignmentNotFound) {
		return nil, connect.NewError(connect.CodeInternal, errors.New("read service placement"))
	}

	if err := s.state.BeginVolumeMove(ctx, source.application, source.service, destination.ID, s.now()); err != nil {
		if errors.Is(err, store.ErrVolumeMoveInProgress) {
			return nil, connect.NewError(connect.CodeAborted, fmt.Errorf("the volumes of service %q are already being moved", source.service))
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("record volume move"))
	}
	finished := false
	defer func() {
		if finished {
			return
		}
		// Start the service on its old node again, even if the caller left.
		cleanup := context.WithoutCancel(ctx)
		if err := s.state.CancelVolumeMove(cleanup, source.application, source.service); err == nil {
			s.reconcileAfterMove(cleanup)
		}
	}()
	if err := s.reconcileAfterMove(ctx); err != nil {
		return nil, err
	}
	if err := s.waitServiceReleased(ctx, source); err != nil {
		return nil, err
	}

	response := &orchv1alpha1.MoveVolumeResponse{Service: source.service, FromNode: source.node.Name, ToNode: destination.Name}
	for _, volume := range volumes {
		from, to := source, volumeTarget{application: source.application, volume: volume, service: source.service, node: destination, placed: true}
		from.volume = volume
		backup, err := s.moveOneVolume(ctx, from, to, allocationID)
		if err != nil {
			return nil, err
		}
		response.Volumes = append(response.Volumes, &orchv1alpha1.MovedVolume{Volume: volume, Backup: backup})
	}
	if err := s.state.FinishVolumeMove(ctx, source.application, source.service, volumes, s.now()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	finished = true
	if err := s.reconcileAfterMove(ctx); err != nil {
		return nil, err
	}
	if s.offsite != nil {
		s.offsite.kick()
	}
	return connect.NewResponse(response), nil
}

// volumeMoveDestinationProblem says why a service's volumes cannot move to
// destination, or nil.
func volumeMoveDestinationProblem(from, destination model.Node, constraints map[string]string) error {
	if destination.ID == from.ID {
		return fmt.Errorf("the volume is already on node %q", destination.Name)
	}
	if destination.Lifecycle != model.NodeLifecycleActive {
		return fmt.Errorf("node %q is %s; uncordon it first or pick an active node", destination.Name, strings.ToLower(destination.Lifecycle))
	}
	keys := make([]string, 0, len(constraints))
	for key := range constraints {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if destination.Labels[key] != constraints[key] {
			return fmt.Errorf("the service requires label %s=%q, which node %q does not have", key, constraints[key], destination.Name)
		}
	}
	return nil
}

func (s *service) reconcileAfterMove(ctx context.Context) error {
	if s.reconciler == nil {
		return nil
	}
	if err := s.reconciler.Reconcile(ctx); err != nil {
		return connect.NewError(connect.CodeInternal, errors.New("reconcile the volume move"))
	}
	return nil
}

// waitServiceReleased waits until the old node acknowledged an assignment
// without the service, which it applies before acknowledging.
func (s *service) waitServiceReleased(ctx context.Context, source volumeTarget) error {
	ctx, cancel := context.WithTimeout(ctx, volumeMoveReleaseTimeout)
	defer cancel()
	ticker := time.NewTicker(volumeMovePollInterval)
	defer ticker.Stop()
	for {
		released, err := s.serviceReleased(ctx, source)
		if err != nil && ctx.Err() == nil {
			return err
		}
		if released {
			return nil
		}
		select {
		case <-ctx.Done():
			return connect.NewError(connect.CodeDeadlineExceeded,
				fmt.Errorf("node %q did not stop service %q in time; nothing was moved", source.node.Name, source.service))
		case <-ticker.C:
		}
	}
}

func (s *service) serviceReleased(ctx context.Context, source volumeTarget) (bool, error) {
	assignment, err := s.state.CurrentAssignment(ctx, source.node.ID)
	if errors.Is(err, store.ErrAssignmentNotFound) {
		return true, nil
	}
	if err != nil {
		return false, connect.NewError(connect.CodeInternal, errors.New("read node assignment"))
	}
	for _, allocation := range assignment.Allocations {
		if allocation.Application == source.application && allocation.Service == source.service {
			return false, nil
		}
	}
	node, err := s.state.NodeByID(ctx, source.node.ID)
	if err != nil {
		return false, connect.NewError(connect.CodeInternal, errors.New("read node"))
	}
	if !s.sessions.Connected(node.ID) {
		return false, connect.NewError(connect.CodeUnavailable, fmt.Errorf("node %q disconnected; nothing was moved", node.Name))
	}
	return node.AcknowledgedGeneration >= assignment.Generation, nil
}

// moveOneVolume takes the final backup of one volume on its old node,
// copies it to the new node and restores it there.
func (s *service) moveOneVolume(ctx context.Context, from, to volumeTarget, allocationID string) (*orchv1alpha1.VolumeBackupInfo, error) {
	var backup *orchv1alpha1.VolumeBackupInfo
	backupContext, cancel := context.WithTimeout(ctx, volumeOperationTimeout)
	err := s.volumeOperation(backupContext, from, &orchv1alpha1.VolumeCommand{
		Action: orchv1alpha1.VolumeAction_VOLUME_ACTION_BACKUP, AllocationId: allocationID,
	}, func(result *orchv1alpha1.VolumeResponse) error {
		backup = result.GetBackup()
		return nil
	})
	cancel()
	if err != nil {
		return nil, wrapMoveError(err, "back up %s on node %q", from.volume, from.node.Name)
	}
	if backup.GetName() == "" {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("node %q returned no backup of %s", from.node.Name, from.volume))
	}
	if err := s.copyVolumeBackup(ctx, from, to, backup.GetName()); err != nil {
		return nil, wrapMoveError(err, "copy %s to node %q", from.volume, to.node.Name)
	}
	restoreContext, cancel := context.WithTimeout(ctx, volumeOperationTimeout)
	defer cancel()
	if err := s.volumeOperation(restoreContext, to, &orchv1alpha1.VolumeCommand{
		Action: orchv1alpha1.VolumeAction_VOLUME_ACTION_RESTORE, Backup: backup.GetName(),
	}, func(*orchv1alpha1.VolumeResponse) error { return nil }); err != nil {
		return nil, wrapMoveError(err, "restore %s on node %q", from.volume, to.node.Name)
	}
	backup.OnNode = true
	return backup, nil
}

// wrapMoveError says which step of a move failed, keeps the error's code,
// and notes that the service goes back to its old node.
func wrapMoveError(err error, format string, args ...any) error {
	step := fmt.Sprintf(format, args...)
	return connect.NewError(connect.CodeOf(err), fmt.Errorf("%s: %s; nothing was moved and the service starts on its old node again", step, errorMessage(err)))
}

func errorMessage(err error) string {
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		return connectErr.Message()
	}
	return err.Error()
}

// copyVolumeBackup relays a backup from one node to another one read
// window at a time, so neither side holds more than a window in memory.
func (s *service) copyVolumeBackup(ctx context.Context, from, to volumeTarget, name string) error {
	var offset uint64
	for {
		var window []byte
		var size uint64
		readContext, cancel := context.WithTimeout(ctx, volumeCopyWindowTimeout)
		err := s.volumeOperation(readContext, from, &orchv1alpha1.VolumeCommand{
			Action: orchv1alpha1.VolumeAction_VOLUME_ACTION_READ, Backup: name, Offset: offset,
		}, func(result *orchv1alpha1.VolumeResponse) error {
			size = result.GetSizeBytes()
			window = append(window, result.GetData()...)
			return nil
		})
		cancel()
		if err != nil {
			return err
		}
		end := offset + uint64(len(window))
		complete := end >= size
		if len(window) == 0 && !complete {
			return connect.NewError(connect.CodeInternal, errors.New("the backup stopped growing while it was copied"))
		}
		writeContext, cancel := context.WithTimeout(ctx, volumeCopyWindowTimeout)
		err = s.volumeOperation(writeContext, to, &orchv1alpha1.VolumeCommand{
			Action: orchv1alpha1.VolumeAction_VOLUME_ACTION_WRITE, Backup: name, Offset: offset, Data: window,
			SizeBytes: size, Complete: complete,
		}, func(*orchv1alpha1.VolumeResponse) error { return nil })
		cancel()
		if err != nil {
			return err
		}
		if complete {
			return nil
		}
		offset = end
	}
}
