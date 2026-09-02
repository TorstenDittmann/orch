package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	"change.codes/torsten/better-swarm/gen/orch/v1alpha1/orchv1alpha1connect"
	"change.codes/torsten/better-swarm/internal/config"
	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/store"
	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxManifestBytes  = 1 << 20
	defaultEventLimit = 50
	maxEventLimit     = 200
)

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$`)

type service struct {
	orchv1alpha1connect.UnimplementedControlServiceHandler
	state   *store.Store
	cluster model.Cluster
	now     func() time.Time
}

func newService(state *store.Store, cluster model.Cluster, now func() time.Time) *service {
	if now == nil {
		now = time.Now
	}
	return &service{state: state, cluster: cluster, now: now}
}

func (s *service) GetCluster(ctx context.Context, _ *connect.Request[orchv1alpha1.GetClusterRequest]) (*connect.Response[orchv1alpha1.GetClusterResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	return connect.NewResponse(&orchv1alpha1.GetClusterResponse{Cluster: clusterMessage(s.cluster)}), nil
}

func (s *service) GetApplication(ctx context.Context, request *connect.Request[orchv1alpha1.GetApplicationRequest]) (*connect.Response[orchv1alpha1.GetApplicationResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	name := request.Msg.GetName()
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
	return connect.NewResponse(&orchv1alpha1.GetApplicationResponse{Application: applicationMessage(application)}), nil
}

func (s *service) ListEvents(ctx context.Context, request *connect.Request[orchv1alpha1.ListEventsRequest]) (*connect.Response[orchv1alpha1.ListEventsResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	after := request.Msg.GetAfterSequence()
	if after > math.MaxInt64 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("after_sequence is too large"))
	}
	limit := request.Msg.GetLimit()
	if limit == 0 {
		limit = defaultEventLimit
	}
	if limit > maxEventLimit {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("limit must not exceed %d", maxEventLimit))
	}
	events, err := s.state.EventsAfter(ctx, int64(after), int(limit))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("read cluster events"))
	}
	response := &orchv1alpha1.ListEventsResponse{
		Events: make([]*orchv1alpha1.Event, 0, len(events)), NextAfterSequence: after,
	}
	for _, event := range events {
		if event.Sequence < 1 {
			return nil, connect.NewError(connect.CodeInternal, errors.New("stored event has an invalid sequence"))
		}
		response.Events = append(response.Events, &orchv1alpha1.Event{
			Sequence: uint64(event.Sequence), Kind: event.Kind,
			PayloadJson: string(event.Payload), CreatedAt: timestamppb.New(event.CreatedAt),
		})
		response.NextAfterSequence = uint64(event.Sequence)
	}
	return connect.NewResponse(response), nil
}

func (s *service) ApplyApplication(ctx context.Context, request *connect.Request[orchv1alpha1.ApplyApplicationRequest]) (*connect.Response[orchv1alpha1.ApplyApplicationResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	manifestTOML := request.Msg.GetManifestToml()
	if len(manifestTOML) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("manifest_toml is required"))
	}
	if len(manifestTOML) > maxManifestBytes {
		return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("manifest_toml exceeds %d bytes", maxManifestBytes))
	}
	idempotencyKey := request.Msg.GetIdempotencyKey()
	if !idempotencyKeyPattern.MatchString(idempotencyKey) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("idempotency_key must be 16-128 URL-safe characters"))
	}
	manifest, err := config.Parse(manifestTOML)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	canonical, err := config.CanonicalJSON(manifest)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("canonicalize application manifest"))
	}
	if len(canonical) > maxManifestBytes {
		return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("canonical manifest exceeds %d bytes", maxManifestBytes))
	}
	digest, err := config.Digest(manifest)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("digest application manifest"))
	}
	revisionID := newRevisionID(s.cluster.ID, idempotencyKey, digest)
	result, err := s.state.ApplyApplication(ctx, model.ApplyApplication{
		Name: manifest.Name, RevisionID: revisionID, Digest: digest,
		CanonicalManifest: canonical, IdempotencyKey: idempotencyKey,
		CreatedAt: s.now().UTC(),
	})
	if errors.Is(err, store.ErrIdempotencyConflict) {
		return nil, connect.NewError(connect.CodeAlreadyExists, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("persist application desired state"))
	}
	return connect.NewResponse(applyApplicationMessage(result)), nil
}

func clusterMessage(cluster model.Cluster) *orchv1alpha1.Cluster {
	return &orchv1alpha1.Cluster{
		Id: cluster.ID, Name: cluster.Name, Endpoint: cluster.Endpoint,
		Generation: cluster.Generation, CreatedAt: timestamppb.New(cluster.CreatedAt),
	}
}

func applicationMessage(application model.Application) *orchv1alpha1.Application {
	return &orchv1alpha1.Application{
		Name: application.Name, Generation: application.Generation,
		RevisionId: application.RevisionID, Digest: application.Digest,
		CanonicalManifestJson: string(application.CanonicalManifest),
		ExecutionStatus:       orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_PENDING,
		CreatedAt:             timestamppb.New(application.CreatedAt), UpdatedAt: timestamppb.New(application.UpdatedAt),
	}
}

func applyApplicationMessage(result model.ApplyApplicationResult) *orchv1alpha1.ApplyApplicationResponse {
	return &orchv1alpha1.ApplyApplicationResponse{
		Name: result.Name, Generation: result.Generation, RevisionId: result.RevisionID,
		Digest: result.Digest, Changed: result.Changed, EventSequence: result.EventSequence,
		ExecutionStatus: orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_PENDING,
		CreatedAt:       timestamppb.New(result.CreatedAt), UpdatedAt: timestamppb.New(result.UpdatedAt),
	}
}

func newRevisionID(clusterID, idempotencyKey, digest string) string {
	sum := sha256.Sum256([]byte(clusterID + "\x00" + idempotencyKey + "\x00" + digest))
	return "rev_" + hex.EncodeToString(sum[:])
}
