package control

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	"change.codes/torsten/better-swarm/internal/config"
	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/store"
	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func validateDeploymentRequest(ctx context.Context, msg proto.Message, name string) error {
	if err := requireAdministrator(ctx); err != nil {
		return err
	}
	if err := rejectUnknownFields(msg); err != nil {
		return err
	}
	if err := config.ValidateApplicationName(name); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	return nil
}

func validateRevisionID(id string) error {
	if strings.TrimSpace(id) == "" || len(id) > 128 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("revision_id must contain 1-128 bytes"))
	}
	return nil
}

func deploymentError(err error) error {
	switch {
	case errors.Is(err, store.ErrApplicationNotFound), errors.Is(err, store.ErrRevisionNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, store.ErrGenerationConflict):
		return connect.NewError(connect.CodeAborted, err)
	case errors.Is(err, store.ErrRollbackSource):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, store.ErrIdempotencyConflict):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, store.ErrAllocationLimit), errors.Is(err, store.ErrApplicationLimit):
		return connect.NewError(connect.CodeResourceExhausted, err)
	default:
		return connect.NewError(connect.CodeInternal, errors.New("access deployment state"))
	}
}

func (s *service) ListDeployments(ctx context.Context, req *connect.Request[orchv1alpha1.ListDeploymentsRequest]) (*connect.Response[orchv1alpha1.ListDeploymentsResponse], error) {
	if err := validateDeploymentRequest(ctx, req.Msg, req.Msg.GetApplication()); err != nil {
		return nil, err
	}
	limit := req.Msg.GetLimit()
	if limit == 0 {
		limit = 20
	}
	if limit > 50 || req.Msg.GetBeforeGeneration() > math.MaxInt64 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("history limit must be 1-50 and generation must fit int64"))
	}
	records, next, err := s.state.Deployments(ctx, req.Msg.GetApplication(), req.Msg.GetBeforeGeneration(), int(limit))
	if err != nil {
		return nil, deploymentError(err)
	}
	response := &orchv1alpha1.ListDeploymentsResponse{NextBeforeGeneration: next}
	for _, record := range records {
		response.Deployments = append(response.Deployments, deploymentMessage(record))
		if proto.Size(response) > 1<<20 {
			response.Deployments = response.Deployments[:len(response.Deployments)-1]
			if len(response.Deployments) == 0 {
				return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("deployment summary exceeds response limit"))
			}
			response.NextBeforeGeneration = response.Deployments[len(response.Deployments)-1].GetGeneration()
			break
		}
	}
	return connect.NewResponse(response), nil
}

func (s *service) GetDeployment(ctx context.Context, req *connect.Request[orchv1alpha1.GetDeploymentRequest]) (*connect.Response[orchv1alpha1.GetDeploymentResponse], error) {
	if err := validateDeploymentRequest(ctx, req.Msg, req.Msg.GetApplication()); err != nil {
		return nil, err
	}
	if err := validateRevisionID(req.Msg.GetRevisionId()); err != nil {
		return nil, err
	}
	record, err := s.state.Deployment(ctx, req.Msg.GetApplication(), req.Msg.GetRevisionId())
	if err != nil {
		return nil, deploymentError(err)
	}
	return connect.NewResponse(&orchv1alpha1.GetDeploymentResponse{Deployment: deploymentMessage(record)}), nil
}

func (s *service) CompareDeployments(ctx context.Context, req *connect.Request[orchv1alpha1.CompareDeploymentsRequest]) (*connect.Response[orchv1alpha1.CompareDeploymentsResponse], error) {
	if err := validateDeploymentRequest(ctx, req.Msg, req.Msg.GetApplication()); err != nil {
		return nil, err
	}
	if err := validateRevisionID(req.Msg.GetFromRevisionId()); err != nil {
		return nil, err
	}
	toID := req.Msg.GetToRevisionId()
	if toID == "" {
		current, err := s.state.Application(ctx, req.Msg.GetApplication())
		if err != nil {
			return nil, deploymentError(err)
		}
		toID = current.RevisionID
	}
	if err := validateRevisionID(toID); err != nil {
		return nil, err
	}
	from, err := s.state.Deployment(ctx, req.Msg.GetApplication(), req.Msg.GetFromRevisionId())
	if err != nil {
		return nil, deploymentError(err)
	}
	to, err := s.state.Deployment(ctx, req.Msg.GetApplication(), toID)
	if err != nil {
		return nil, deploymentError(err)
	}
	// Decode stored resolved values without reevaluating Pkl or consulting secret stores.
	var before, after config.Manifest
	if json.Unmarshal(from.CanonicalManifest, &before) != nil || json.Unmarshal(to.CanonicalManifest, &after) != nil {
		return nil, deploymentError(errors.New("invalid stored manifest"))
	}
	changes, err := config.Diff(&before, &after)
	if err != nil {
		return nil, deploymentError(err)
	}
	response := &orchv1alpha1.CompareDeploymentsResponse{From: deploymentMessage(from), To: deploymentMessage(to)}
	if proto.Size(response) > 1<<20 {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("comparison summaries exceed response limit"))
	}
	if len(changes) > 500 {
		changes = changes[:500]
		response.Truncated = true
	}
	for _, change := range changes {
		item := &orchv1alpha1.DeploymentChange{Kind: string(change.Kind), Path: change.Path}
		// Only scalar, operational fields are displayed. Added/removed objects and
		// arbitrary command, argument, environment and secret fields stay private.
		parts := strings.Split(change.Path, ".")
		safe := len(parts) == 2 && parts[0] == "deploy"
		if len(parts) >= 3 && (parts[0] == "services" || parts[0] == "cron_jobs") {
			safe = len(parts) == 3 && (parts[2] == "image" || parts[2] == "replicas" || parts[2] == "schedule") ||
				len(parts) == 4 && parts[2] == "resources"
		}
		if safe {
			item.Before = deploymentValue(change.Before)
			item.After = deploymentValue(change.After)
		} else {
			item.Redacted = true
			if change.Kind != config.ChangeAdd {
				item.Before = "[redacted]"
			}
			if change.Kind != config.ChangeRemove {
				item.After = "[redacted]"
			}
		}
		response.Changes = append(response.Changes, item)
		if proto.Size(response) > 1<<20 {
			response.Changes = response.Changes[:len(response.Changes)-1]
			response.Truncated = true
			break
		}
	}
	return connect.NewResponse(response), nil
}

func deploymentValue(value any) string {
	if value == nil {
		return ""
	}
	data, _ := json.Marshal(value)
	if len(data) > 512 {
		return "[value exceeds display limit]"
	}
	return string(data)
}

func (s *service) RollbackApplication(ctx context.Context, req *connect.Request[orchv1alpha1.RollbackApplicationRequest]) (*connect.Response[orchv1alpha1.RollbackApplicationResponse], error) {
	if err := validateDeploymentRequest(ctx, req.Msg, req.Msg.GetApplication()); err != nil {
		return nil, err
	}
	if err := validateRevisionID(req.Msg.GetRevisionId()); err != nil {
		return nil, err
	}
	if req.Msg.GetExpectedGeneration() == 0 || req.Msg.GetExpectedGeneration() > math.MaxInt64 || !idempotencyKeyPattern.MatchString(req.Msg.GetIdempotencyKey()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("expected_generation and a 16-128 character URL-safe idempotency_key are required"))
	}
	source, err := s.state.Deployment(ctx, req.Msg.GetApplication(), req.Msg.GetRevisionId())
	if err != nil {
		return nil, deploymentError(err)
	}
	// Revalidate against this binary's supported configuration before accepting
	// older intent. References are restored; secret contents and volumes are not.
	manifest, err := config.ParseCanonicalJSON(source.CanonicalManifest)
	if err != nil || manifest.Name != req.Msg.GetApplication() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("stored revision is not supported by this controller"))
	}
	result, err := s.state.ApplyApplication(ctx, model.ApplyApplication{
		Name: source.Application, RevisionID: newRevisionID(s.cluster.ID, req.Msg.GetIdempotencyKey(), source.Digest),
		Digest: source.Digest, CanonicalManifest: source.CanonicalManifest, RequestedManifest: source.RequestedManifest,
		IdempotencyKey: req.Msg.GetIdempotencyKey(), CreatedAt: s.now().UTC(),
		ExpectedGeneration: req.Msg.GetExpectedGeneration(), RollbackFromRevisionID: source.RevisionID,
	})
	if err != nil {
		return nil, deploymentError(err)
	}
	if s.reconciler != nil {
		if err := s.reconciler.Reconcile(ctx); err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("publish rollback assignment; retry with the same idempotency key"))
		}
	}
	return connect.NewResponse(&orchv1alpha1.RollbackApplicationResponse{Deployment: applyApplicationMessage(result)}), nil
}

func deploymentMessage(d store.Deployment) *orchv1alpha1.Deployment {
	result := &orchv1alpha1.Deployment{
		Application: d.Application, RevisionId: d.RevisionID, Generation: d.Generation, Digest: d.Digest,
		CreatedAt: timestamppb.New(d.CreatedAt), RollbackFromRevisionId: d.RollbackFromRevisionID,
		Outcome: d.Outcome, ExecutionStatus: executionStatusMessage(d.Status), Reason: d.Reason, Message: d.Message,
		UpdatedAt: timestamppb.New(d.UpdatedAt), Current: d.Current, Active: d.Active,
	}
	if d.FinishedAt != nil {
		result.FinishedAt = timestamppb.New(*d.FinishedAt)
	}
	var manifest struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
		CronJobs map[string]struct {
			Image string `json:"image"`
		} `json:"cron_jobs"`
	}
	var requested config.Manifest
	_ = json.Unmarshal(d.RequestedManifest, &requested)
	if json.Unmarshal(d.CanonicalManifest, &manifest) == nil {
		for name, service := range manifest.Services {
			result.Images = append(result.Images, &orchv1alpha1.DeploymentImage{Kind: "service", Name: name, Image: service.Image, RequestedImage: requested.Services[name].Image})
		}
		for name, job := range manifest.CronJobs {
			result.Images = append(result.Images, &orchv1alpha1.DeploymentImage{Kind: "cron", Name: name, Image: job.Image, RequestedImage: requested.CronJobs[name].Image})
		}
		sort.Slice(result.Images, func(i, j int) bool {
			if result.Images[i].Kind == result.Images[j].Kind {
				return result.Images[i].Name < result.Images[j].Name
			}
			return result.Images[i].Kind < result.Images[j].Kind
		})
	}
	return result
}
