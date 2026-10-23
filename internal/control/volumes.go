package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	volumeListTimeout = 5 * time.Second
	// volumeBucketListTimeout bounds listing the bucket's copies, so a slow
	// bucket only delays volume listings a little.
	volumeBucketListTimeout = 10 * time.Second
	volumeOperationTimeout  = time.Hour
	restartReadyTimeout     = 2 * time.Minute
	restartPollInterval     = time.Second
)

// volumeTarget is a declared local volume and the node that holds it.
type volumeTarget struct {
	application string
	volume      string
	service     string
	backup      *config.VolumeBackup
	node        model.Node
	placed      bool
}

func (s *service) volumeTargets(ctx context.Context, application model.Application, only string) ([]volumeTarget, error) {
	var manifest config.Manifest
	if err := json.Unmarshal(application.CanonicalManifest, &manifest); err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("decode stored manifest"))
	}
	names := make([]string, 0, len(manifest.Volumes))
	for name := range manifest.Volumes {
		if only == "" || name == only {
			names = append(names, name)
		}
	}
	if only != "" && len(names) == 0 {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("application %q has no volume %q", application.Name, only))
	}
	sort.Strings(names)
	targets := make([]volumeTarget, 0, len(names))
	for _, name := range names {
		target := volumeTarget{application: application.Name, volume: name, backup: manifest.Volumes[name].Backup}
		for serviceName, item := range manifest.Services {
			for _, mount := range item.Mounts {
				if mount.Source == name {
					target.service = serviceName
				}
			}
		}
		if target.service != "" {
			if _, node, err := s.state.DesiredAllocationNode(ctx, application.Name, target.service, 1); err == nil {
				target.node, target.placed = node, true
			} else if !errors.Is(err, store.ErrAssignmentNotFound) {
				return nil, connect.NewError(connect.CodeInternal, errors.New("read volume owner"))
			}
		}
		if !target.placed {
			nodeID, err := s.state.VolumeClaim(ctx, application.Name, name)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, errors.New("read volume claim"))
			}
			if nodeID != "" {
				if node, err := s.state.NodeByID(ctx, nodeID); err == nil {
					target.node, target.placed = node, true
				}
			}
		}
		targets = append(targets, target)
	}
	return targets, nil
}

func (s *service) volumeTarget(ctx context.Context, applicationName, volume string) (volumeTarget, error) {
	if err := config.ValidateApplicationName(applicationName); err != nil {
		return volumeTarget{}, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := config.ValidateApplicationName(volume); err != nil {
		return volumeTarget{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid volume name: %w", err))
	}
	application, err := s.state.Application(ctx, applicationName)
	if errors.Is(err, store.ErrApplicationNotFound) {
		return volumeTarget{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("application %q was not found", applicationName))
	}
	if err != nil {
		return volumeTarget{}, connect.NewError(connect.CodeInternal, errors.New("read application state"))
	}
	targets, err := s.volumeTargets(ctx, application, volume)
	if err != nil {
		return volumeTarget{}, err
	}
	target := targets[0]
	if !target.placed {
		return target, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("volume %q is not on a node yet", volume))
	}
	if s.sessions == nil || !s.sessions.Connected(target.node.ID) {
		// The target comes along, so a caller can read the bucket instead.
		return target, connect.NewError(connect.CodeUnavailable, fmt.Errorf("node %q that holds volume %q is not connected", target.node.Name, volume))
	}
	return target, nil
}

// volumeOperation sends one volume command to the volume's node and passes
// every response to handle until the agent marks one complete.
func (s *service) volumeOperation(ctx context.Context, target volumeTarget, command *orchv1alpha1.VolumeCommand, handle func(*orchv1alpha1.VolumeResponse) error) error {
	_, frames, cancel, err := s.sessions.beginOperation(ctx, target.node.ID, func(requestID string) *orchv1alpha1.ConnectResponse {
		message := &orchv1alpha1.VolumeCommand{
			RequestId: requestID, Action: command.GetAction(), Application: target.application, Volume: target.volume,
			Backup: command.GetBackup(), Offset: command.GetOffset(), Urls: command.GetUrls(), PartSize: command.GetPartSize(),
		}
		return &orchv1alpha1.ConnectResponse{Message: &orchv1alpha1.ConnectResponse_Volume{Volume: message}}
	})
	if err != nil {
		return operationConnectError(err)
	}
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return connect.NewError(connect.CodeDeadlineExceeded, errors.New("volume request timed out"))
		case result, ok := <-frames:
			if !ok {
				return connect.NewError(connect.CodeUnavailable, errAgentDisconnected)
			}
			switch value := result.GetResult().(type) {
			case *orchv1alpha1.AgentOperationResult_Volume:
				if err := handle(value.Volume); err != nil {
					return err
				}
				if value.Volume.GetComplete() {
					return nil
				}
			case *orchv1alpha1.AgentOperationResult_Error:
				return agentOperationError(value.Error)
			default:
				return connect.NewError(connect.CodeInternal, errors.New("agent returned the wrong operation frame"))
			}
		}
	}
}

func (s *service) ListVolumes(ctx context.Context, request *connect.Request[orchv1alpha1.ListVolumesRequest]) (*connect.Response[orchv1alpha1.ListVolumesResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	name, only := request.Msg.GetApplication(), request.Msg.GetVolume()
	if name == "" && only != "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a volume filter needs an application"))
	}
	if only != "" {
		if err := config.ValidateApplicationName(only); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid volume name: %w", err))
		}
	}
	var applications []model.Application
	if name != "" {
		if err := config.ValidateApplicationName(name); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		application, err := s.state.Application(ctx, name)
		if errors.Is(err, store.ErrApplicationNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("application %q was not found", name))
		}
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("read application state"))
		}
		applications = []model.Application{application}
	} else {
		var err error
		if applications, err = s.state.Applications(ctx, false); err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("read applications"))
		}
	}
	// One listing of the bucket covers every volume asked for. It runs
	// while the nodes list their own backups.
	type bucketListing struct {
		backups map[string][]*orchv1alpha1.VolumeBackupInfo
		failure string
	}
	listed := make(chan bucketListing, 1)
	go func() {
		var result bucketListing
		if client, err := s.offsiteClient(ctx); err != nil {
			result.failure = "read backup destination"
		} else if client != nil {
			if result.backups, err = remoteBackups(ctx, client, name, only, volumeBucketListTimeout); err != nil {
				result.failure = boundedFailure(err.Error())
			}
		}
		listed <- result
	}()
	type pendingMerge struct {
		summary *orchv1alpha1.VolumeSummary
		key     string
		local   []*orchv1alpha1.VolumeBackupInfo
	}
	var pending []pendingMerge
	response := &orchv1alpha1.ListVolumesResponse{}
	for _, application := range applications {
		targets, err := s.volumeTargets(ctx, application, only)
		if err != nil {
			<-listed
			return nil, err
		}
		for _, target := range targets {
			summary := &orchv1alpha1.VolumeSummary{Application: target.application, Volume: target.volume, Service: target.service}
			if target.backup != nil {
				summary.BackupSchedule, summary.BackupKeep = target.backup.Schedule, uint32(target.backup.Keep)
			}
			item := pendingMerge{summary: summary, key: target.application + "/" + target.volume}
			if target.placed {
				summary.Node = target.node.Name
				item.local = s.localBackupsForListing(ctx, target, summary)
				if claimedAt, err := s.state.VolumeClaimedAt(ctx, target.application, target.volume); err == nil && !claimedAt.IsZero() {
					summary.ClaimedAt = timestamppb.New(claimedAt)
				}
			}
			// A volume not on a node, such as one whose node was lost, still
			// lists the bucket's backups.
			pending = append(pending, item)
			response.Volumes = append(response.Volumes, summary)
		}
	}
	bucket := <-listed
	for _, summary := range response.Volumes {
		summary.BucketError = bucket.failure
	}
	for _, item := range pending {
		remote := bucket.backups[item.key]
		if !item.summary.Reachable && len(remote) == 0 {
			continue
		}
		item.summary.Backups = mergeVolumeBackups(item.local, remote)
		item.summary.BackupCount = uint32(len(item.summary.Backups))
		if only == "" && len(item.summary.Backups) > 1 {
			item.summary.Backups = item.summary.Backups[:1]
		}
	}
	return connect.NewResponse(response), nil
}

// localBackupsForListing asks a volume's node for its backups and records
// on summary whether it answered and how its last scheduled backup went.
func (s *service) localBackupsForListing(ctx context.Context, target volumeTarget, summary *orchv1alpha1.VolumeSummary) []*orchv1alpha1.VolumeBackupInfo {
	if s.sessions == nil || !s.sessions.Connected(target.node.ID) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, volumeListTimeout)
	defer cancel()
	var local []*orchv1alpha1.VolumeBackupInfo
	err := s.volumeOperation(ctx, target, &orchv1alpha1.VolumeCommand{Action: orchv1alpha1.VolumeAction_VOLUME_ACTION_LIST},
		func(response *orchv1alpha1.VolumeResponse) error {
			local = response.GetBackups()
			summary.BackupFailedAt, summary.BackupFailure = response.GetFailedAt(), response.GetFailure()
			return nil
		})
	summary.Reachable = err == nil
	return local
}

func (s *service) BackupVolume(ctx context.Context, request *connect.Request[orchv1alpha1.BackupVolumeRequest]) (*connect.Response[orchv1alpha1.BackupVolumeResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	target, err := s.volumeTarget(ctx, request.Msg.GetApplication(), request.Msg.GetVolume())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, volumeOperationTimeout)
	defer cancel()
	response := &orchv1alpha1.BackupVolumeResponse{}
	if err := s.volumeOperation(ctx, target, &orchv1alpha1.VolumeCommand{Action: orchv1alpha1.VolumeAction_VOLUME_ACTION_BACKUP},
		func(result *orchv1alpha1.VolumeResponse) error {
			response.Backup = result.GetBackup()
			return nil
		}); err != nil {
		return nil, err
	}
	s.offsite.kick()
	return connect.NewResponse(response), nil
}

func (s *service) RestoreVolume(ctx context.Context, request *connect.Request[orchv1alpha1.RestoreVolumeRequest]) (*connect.Response[orchv1alpha1.RestoreVolumeResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	if request.Msg.GetBackup() == "" || len(request.Msg.GetBackup()) > 64 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("backup name is required"))
	}
	target, err := s.volumeTarget(ctx, request.Msg.GetApplication(), request.Msg.GetVolume())
	if err != nil {
		return nil, err
	}
	response := &orchv1alpha1.RestoreVolumeResponse{}
	restore := func(urls []string, limit time.Duration) error {
		ctx, cancel := context.WithTimeout(ctx, limit)
		defer cancel()
		return s.volumeOperation(ctx, target, &orchv1alpha1.VolumeCommand{
			Action: orchv1alpha1.VolumeAction_VOLUME_ACTION_RESTORE, Backup: request.Msg.GetBackup(), Urls: urls,
		}, func(result *orchv1alpha1.VolumeResponse) error {
			response.Previous = result.GetBackup()
			return nil
		})
	}
	err = restore(nil, volumeOperationTimeout)
	if connect.CodeOf(err) == connect.CodeNotFound {
		// The node does not have it; the bucket may. The node downloads it
		// and restores it in one step, with time for the download.
		source, sourceErr := s.bucketSource(ctx, target, request.Msg.GetBackup())
		if sourceErr != nil {
			return nil, sourceErr
		}
		if source != "" {
			err = restore([]string{source}, offsiteTransferLimit+volumeOperationTimeout)
		}
	}
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(response), nil
}

// DownloadVolumeBackup streams an archive in windows the agent reads one at a
// time, so a slow client slows the agent down instead of overflowing it.
func (s *service) DownloadVolumeBackup(ctx context.Context, request *connect.Request[orchv1alpha1.DownloadVolumeBackupRequest], stream *connect.ServerStream[orchv1alpha1.DownloadVolumeBackupResponse]) error {
	if err := requireAdministrator(ctx); err != nil {
		return err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return err
	}
	if request.Msg.GetBackup() == "" || len(request.Msg.GetBackup()) > 64 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("backup name is required"))
	}
	target, err := s.volumeTarget(ctx, request.Msg.GetApplication(), request.Msg.GetVolume())
	if code := connect.CodeOf(err); target.application != "" && (code == connect.CodeUnavailable || code == connect.CodeFailedPrecondition) {
		// The node is offline or gone; the bucket may still have the backup.
		body, size, bucketErr := s.openBucketBackup(ctx, target, request.Msg.GetBackup())
		if bucketErr != nil {
			return bucketErr
		}
		if body == nil {
			return err
		}
		defer body.Close()
		return streamBucketBackup(body, size, stream)
	}
	if err != nil {
		return err
	}
	var offset uint64
	for {
		size := uint64(0)
		windowContext, cancel := context.WithTimeout(ctx, time.Minute)
		err := s.volumeOperation(windowContext, target, &orchv1alpha1.VolumeCommand{
			Action: orchv1alpha1.VolumeAction_VOLUME_ACTION_READ, Backup: request.Msg.GetBackup(), Offset: offset,
		}, func(result *orchv1alpha1.VolumeResponse) error {
			size = result.GetSizeBytes()
			if len(result.GetData()) == 0 {
				return nil
			}
			offset += uint64(len(result.GetData()))
			return stream.Send(&orchv1alpha1.DownloadVolumeBackupResponse{Data: result.GetData(), SizeBytes: size})
		})
		cancel()
		if connect.CodeOf(err) == connect.CodeNotFound && offset == 0 {
			// A backup only the bucket has streams straight from it.
			body, bucketSize, bucketErr := s.openBucketBackup(ctx, target, request.Msg.GetBackup())
			if bucketErr != nil {
				return bucketErr
			}
			if body != nil {
				defer body.Close()
				return streamBucketBackup(body, bucketSize, stream)
			}
		}
		if err != nil {
			return err
		}
		if offset >= size {
			return nil
		}
	}
}

// bucketStreamChunk matches the agents' download frame size.
const bucketStreamChunk = 64 << 10

func streamBucketBackup(body io.Reader, size int64, stream *connect.ServerStream[orchv1alpha1.DownloadVolumeBackupResponse]) error {
	buffer := make([]byte, bucketStreamChunk)
	var sent int64
	for {
		count, err := io.ReadFull(body, buffer)
		sent += int64(count)
		if count > 0 {
			if sendErr := stream.Send(&orchv1alpha1.DownloadVolumeBackupResponse{Data: append([]byte(nil), buffer[:count]...),
				SizeBytes: uint64(size)}); sendErr != nil {
				return sendErr
			}
		}
		if (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) && sent == size {
			return nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				err = fmt.Errorf("got %d of %d bytes", sent, size)
			}
			return connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the backup from the bucket: %w", err))
		}
	}
}

// RestartService restarts a service's replicas one at a time. Each replica
// must run again before the next one stops.
func (s *service) RestartService(ctx context.Context, request *connect.Request[orchv1alpha1.RestartServiceRequest]) (*connect.Response[orchv1alpha1.RestartServiceResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	name, serviceName := request.Msg.GetApplication(), request.Msg.GetService()
	if err := config.ValidateApplicationName(name); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	application, err := s.state.Application(ctx, name)
	if errors.Is(err, store.ErrApplicationNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("application %q was not found", name))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("read application state"))
	}
	var manifest config.Manifest
	if err := json.Unmarshal(application.CanonicalManifest, &manifest); err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("decode stored manifest"))
	}
	services := make([]string, 0, len(manifest.Services))
	for item := range manifest.Services {
		if serviceName == "" || item == serviceName {
			services = append(services, item)
		}
	}
	if len(services) == 0 {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("application %q has no service %q", name, serviceName))
	}
	sort.Strings(services)
	response := &orchv1alpha1.RestartServiceResponse{}
	for _, item := range services {
		replicas := make([]uint32, 0, manifest.Services[item].Replicas)
		if replica := request.Msg.GetReplica(); replica != 0 {
			if serviceName == "" {
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a replica needs a service"))
			}
			replicas = append(replicas, replica)
		} else {
			for replica := 1; replica <= manifest.Services[item].Replicas; replica++ {
				replicas = append(replicas, uint32(replica))
			}
		}
		for _, replica := range replicas {
			allocation, node, err := s.operationTarget(ctx, name, item, replica)
			if err != nil {
				return nil, err
			}
			if err := s.restartAllocation(ctx, allocation, node); err != nil {
				return nil, err
			}
			if serviceName != "" {
				response.Replicas = append(response.Replicas, replica)
			}
		}
	}
	return connect.NewResponse(response), nil
}

func (s *service) restartAllocation(ctx context.Context, allocation model.AllocationDesired, node model.Node) error {
	operationContext, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	_, frames, cancelOperation, err := s.sessions.beginOperation(operationContext, node.ID, func(requestID string) *orchv1alpha1.ConnectResponse {
		return &orchv1alpha1.ConnectResponse{Message: &orchv1alpha1.ConnectResponse_Restart{Restart: &orchv1alpha1.RestartCommand{
			RequestId: requestID, AllocationId: allocation.ID,
		}}}
	})
	if err != nil {
		return operationConnectError(err)
	}
	defer cancelOperation()
	select {
	case <-operationContext.Done():
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("restart request timed out"))
	case result, ok := <-frames:
		if !ok {
			return connect.NewError(connect.CodeUnavailable, errAgentDisconnected)
		}
		switch value := result.GetResult().(type) {
		case *orchv1alpha1.AgentOperationResult_Restart:
			// Compare against the agent's own clock, which stamps its reports.
			return s.waitRunningAgain(ctx, allocation, value.Restart.GetStoppedAt().AsTime())
		case *orchv1alpha1.AgentOperationResult_Error:
			return agentOperationError(value.Error)
		default:
			return connect.NewError(connect.CodeInternal, errors.New("agent returned the wrong operation frame"))
		}
	}
}

func (s *service) waitRunningAgain(ctx context.Context, allocation model.AllocationDesired, after time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, restartReadyTimeout)
	defer cancel()
	ticker := time.NewTicker(restartPollInterval)
	defer ticker.Stop()
	for {
		observations, err := s.state.ApplicationAllocationObservations(ctx, allocation.Application)
		if err != nil && ctx.Err() == nil {
			return connect.NewError(connect.CodeInternal, errors.New("read allocation observations"))
		}
		for _, observed := range observations {
			if observed.ID == allocation.ID && observed.UpdatedAt.After(after) && observed.Phase == model.AllocationRunning &&
				observed.HealthStatus != model.HealthChecking && observed.HealthStatus != model.HealthUnhealthy {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return connect.NewError(connect.CodeDeadlineExceeded,
				fmt.Errorf("%s replica %d was restarted but is not running again yet; check orch app status", allocation.Service, allocation.Replica))
		case <-ticker.C:
		}
	}
}
