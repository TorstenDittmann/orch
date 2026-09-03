package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	"change.codes/torsten/better-swarm/gen/orch/v1alpha1/orchv1alpha1connect"
	"change.codes/torsten/better-swarm/internal/config"
	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/security"
	"change.codes/torsten/better-swarm/internal/store"
	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxManifestBytes  = 1 << 20
	maxCSRBytes       = 16 << 10
	defaultEventLimit = 50
	maxEventLimit     = 200
	defaultTailLines  = 100
	maxTailLines      = 10_000
	defaultExecTime   = 30 * time.Second
	maxExecTime       = 5 * time.Minute
)

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$`)

type service struct {
	orchv1alpha1connect.UnimplementedControlServiceHandler
	state      *store.Store
	cluster    model.Cluster
	now        func() time.Time
	random     io.Reader
	caPEM      []byte
	caKey      []byte
	secretBox  *security.SecretBox
	sessions   *sessionManager
	reconciler *Reconciler
}

func newService(state *store.Store, cluster model.Cluster, caPEM, caKey []byte, secretBox *security.SecretBox, sessions *sessionManager, now func() time.Time) *service {
	if now == nil {
		now = time.Now
	}
	return &service{
		state: state, cluster: cluster, now: now, random: rand.Reader,
		caPEM: append([]byte(nil), caPEM...), caKey: append([]byte(nil), caKey...), secretBox: secretBox,
		sessions: sessions,
	}
}

func (s *service) GetCluster(ctx context.Context, request *connect.Request[orchv1alpha1.GetClusterRequest]) (*connect.Response[orchv1alpha1.GetClusterResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	cluster, err := s.state.Cluster(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("read cluster state"))
	}
	return connect.NewResponse(&orchv1alpha1.GetClusterResponse{Cluster: clusterMessage(cluster)}), nil
}

func (s *service) ConfigureNetwork(ctx context.Context, request *connect.Request[orchv1alpha1.ConfigureNetworkRequest]) (*connect.Response[orchv1alpha1.ConfigureNetworkResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	if !idempotencyKeyPattern.MatchString(request.Msg.GetIdempotencyKey()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("idempotency key must be 16-128 URL-safe characters"))
	}
	prefix, err := model.ParseWorkloadCIDR(request.Msg.GetCidr())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	digest := sha256.Sum256([]byte("network.configure\x00" + prefix.String()))
	result, err := s.state.ConfigureNetwork(ctx, model.ConfigureNetwork{
		CIDR: prefix.String(), IdempotencyKey: request.Msg.GetIdempotencyKey(),
		RequestHash: hex.EncodeToString(digest[:]), UpdatedAt: s.now().UTC(),
	})
	if errors.Is(err, store.ErrNetworkFrozen) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if errors.Is(err, store.ErrIdempotencyConflict) {
		return nil, connect.NewError(connect.CodeAlreadyExists, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("configure workload network"))
	}
	if result.Changed && s.reconciler != nil {
		if err := s.reconciler.Reconcile(ctx); err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("publish configured workload network"))
		}
	}
	return connect.NewResponse(&orchv1alpha1.ConfigureNetworkResponse{
		Cidr: result.CIDR, Generation: result.Generation, Frozen: result.Frozen,
		Changed: result.Changed, EventSequence: result.EventSequence,
		UpdatedAt: timestamppb.New(result.UpdatedAt),
	}), nil
}

func (s *service) GetApplication(ctx context.Context, request *connect.Request[orchv1alpha1.GetApplicationRequest]) (*connect.Response[orchv1alpha1.GetApplicationResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
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
	if err := rejectUnknownFields(request.Msg); err != nil {
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
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	manifestTOML := request.Msg.GetManifestToml()
	canonical := request.Msg.GetCanonicalManifestJson()
	if len(manifestTOML) != 0 {
		if len(canonical) != 0 {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("manifest_toml and canonical_manifest_json cannot both be set"))
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("manifest_toml is no longer supported; evaluate Pkl locally and send canonical_manifest_json"))
	}
	if len(canonical) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("canonical_manifest_json is required"))
	}
	if len(canonical) > maxManifestBytes {
		return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("canonical_manifest_json exceeds %d bytes", maxManifestBytes))
	}
	idempotencyKey := request.Msg.GetIdempotencyKey()
	if !idempotencyKeyPattern.MatchString(idempotencyKey) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("idempotency_key must be 16-128 URL-safe characters"))
	}
	manifest, err := config.ParseCanonicalJSON(canonical)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	digest := config.DigestCanonicalJSON(canonical)
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
	if s.reconciler != nil {
		if err := s.reconciler.Reconcile(ctx); err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("publish application assignment"))
		}
	}
	if result.ExecutionStatus != model.ExecutionPending {
		return nil, connect.NewError(connect.CodeInternal, errors.New("stored application response has an invalid execution status"))
	}
	return connect.NewResponse(applyApplicationMessage(result)), nil
}

func (s *service) EnrollAgent(ctx context.Context, request *connect.Request[orchv1alpha1.EnrollAgentRequest]) (*connect.Response[orchv1alpha1.EnrollAgentResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	name := request.Msg.GetName()
	if err := config.ValidateApplicationName(name); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	csrDER := request.Msg.GetCertificateRequestDer()
	if len(csrDER) == 0 || len(csrDER) > maxCSRBytes {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("certificate_request_der must contain 1-%d bytes", maxCSRBytes))
	}
	idempotencyKey := request.Msg.GetIdempotencyKey()
	if !idempotencyKeyPattern.MatchString(idempotencyKey) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("idempotency_key must be 16-128 URL-safe characters"))
	}
	publicKeyHash, err := security.AgentCSRPublicKeyHash(csrDER)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	nodeID := newNodeID(s.cluster.ID, publicKeyHash)
	now := s.now().UTC()
	signed, err := security.SignAgentCertificate(s.caPEM, s.caKey, s.cluster.ID, nodeID, csrDER, now, s.random)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("sign agent identity"))
	}
	requestBytes := make([]byte, 0, len(name)+1+len(csrDER))
	requestBytes = append(requestBytes, name...)
	requestBytes = append(requestBytes, 0)
	requestBytes = append(requestBytes, csrDER...)
	requestDigest := sha256.Sum256(requestBytes)
	result, err := s.state.EnrollAgent(ctx, model.EnrollAgent{
		NodeID: nodeID, Name: name, PublicKeyHash: signed.PublicKeyHash,
		CertificateSerial: signed.Serial, CertificatePEM: signed.CertificatePEM,
		CertificateNotAfter: signed.NotAfter, ClusterID: s.cluster.ID,
		ControllerEndpoint: s.cluster.Endpoint, CACertificatePEM: s.caPEM,
		IdempotencyKey: idempotencyKey, RequestHash: hex.EncodeToString(requestDigest[:]), CreatedAt: now,
	})
	if errors.Is(err, store.ErrIdempotencyConflict) || errors.Is(err, store.ErrNodeAlreadyEnrolled) {
		return nil, connect.NewError(connect.CodeAlreadyExists, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("persist agent enrollment"))
	}
	if s.reconciler != nil {
		if err := s.reconciler.Reconcile(ctx); err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("publish initial agent assignment"))
		}
	}
	return connect.NewResponse(&orchv1alpha1.EnrollAgentResponse{
		NodeId: result.NodeID, NodeName: result.NodeName, ClusterId: result.ClusterID,
		ControllerEndpoint:  result.ControllerEndpoint,
		CaCertificatePem:    append([]byte(nil), result.CACertificatePEM...),
		AgentCertificatePem: append([]byte(nil), result.AgentCertificatePEM...),
		ExpiresAt:           timestamppb.New(result.ExpiresAt), Changed: result.Changed,
		EventSequence: result.EventSequence,
	}), nil
}

func (s *service) ListNodes(ctx context.Context, request *connect.Request[orchv1alpha1.ListNodesRequest]) (*connect.Response[orchv1alpha1.ListNodesResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	nodes, err := s.state.Nodes(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("read nodes"))
	}
	response := &orchv1alpha1.ListNodesResponse{Nodes: make([]*orchv1alpha1.Node, 0, len(nodes))}
	for _, node := range nodes {
		if s.sessions != nil && s.sessions.Connected(node.ID) {
			node.ConnectionStatus = model.NodeConnected
		} else if node.LastSeenAt != nil {
			node.ConnectionStatus = model.NodeDisconnected
		}
		response.Nodes = append(response.Nodes, nodeMessage(node))
	}
	return connect.NewResponse(response), nil
}

func (s *service) RemoveApplication(ctx context.Context, request *connect.Request[orchv1alpha1.RemoveApplicationRequest]) (*connect.Response[orchv1alpha1.RemoveApplicationResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	name := request.Msg.GetName()
	if err := config.ValidateApplicationName(name); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	key := request.Msg.GetIdempotencyKey()
	if !idempotencyKeyPattern.MatchString(key) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("idempotency_key must be 16-128 URL-safe characters"))
	}
	result, err := s.state.RemoveApplication(ctx, name, key, s.now().UTC())
	if errors.Is(err, store.ErrApplicationNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if errors.Is(err, store.ErrIdempotencyConflict) {
		return nil, connect.NewError(connect.CodeAlreadyExists, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("persist application removal"))
	}
	if s.reconciler != nil {
		if err := s.reconciler.Reconcile(ctx); err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("publish application removal"))
		}
	}
	return connect.NewResponse(&orchv1alpha1.RemoveApplicationResponse{
		Name: result.Name, Generation: result.Generation, Changed: result.Changed,
		EventSequence: result.EventSequence, ExecutionStatus: executionStatusMessage(result.ExecutionStatus),
		UpdatedAt: timestamppb.New(result.UpdatedAt),
	}), nil
}

func (s *service) TailLogs(ctx context.Context, request *connect.Request[orchv1alpha1.TailLogsRequest], stream *connect.ServerStream[orchv1alpha1.TailLogsResponse]) error {
	if err := requireAdministrator(ctx); err != nil {
		return err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return err
	}
	allocation, node, err := s.operationTarget(ctx, request.Msg.GetApplication(), request.Msg.GetService(), request.Msg.GetReplica())
	if err != nil {
		return err
	}
	tailLines := request.Msg.GetTailLines()
	if tailLines == 0 {
		tailLines = defaultTailLines
	}
	if tailLines > maxTailLines {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("tail_lines must not exceed %d", maxTailLines))
	}
	operationContext, cancelContext := context.WithTimeout(ctx, 30*time.Second)
	defer cancelContext()
	_, frames, cancelOperation, err := s.sessions.beginOperation(operationContext, node.ID, func(requestID string) *orchv1alpha1.ConnectResponse {
		return &orchv1alpha1.ConnectResponse{Message: &orchv1alpha1.ConnectResponse_TailLogs{TailLogs: &orchv1alpha1.TailLogsCommand{
			RequestId: requestID, AllocationId: allocation.ID, TailLines: tailLines,
		}}}
	})
	if err != nil {
		return operationConnectError(err)
	}
	defer cancelOperation()
	terminal := false
	for {
		select {
		case <-operationContext.Done():
			return connect.NewError(connect.CodeDeadlineExceeded, errors.New("log request timed out"))
		case result, ok := <-frames:
			if !ok {
				if terminal {
					return nil
				}
				return connect.NewError(connect.CodeUnavailable, errAgentDisconnected)
			}
			switch value := result.GetResult().(type) {
			case *orchv1alpha1.AgentOperationResult_Log:
				if value.Log == nil {
					return connect.NewError(connect.CodeInternal, errors.New("agent returned an empty log frame"))
				}
				terminal = value.Log.GetComplete()
				if err := stream.Send(value.Log); err != nil {
					return err
				}
			case *orchv1alpha1.AgentOperationResult_Error:
				return agentOperationError(value.Error)
			default:
				return connect.NewError(connect.CodeInternal, errors.New("agent returned the wrong operation frame"))
			}
		}
	}
}

func (s *service) ListCronRuns(ctx context.Context, request *connect.Request[orchv1alpha1.ListCronRunsRequest]) (*connect.Response[orchv1alpha1.ListCronRunsResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	if err := config.ValidateApplicationName(request.Msg.GetApplication()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if request.Msg.GetJob() != "" {
		if err := config.ValidateApplicationName(request.Msg.GetJob()); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	limit := request.Msg.GetLimit()
	if limit == 0 {
		limit = 20
	}
	if limit > 100 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("limit must not exceed 100"))
	}
	runs, err := s.state.CronRuns(ctx, request.Msg.GetApplication(), request.Msg.GetJob(), limit)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("read cron runs"))
	}
	response := &orchv1alpha1.ListCronRunsResponse{Runs: make([]*orchv1alpha1.JobRunStatus, 0, len(runs))}
	for _, run := range runs {
		response.Runs = append(response.Runs, jobRunStatusMessage(run))
	}
	return connect.NewResponse(response), nil
}

func (s *service) SetSecret(ctx context.Context, request *connect.Request[orchv1alpha1.SetSecretRequest]) (*connect.Response[orchv1alpha1.SetSecretResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	application, name := request.Msg.GetApplication(), request.Msg.GetName()
	if err := config.ValidateApplicationName(application); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := config.ValidateApplicationName(name); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("secret name: %w", err))
	}
	value := request.Msg.GetValue()
	if len(value) == 0 || len(value) > maxManifestBytes {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("secret value must contain 1-%d bytes", maxManifestBytes))
	}
	defer wipeBytes(value)
	if !idempotencyKeyPattern.MatchString(request.Msg.GetIdempotencyKey()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("idempotency_key must be 16-128 URL-safe characters"))
	}
	versionID, err := newOpaqueID("secv_", s.random)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("generate secret version"))
	}
	result, err := s.state.SetApplicationSecret(ctx, s.secretBox, application, name, versionID,
		value, request.Msg.GetIdempotencyKey(), s.now().UTC())
	if errors.Is(err, store.ErrIdempotencyConflict) {
		return nil, connect.NewError(connect.CodeAlreadyExists, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("store secret version"))
	}
	if s.reconciler != nil {
		if err := s.reconciler.Reconcile(ctx); err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("publish secret rotation"))
		}
	}
	return connect.NewResponse(&orchv1alpha1.SetSecretResponse{
		Application: result.Application, Name: result.Name, VersionId: result.VersionID,
		Changed: result.Changed, EventSequence: result.EventSequence, UpdatedAt: timestamppb.New(result.UpdatedAt),
	}), nil
}

func (s *service) ListSecrets(ctx context.Context, request *connect.Request[orchv1alpha1.ListSecretsRequest]) (*connect.Response[orchv1alpha1.ListSecretsResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	application := request.Msg.GetApplication()
	if err := config.ValidateApplicationName(application); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	items, err := s.state.ApplicationSecrets(ctx, application)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("list secret metadata"))
	}
	response := &orchv1alpha1.ListSecretsResponse{Secrets: make([]*orchv1alpha1.SecretMetadata, 0, len(items))}
	for _, item := range items {
		response.Secrets = append(response.Secrets, &orchv1alpha1.SecretMetadata{
			Application: item.Application, Name: item.Name, CurrentVersionId: item.CurrentVersionID,
			Referenced: item.Referenced, CreatedAt: timestamppb.New(item.CreatedAt), UpdatedAt: timestamppb.New(item.UpdatedAt),
		})
	}
	return connect.NewResponse(response), nil
}

func (s *service) DeleteSecret(ctx context.Context, request *connect.Request[orchv1alpha1.DeleteSecretRequest]) (*connect.Response[orchv1alpha1.DeleteSecretResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	application, name := request.Msg.GetApplication(), request.Msg.GetName()
	if err := config.ValidateApplicationName(application); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := config.ValidateApplicationName(name); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("secret name: %w", err))
	}
	if !idempotencyKeyPattern.MatchString(request.Msg.GetIdempotencyKey()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("idempotency_key must be 16-128 URL-safe characters"))
	}
	result, err := s.state.DeleteApplicationSecret(ctx, s.secretBox, application, name, request.Msg.GetIdempotencyKey(), s.now().UTC())
	switch {
	case errors.Is(err, store.ErrSecretNotFound):
		return nil, connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, store.ErrSecretInUse):
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, store.ErrIdempotencyConflict):
		return nil, connect.NewError(connect.CodeAlreadyExists, err)
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, errors.New("delete secret"))
	}
	return connect.NewResponse(&orchv1alpha1.DeleteSecretResponse{
		Application: result.Application, Name: result.Name, Changed: result.Changed,
		EventSequence: result.EventSequence, UpdatedAt: timestamppb.New(result.UpdatedAt),
	}), nil
}

func (s *service) LoginRegistry(ctx context.Context, request *connect.Request[orchv1alpha1.LoginRegistryRequest]) (*connect.Response[orchv1alpha1.LoginRegistryResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	host, err := store.NormalizeRegistryHost(request.Msg.GetHost())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	username := request.Msg.GetUsername()
	if strings.TrimSpace(username) == "" || len(username) > 1024 || strings.ContainsAny(username, "\x00\r\n") {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("username must be nonempty, bounded, and contain no control characters"))
	}
	secret := request.Msg.GetSecret()
	if len(secret) == 0 || len(secret) > maxManifestBytes {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("registry secret must contain 1-%d bytes", maxManifestBytes))
	}
	defer wipeBytes(secret)
	if !idempotencyKeyPattern.MatchString(request.Msg.GetIdempotencyKey()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("idempotency_key must be 16-128 URL-safe characters"))
	}
	versionID, err := newOpaqueID("regv_", s.random)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("generate registry credential version"))
	}
	result, err := s.state.SetRegistryCredential(ctx, s.secretBox, host, username, versionID,
		secret, request.Msg.GetIdempotencyKey(), s.now().UTC())
	if errors.Is(err, store.ErrIdempotencyConflict) {
		return nil, connect.NewError(connect.CodeAlreadyExists, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("store registry credential"))
	}
	if s.reconciler != nil {
		if err := s.reconciler.Reconcile(ctx); err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("publish registry credential"))
		}
	}
	return connect.NewResponse(&orchv1alpha1.LoginRegistryResponse{
		Host: result.Host, Username: result.Username, VersionId: result.VersionID,
		Changed: result.Changed, EventSequence: result.EventSequence, UpdatedAt: timestamppb.New(result.UpdatedAt),
	}), nil
}

func (s *service) ListRegistries(ctx context.Context, request *connect.Request[orchv1alpha1.ListRegistriesRequest]) (*connect.Response[orchv1alpha1.ListRegistriesResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	items, err := s.state.RegistryCredentials(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("list registry credentials"))
	}
	response := &orchv1alpha1.ListRegistriesResponse{Registries: make([]*orchv1alpha1.RegistryCredentialMetadata, 0, len(items))}
	for _, item := range items {
		response.Registries = append(response.Registries, &orchv1alpha1.RegistryCredentialMetadata{
			Host: item.Host, Username: item.Username, CurrentVersionId: item.CurrentVersionID,
			CreatedAt: timestamppb.New(item.CreatedAt), UpdatedAt: timestamppb.New(item.UpdatedAt),
		})
	}
	return connect.NewResponse(response), nil
}

func (s *service) LogoutRegistry(ctx context.Context, request *connect.Request[orchv1alpha1.LogoutRegistryRequest]) (*connect.Response[orchv1alpha1.LogoutRegistryResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	host, err := store.NormalizeRegistryHost(request.Msg.GetHost())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if !idempotencyKeyPattern.MatchString(request.Msg.GetIdempotencyKey()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("idempotency_key must be 16-128 URL-safe characters"))
	}
	result, err := s.state.DeleteRegistryCredential(ctx, s.secretBox, host, request.Msg.GetIdempotencyKey(), s.now().UTC())
	switch {
	case errors.Is(err, store.ErrRegistryCredentialNotFound):
		return nil, connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, store.ErrIdempotencyConflict):
		return nil, connect.NewError(connect.CodeAlreadyExists, err)
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, errors.New("delete registry credential"))
	}
	if s.reconciler != nil {
		if err := s.reconciler.Reconcile(ctx); err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("publish registry credential removal"))
		}
	}
	return connect.NewResponse(&orchv1alpha1.LogoutRegistryResponse{
		Host: result.Host, Changed: result.Changed, EventSequence: result.EventSequence, UpdatedAt: timestamppb.New(result.UpdatedAt),
	}), nil
}

func (s *service) TailCronLogs(ctx context.Context, request *connect.Request[orchv1alpha1.TailCronLogsRequest], stream *connect.ServerStream[orchv1alpha1.TailCronLogsResponse]) error {
	if err := requireAdministrator(ctx); err != nil {
		return err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return err
	}
	if err := config.ValidateApplicationName(request.Msg.GetApplication()); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := config.ValidateApplicationName(request.Msg.GetJob()); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	if request.Msg.GetRunId() == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("run_id is required"))
	}
	runs, err := s.state.CronRuns(ctx, request.Msg.GetApplication(), request.Msg.GetJob(), 100)
	if err != nil {
		return connect.NewError(connect.CodeInternal, errors.New("read cron runs"))
	}
	found := false
	for _, run := range runs {
		if run.ID == request.Msg.GetRunId() {
			found = true
			break
		}
	}
	if !found {
		return connect.NewError(connect.CodeNotFound, errors.New("cron run was not found"))
	}
	nodes, err := s.state.Nodes(ctx)
	if err != nil || len(nodes) == 0 {
		return connect.NewError(connect.CodeUnavailable, errors.New("agent is unavailable"))
	}
	tailLines := request.Msg.GetTailLines()
	if tailLines == 0 {
		tailLines = defaultTailLines
	}
	if tailLines > maxTailLines {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("tail_lines must not exceed %d", maxTailLines))
	}
	operationContext, cancelContext := context.WithTimeout(ctx, 30*time.Second)
	defer cancelContext()
	_, frames, cancelOperation, err := s.sessions.beginOperation(operationContext, nodes[0].ID, func(requestID string) *orchv1alpha1.ConnectResponse {
		return &orchv1alpha1.ConnectResponse{Message: &orchv1alpha1.ConnectResponse_TailCronLogs{TailCronLogs: &orchv1alpha1.TailCronLogsCommand{RequestId: requestID, RunId: request.Msg.GetRunId(), TailLines: tailLines}}}
	})
	if err != nil {
		return operationConnectError(err)
	}
	defer cancelOperation()
	terminal := false
	for {
		select {
		case <-operationContext.Done():
			return connect.NewError(connect.CodeDeadlineExceeded, errors.New("cron log request timed out"))
		case result, ok := <-frames:
			if !ok {
				if terminal {
					return nil
				}
				return connect.NewError(connect.CodeUnavailable, errAgentDisconnected)
			}
			switch value := result.GetResult().(type) {
			case *orchv1alpha1.AgentOperationResult_Log:
				if value.Log == nil {
					return connect.NewError(connect.CodeInternal, errors.New("agent returned an empty log frame"))
				}
				terminal = value.Log.GetComplete()
				if err := stream.Send(&orchv1alpha1.TailCronLogsResponse{Stream: value.Log.GetStream(), Data: value.Log.GetData(),
					Offset: value.Log.GetOffset(), Truncated: value.Log.GetTruncated(), Complete: value.Log.GetComplete()}); err != nil {
					return err
				}
			case *orchv1alpha1.AgentOperationResult_Error:
				return agentOperationError(value.Error)
			default:
				return connect.NewError(connect.CodeInternal, errors.New("agent returned the wrong operation frame"))
			}
		}
	}
}

func (s *service) Exec(ctx context.Context, request *connect.Request[orchv1alpha1.ExecRequest], stream *connect.ServerStream[orchv1alpha1.ExecResponse]) error {
	if err := requireAdministrator(ctx); err != nil {
		return err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return err
	}
	allocation, node, err := s.operationTarget(ctx, request.Msg.GetApplication(), request.Msg.GetService(), request.Msg.GetReplica())
	if err != nil {
		return err
	}
	command := request.Msg.GetCommand()
	if len(command) == 0 || len(command) > 64 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("command must contain 1-64 arguments"))
	}
	totalBytes := 0
	for _, argument := range command {
		if argument == "" || strings.ContainsRune(argument, 0) {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("command arguments must be nonempty and contain no NUL bytes"))
		}
		totalBytes += len(argument)
	}
	if totalBytes > 8<<10 {
		return connect.NewError(connect.CodeResourceExhausted, errors.New("command exceeds 8 KiB"))
	}
	timeout := time.Duration(request.Msg.GetTimeoutSeconds()) * time.Second
	if timeout == 0 {
		timeout = defaultExecTime
	}
	if timeout > maxExecTime {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("timeout_seconds must not exceed %d", int(maxExecTime/time.Second)))
	}
	operationContext, cancelContext := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancelContext()
	_, frames, cancelOperation, err := s.sessions.beginOperation(operationContext, node.ID, func(requestID string) *orchv1alpha1.ConnectResponse {
		return &orchv1alpha1.ConnectResponse{Message: &orchv1alpha1.ConnectResponse_Exec{Exec: &orchv1alpha1.ExecCommand{
			RequestId: requestID, AllocationId: allocation.ID,
			Command: append([]string(nil), command...), TimeoutSeconds: uint32(timeout / time.Second),
		}}}
	})
	if err != nil {
		return operationConnectError(err)
	}
	defer cancelOperation()
	terminal := false
	for {
		select {
		case <-operationContext.Done():
			return connect.NewError(connect.CodeDeadlineExceeded, errors.New("exec request timed out"))
		case result, ok := <-frames:
			if !ok {
				if terminal {
					return nil
				}
				return connect.NewError(connect.CodeUnavailable, errAgentDisconnected)
			}
			switch value := result.GetResult().(type) {
			case *orchv1alpha1.AgentOperationResult_Exec:
				if value.Exec == nil {
					return connect.NewError(connect.CodeInternal, errors.New("agent returned an empty exec frame"))
				}
				terminal = value.Exec.GetExited()
				if err := stream.Send(value.Exec); err != nil {
					return err
				}
			case *orchv1alpha1.AgentOperationResult_Error:
				return agentOperationError(value.Error)
			default:
				return connect.NewError(connect.CodeInternal, errors.New("agent returned the wrong operation frame"))
			}
		}
	}
}

func (s *service) operationTarget(ctx context.Context, application, serviceName string, replica uint32) (model.AllocationDesired, model.Node, error) {
	if err := config.ValidateApplicationName(application); err != nil {
		return model.AllocationDesired{}, model.Node{}, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := config.ValidateApplicationName(serviceName); err != nil {
		return model.AllocationDesired{}, model.Node{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid service name: %w", err))
	}
	if replica == 0 {
		return model.AllocationDesired{}, model.Node{}, connect.NewError(connect.CodeInvalidArgument, errors.New("replica must be positive"))
	}
	allocation, err := s.state.DesiredAllocation(ctx, application, serviceName, replica)
	if errors.Is(err, store.ErrAssignmentNotFound) {
		return model.AllocationDesired{}, model.Node{}, connect.NewError(connect.CodeNotFound, errors.New("allocation was not found in current desired state"))
	}
	if err != nil {
		return model.AllocationDesired{}, model.Node{}, connect.NewError(connect.CodeInternal, errors.New("read desired allocation"))
	}
	node, err := s.state.Node(ctx)
	if err != nil {
		return model.AllocationDesired{}, model.Node{}, connect.NewError(connect.CodeFailedPrecondition, errors.New("no agent is enrolled"))
	}
	if s.sessions == nil || !s.sessions.Connected(node.ID) {
		return model.AllocationDesired{}, model.Node{}, connect.NewError(connect.CodeUnavailable, errAgentDisconnected)
	}
	return allocation, node, nil
}

func operationConnectError(err error) error {
	if errors.Is(err, errAgentDisconnected) {
		return connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewError(connect.CodeInternal, errors.New("start agent operation"))
}

func agentOperationError(operation *orchv1alpha1.OperationError) error {
	if operation == nil {
		return connect.NewError(connect.CodeInternal, errors.New("agent returned an empty operation error"))
	}
	code := connect.CodeFailedPrecondition
	switch operation.GetCode() {
	case "not_found":
		code = connect.CodeNotFound
	case "timeout":
		code = connect.CodeDeadlineExceeded
	case "unavailable":
		code = connect.CodeUnavailable
	}
	return connect.NewError(code, errors.New(operation.GetMessage()))
}

func clusterMessage(cluster model.Cluster) *orchv1alpha1.Cluster {
	return &orchv1alpha1.Cluster{
		Id: cluster.ID, Name: cluster.Name, Endpoint: cluster.Endpoint,
		Generation: cluster.Generation, CreatedAt: timestamppb.New(cluster.CreatedAt),
		WorkloadCidr: cluster.WorkloadCIDR, NetworkGeneration: cluster.NetworkGeneration,
		NetworkFrozen: cluster.NetworkFrozenAt != nil,
	}
}

func applicationMessage(application model.Application) *orchv1alpha1.Application {
	message := &orchv1alpha1.Application{
		Name: application.Name, Generation: application.Generation,
		RevisionId: application.RevisionID, Digest: application.Digest,
		CanonicalManifestJson: string(application.CanonicalManifest),
		ExecutionStatus:       executionStatusMessage(application.Execution.Status),
		CreatedAt:             timestamppb.New(application.CreatedAt), UpdatedAt: timestamppb.New(application.UpdatedAt),
		ObservedGeneration: application.Execution.ObservedGeneration,
		ExecutionReason:    application.Execution.Reason,
		ExecutionMessage:   application.Execution.Message,
		ExecutionUpdatedAt: timestamppb.New(application.Execution.UpdatedAt),
		Deleted:            application.DeletedAt != nil,
		ActiveGeneration:   application.Execution.ActiveGeneration,
		ActiveRevisionId:   application.Execution.ActiveRevisionID,
	}
	for _, allocation := range application.Execution.Allocations {
		message.Allocations = append(message.Allocations, allocationStatusMessage(allocation))
	}
	for _, route := range application.Execution.Routes {
		message.Routes = append(message.Routes, routeStatusMessage(route))
	}
	for _, route := range application.Execution.TCPRoutes {
		message.TcpRoutes = append(message.TcpRoutes, tcpRouteStatusMessage(route))
	}
	for _, job := range application.Execution.CronJobs {
		message.CronJobs = append(message.CronJobs, cronJobStatusMessage(job))
	}
	return message
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

func newOpaqueID(prefix string, source io.Reader) (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(source, value); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value), nil
}

func newNodeID(clusterID, publicKeyHash string) string {
	sum := sha256.Sum256([]byte(clusterID + "\x00" + publicKeyHash))
	return "node_" + hex.EncodeToString(sum[:16])
}

func executionStatusMessage(status string) orchv1alpha1.ExecutionStatus {
	switch status {
	case model.ExecutionPending:
		return orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_PENDING
	case model.ExecutionBlocked:
		return orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_BLOCKED
	case model.ExecutionStarting:
		return orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_STARTING
	case model.ExecutionRunning:
		return orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_RUNNING
	case model.ExecutionDegraded:
		return orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_DEGRADED
	case model.ExecutionFailed:
		return orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_FAILED
	case model.ExecutionStopping:
		return orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_STOPPING
	case model.ExecutionRemoved:
		return orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_REMOVED
	case model.ExecutionHealthy:
		return orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_HEALTHY
	case model.ExecutionRolledBack:
		return orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_ROLLED_BACK
	default:
		return orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_UNSPECIFIED
	}
}

func nodeMessage(node model.Node) *orchv1alpha1.Node {
	message := &orchv1alpha1.Node{
		Id: node.ID, Name: node.Name, CreatedAt: timestamppb.New(node.CreatedAt),
		OperatingSystem: node.OperatingSystem, Architecture: node.Architecture,
		RuntimeVersion: node.RuntimeVersion, Snapshotter: node.Snapshotter, CgroupsV2: node.CgroupsV2,
		AssignmentGeneration: node.AssignmentGeneration, AcknowledgedGeneration: node.AcknowledgedGeneration,
		NodeSubnet: node.NodeSubnet, NetworkState: node.NetworkState, NetworkReason: node.NetworkReason,
		CniVersion: node.CNIVersion, Nftables: node.Nftables, DnsState: node.DNSState,
		CaddyVersion: node.CaddyVersion, IngressState: node.IngressState,
		DrainingObjects:   node.DrainingObjects,
		CniPluginsVersion: node.CNIPluginsVersion, DnsAvailable: node.DNSAvailable,
	}
	if node.LastSeenAt != nil {
		message.LastSeenAt = timestamppb.New(*node.LastSeenAt)
	}
	switch node.ConnectionStatus {
	case model.NodeConnected:
		message.ConnectionStatus = orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_CONNECTED
	case model.NodeDisconnected:
		message.ConnectionStatus = orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_DISCONNECTED
	default:
		message.ConnectionStatus = orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_ENROLLED
	}
	return message
}

func allocationStatusMessage(status model.AllocationStatus) *orchv1alpha1.AllocationStatus {
	message := &orchv1alpha1.AllocationStatus{
		Id: status.ID, Application: status.Application, Service: status.Service, Replica: status.Replica,
		ApplicationGeneration: status.ApplicationGeneration, SpecDigest: status.SpecDigest,
		Phase: allocationPhaseMessage(status.Phase), Reason: status.Reason, Message: status.Message,
		RestartCount: status.RestartCount, UpdatedAt: timestamppb.New(status.UpdatedAt),
		HealthStatus: healthStatusMessage(status.HealthStatus), Address: status.Address,
		DeclaredPort:                status.DeclaredPort,
		ActiveSpecDigest:            status.ActiveSpecDigest,
		ActiveApplicationGeneration: status.ActiveGeneration, ActiveRevisionId: status.ActiveRevisionID,
	}
	if status.ExitCode != nil {
		message.HasExitCode = true
		message.ExitCode = *status.ExitCode
	}
	if status.ReadyAt != nil {
		message.ReadyAt = timestamppb.New(*status.ReadyAt)
	}
	return message
}

func routeStatusMessage(status model.RouteStatus) *orchv1alpha1.RouteStatus {
	return &orchv1alpha1.RouteStatus{
		Application: status.Application, Service: status.Service, Domain: status.Domain,
		Https: status.HTTPS, State: routeStateMessage(status.State), CertificateState: status.CertificateState,
		UpstreamCount: status.UpstreamCount, Reason: status.Reason, Message: status.Message,
		UpdatedAt: timestamppb.New(status.UpdatedAt),
	}
}

func tcpRouteStatusMessage(status model.TCPRouteStatus) *orchv1alpha1.TCPRouteStatus {
	return &orchv1alpha1.TCPRouteStatus{Application: status.Application, Service: status.Service,
		PublicPort: status.PublicPort, TargetPort: status.TargetPort, State: routeStateMessage(status.State),
		UpstreamCount: status.UpstreamCount, Reason: status.Reason, Message: status.Message,
		UpdatedAt: timestamppb.New(status.UpdatedAt)}
}

func cronJobStatusMessage(status model.CronJobStatus) *orchv1alpha1.CronJobStatus {
	message := &orchv1alpha1.CronJobStatus{Application: status.Application, Job: status.Job,
		SpecDigest: status.SpecDigest, ActiveRunId: status.ActiveRunID, LastRunId: status.LastRunID,
		LastState: jobRunStateMessage(status.LastState), Reason: status.Reason, Message: status.Message,
		UpdatedAt: timestamppb.New(status.UpdatedAt)}
	if status.NextRunAt != nil {
		message.NextRunAt = timestamppb.New(*status.NextRunAt)
	}
	return message
}

func jobRunStatusMessage(status model.JobRunStatus) *orchv1alpha1.JobRunStatus {
	message := &orchv1alpha1.JobRunStatus{Id: status.ID, Application: status.Application, Job: status.Job,
		SpecDigest: status.SpecDigest, ScheduledAt: timestamppb.New(status.ScheduledAt), State: jobRunStateMessage(status.State),
		Reason: status.Reason, Message: status.Message, UpdatedAt: timestamppb.New(status.UpdatedAt)}
	if status.ExitCode != nil {
		message.HasExitCode = true
		message.ExitCode = *status.ExitCode
	}
	if status.StartedAt != nil {
		message.StartedAt = timestamppb.New(*status.StartedAt)
	}
	if status.FinishedAt != nil {
		message.FinishedAt = timestamppb.New(*status.FinishedAt)
	}
	return message
}

func jobRunStateMessage(value string) orchv1alpha1.JobRunState {
	switch value {
	case model.JobRunWaiting:
		return orchv1alpha1.JobRunState_JOB_RUN_STATE_WAITING
	case model.JobRunRunning:
		return orchv1alpha1.JobRunState_JOB_RUN_STATE_RUNNING
	case model.JobRunSucceeded:
		return orchv1alpha1.JobRunState_JOB_RUN_STATE_SUCCEEDED
	case model.JobRunFailed:
		return orchv1alpha1.JobRunState_JOB_RUN_STATE_FAILED
	case model.JobRunTimedOut:
		return orchv1alpha1.JobRunState_JOB_RUN_STATE_TIMED_OUT
	case model.JobRunSkipped:
		return orchv1alpha1.JobRunState_JOB_RUN_STATE_SKIPPED
	default:
		return orchv1alpha1.JobRunState_JOB_RUN_STATE_UNSPECIFIED
	}
}

func healthStatusMessage(status string) orchv1alpha1.HealthStatus {
	switch status {
	case model.HealthNotConfigured:
		return orchv1alpha1.HealthStatus_HEALTH_STATUS_NOT_CONFIGURED
	case model.HealthChecking:
		return orchv1alpha1.HealthStatus_HEALTH_STATUS_CHECKING
	case model.HealthReady:
		return orchv1alpha1.HealthStatus_HEALTH_STATUS_READY
	case model.HealthUnhealthy:
		return orchv1alpha1.HealthStatus_HEALTH_STATUS_UNHEALTHY
	default:
		return orchv1alpha1.HealthStatus_HEALTH_STATUS_UNSPECIFIED
	}
}

func routeStateMessage(status string) orchv1alpha1.RouteState {
	switch status {
	case model.RoutePending:
		return orchv1alpha1.RouteState_ROUTE_STATE_PENDING
	case model.RouteActive:
		return orchv1alpha1.RouteState_ROUTE_STATE_ACTIVE
	case model.RouteFailed:
		return orchv1alpha1.RouteState_ROUTE_STATE_FAILED
	default:
		return orchv1alpha1.RouteState_ROUTE_STATE_UNSPECIFIED
	}
}

func allocationPhaseMessage(phase string) orchv1alpha1.AllocationPhase {
	switch phase {
	case model.AllocationPending:
		return orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_PENDING
	case model.AllocationPulling:
		return orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_PULLING
	case model.AllocationStarting:
		return orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_STARTING
	case model.AllocationRunning:
		return orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_RUNNING
	case model.AllocationStopped:
		return orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_STOPPED
	case model.AllocationBackoff:
		return orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_BACKOFF
	case model.AllocationFailed:
		return orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_FAILED
	default:
		return orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_UNSPECIFIED
	}
}

func rejectUnknownFields(message proto.Message) error {
	if message == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("request message is required"))
	}
	if len(message.ProtoReflect().GetUnknown()) != 0 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("request contains unknown fields"))
	}
	return nil
}
