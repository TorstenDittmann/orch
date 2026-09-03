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
	sessions   *sessionManager
	reconciler *Reconciler
}

func newService(state *store.Store, cluster model.Cluster, caPEM, caKey []byte, sessions *sessionManager, now func() time.Time) *service {
	if now == nil {
		now = time.Now
	}
	return &service{
		state: state, cluster: cluster, now: now, random: rand.Reader,
		caPEM: append([]byte(nil), caPEM...), caKey: append([]byte(nil), caKey...),
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
	return connect.NewResponse(&orchv1alpha1.GetClusterResponse{Cluster: clusterMessage(s.cluster)}), nil
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
	}
	for _, allocation := range application.Execution.Allocations {
		message.Allocations = append(message.Allocations, allocationStatusMessage(allocation))
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
	}
	if status.ExitCode != nil {
		message.HasExitCode = true
		message.ExitCode = *status.ExitCode
	}
	return message
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
