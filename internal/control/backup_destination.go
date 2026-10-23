package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/objectstore"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SetBackupDestination checks that the bucket is reachable and writable,
// claims its prefix for this cluster, and stores it. Backups start copying
// right away.
func (s *service) SetBackupDestination(ctx context.Context, request *connect.Request[orchv1alpha1.SetBackupDestinationRequest]) (*connect.Response[orchv1alpha1.SetBackupDestinationResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	bucket, prefix, err := objectstore.ParseURL(request.Msg.GetUrl())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	endpoint, err := objectstore.NormalizeEndpoint(request.Msg.GetEndpoint())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	region := strings.TrimSpace(request.Msg.GetRegion())
	if len(region) > 64 || strings.ContainsAny(region, "/ ") {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid region"))
	}
	accessKeyID, secretAccessKey := strings.TrimSpace(request.Msg.GetAccessKeyId()), strings.TrimSpace(request.Msg.GetSecretAccessKey())
	if accessKeyID == "" || secretAccessKey == "" || len(accessKeyID) > 256 || len(secretAccessKey) > 1024 ||
		strings.ContainsAny(accessKeyID, "/ \n") {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("an access key ID and secret access key are required"))
	}
	if region == "" {
		region = objectstore.RegionFromEndpoint(endpoint)
	}
	destination := objectstore.Destination{Bucket: bucket, Prefix: prefix, Endpoint: endpoint, Region: region,
		AccessKeyID: accessKeyID, SecretAccessKey: secretAccessKey}
	client := objectstore.New(destination, nil)
	if region == "" {
		if _, err := client.DetectRegion(ctx); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("reach %s: %w", destination.URL(), err))
		}
	}
	if err := s.claimBackupDestination(ctx, client); err != nil {
		return nil, err
	}
	destination = client.Destination()
	if err := s.state.SetBackupDestination(ctx, s.secretBox, destination, s.now()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("store backup destination"))
	}
	s.offsite.kick()
	return connect.NewResponse(&orchv1alpha1.SetBackupDestinationResponse{Destination: backupDestinationMessage(destination)}), nil
}

// claimBackupDestination writes a marker naming this cluster below the
// prefix, which also proves the credentials can write. A prefix another
// cluster claimed is refused, so two clusters never prune each other's
// backups.
func (s *service) claimBackupDestination(ctx context.Context, client *objectstore.Client) error {
	destination := client.Destination()
	key := destination.Key(offsiteMarkerKey)
	body, _, err := client.Get(ctx, key)
	switch {
	case err == nil:
		owner, readErr := io.ReadAll(io.LimitReader(body, 256))
		body.Close()
		if readErr != nil {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("read %s: %w", destination.URL(), readErr))
		}
		if id := strings.TrimSpace(string(owner)); id != s.cluster.ID {
			return connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("%s already holds the backups of another orch cluster (%s); use another prefix, such as %s/%s",
					destination.URL(), id, destination.URL(), s.cluster.Name))
		}
		return nil
	case errors.Is(err, objectstore.ErrNotFound):
		marker := s.cluster.ID + "\n"
		if err := client.Put(ctx, key, strings.NewReader(marker), int64(len(marker))); err != nil {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("write to %s: %w", destination.URL(), err))
		}
		return nil
	default:
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("read %s: %w", destination.URL(), err))
	}
}

func (s *service) GetBackupDestination(ctx context.Context, request *connect.Request[orchv1alpha1.GetBackupDestinationRequest]) (*connect.Response[orchv1alpha1.GetBackupDestinationResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	destination, found, err := s.state.BackupDestination(ctx, s.secretBox)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("read backup destination"))
	}
	response := &orchv1alpha1.GetBackupDestinationResponse{}
	if !found {
		return connect.NewResponse(response), nil
	}
	response.Destination = backupDestinationMessage(destination)
	if status := s.offsite.status(); status.URL == destination.URL() {
		response.UploadFailure = status.Failure
		if !status.SyncedAt.IsZero() {
			response.UploadedAt = timestamppb.New(status.SyncedAt)
		}
		if !status.FailedAt.IsZero() {
			response.UploadFailedAt = timestamppb.New(status.FailedAt)
		}
	}
	return connect.NewResponse(response), nil
}

// RemoveBackupDestination stops copying backups. What is in the bucket
// stays there.
func (s *service) RemoveBackupDestination(ctx context.Context, request *connect.Request[orchv1alpha1.RemoveBackupDestinationRequest]) (*connect.Response[orchv1alpha1.RemoveBackupDestinationResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	changed, err := s.state.DeleteBackupDestination(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("remove backup destination"))
	}
	s.offsite.kick()
	return connect.NewResponse(&orchv1alpha1.RemoveBackupDestinationResponse{Changed: changed}), nil
}

func backupDestinationMessage(destination objectstore.Destination) *orchv1alpha1.BackupDestination {
	return &orchv1alpha1.BackupDestination{Url: destination.URL(), Endpoint: destination.Endpoint, Region: destination.Region,
		AccessKeyId: destination.AccessKeyID}
}

// describeOffsite adds the bucket copies' state to the controller backup
// status.
func (s *service) describeOffsite(message *orchv1alpha1.ControllerBackup) {
	status := s.offsite.status()
	if status.URL == "" {
		return
	}
	message.Destination, message.UploadFailure = status.URL, status.Failure
	if !status.SyncedAt.IsZero() {
		message.UploadedAt = timestamppb.New(status.SyncedAt)
	}
	if !status.FailedAt.IsZero() {
		message.UploadFailedAt = timestamppb.New(status.FailedAt)
	}
}
