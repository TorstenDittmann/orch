package control

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	"change.codes/torsten/better-swarm/gen/orch/v1alpha1/orchv1alpha1connect"
	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/store"
	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
)

const (
	firstAgentMessageTimeout = 10 * time.Second
	maxReportedAllocations   = 1000
	maxReportedRoutes        = 1000
	maxReportedCronJobs      = 100
	maxReportedJobRuns       = 2000
)

type agentService struct {
	orchv1alpha1connect.UnimplementedAgentServiceHandler
	state      *store.Store
	sessions   *sessionManager
	reconciler *Reconciler
	now        func() time.Time
}

type receivedAgentMessage struct {
	message *orchv1alpha1.ConnectRequest
	err     error
}

func (s *agentService) Connect(ctx context.Context, stream *connect.BidiStream[orchv1alpha1.ConnectRequest, orchv1alpha1.ConnectResponse]) error {
	nodeID, err := requireAgent(ctx)
	if err != nil {
		return err
	}
	received := make(chan receivedAgentMessage, 1)
	receiveContext, cancelReceive := context.WithCancel(ctx)
	defer cancelReceive()
	go receiveAgentMessages(receiveContext, stream, received)
	firstTimer := time.NewTimer(firstAgentMessageTimeout)
	defer firstTimer.Stop()
	var first *orchv1alpha1.ConnectRequest
	select {
	case <-ctx.Done():
		return connect.NewError(connect.CodeCanceled, ctx.Err())
	case <-firstTimer.C:
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("agent Hello was not received in time"))
	case item := <-received:
		if item.err != nil {
			return streamReceiveError(item.err)
		}
		first = item.message
	}
	if err := rejectUnknownMessage(first); err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("Hello must be the first agent message"))
	}
	if err := validateHello(hello, nodeID); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}

	sessionContext, cancel := context.WithCancel(ctx)
	defer cancel()
	session, err := s.sessions.register(sessionContext, nodeID, hello.GetBootId(), cancel)
	if err != nil {
		return connect.NewError(connect.CodeAlreadyExists, err)
	}
	defer s.sessions.unregister(session)
	if err := s.state.UpdateAgentCapabilities(ctx, model.AgentCapabilities{
		NodeID: nodeID, BootID: hello.GetBootId(), OperatingSystem: hello.GetOperatingSystem(),
		Architecture: hello.GetArchitecture(), RuntimeVersion: hello.GetRuntimeVersion(),
		Snapshotter: hello.GetSnapshotter(), CgroupsV2: hello.GetCgroupsV2(),
		CNIVersion: hello.GetCniVersion(), Nftables: hello.GetNftables(),
		CNIPluginsVersion: hello.GetCniPluginsVersion(), DNSAvailable: hello.GetDnsAvailable(),
		CaddyVersion: hello.GetCaddyVersion(), TotalCPUMillis: hello.GetTotalCpuMillis(),
		TotalMemoryBytes: hello.GetTotalMemoryBytes(), AllocatableCPUMillis: hello.GetAllocatableCpuMillis(),
		AllocatableMemoryBytes: hello.GetAllocatableMemoryBytes(), AdvertiseAddress: hello.GetAdvertiseAddress(),
		WireGuardPublicKey: hello.GetWireguardPublicKey(), WireGuardListenPort: hello.GetWireguardListenPort(),
		SeenAt: s.now().UTC(),
	}); err != nil {
		return connect.NewError(connect.CodeInternal, errors.New("record agent capabilities"))
	}
	if err := s.reconciler.Reconcile(ctx); err != nil {
		return connect.NewError(connect.CodeInternal, errors.New("reconcile agent assignment"))
	}
	assignment, err := s.state.CurrentAssignment(ctx, nodeID)
	if err == nil {
		assignment, err = s.reconciler.HydrateAssignment(ctx, assignment)
		if err != nil {
			return connect.NewError(connect.CodeInternal, errors.New("hydrate current agent assignment"))
		}
		enqueueErr := enqueueAssignment(session, assignment)
		wipeMaterials(assignment.Materials)
		if enqueueErr != nil {
			return connect.NewError(connect.CodeResourceExhausted, enqueueErr)
		}
	} else if !errors.Is(err, store.ErrAssignmentNotFound) {
		return connect.NewError(connect.CodeInternal, errors.New("read current agent assignment"))
	}

	heartbeat := time.NewTimer(agentHeartbeatTimeout)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sessionContext.Done():
			return nil
		case response := <-session.send:
			if err := stream.Send(response); err != nil {
				return streamReceiveError(err)
			}
		case <-heartbeat.C:
			return connect.NewError(connect.CodeDeadlineExceeded, errors.New("agent heartbeat timed out"))
		case item := <-received:
			if item.err != nil {
				if errors.Is(item.err, io.EOF) || errors.Is(item.err, context.Canceled) {
					return nil
				}
				return streamReceiveError(item.err)
			}
			if err := rejectUnknownMessage(item.message); err != nil {
				return err
			}
			if item.message.GetHello() != nil {
				return connect.NewError(connect.CodeInvalidArgument, errors.New("Hello may be sent only once"))
			}
			if err := s.handleAgentMessage(ctx, session, item.message); err != nil {
				return err
			}
			s.sessions.touch(session)
			if !heartbeat.Stop() {
				select {
				case <-heartbeat.C:
				default:
				}
			}
			heartbeat.Reset(agentHeartbeatTimeout)
		}
	}
}

func receiveAgentMessages(ctx context.Context, stream *connect.BidiStream[orchv1alpha1.ConnectRequest, orchv1alpha1.ConnectResponse], output chan<- receivedAgentMessage) {
	for {
		message, err := stream.Receive()
		select {
		case <-ctx.Done():
			return
		case output <- receivedAgentMessage{message: message, err: err}:
		}
		if err != nil {
			return
		}
	}
}

func (s *agentService) handleAgentMessage(ctx context.Context, session *agentSession, request *orchv1alpha1.ConnectRequest) error {
	switch message := request.GetMessage().(type) {
	case *orchv1alpha1.ConnectRequest_AssignmentAck:
		if message.AssignmentAck == nil || len(message.AssignmentAck.ProtoReflect().GetUnknown()) != 0 || message.AssignmentAck.GetBootId() != session.bootID {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("assignment ACK has the wrong boot id"))
		}
		if err := s.state.AcknowledgeAssignment(ctx, session.nodeID, message.AssignmentAck.GetGeneration(), message.AssignmentAck.GetDigest()); err != nil {
			if errors.Is(err, store.ErrAssignmentNotFound) {
				return connect.NewError(connect.CodeFailedPrecondition, err)
			}
			return connect.NewError(connect.CodeInvalidArgument, err)
		}
	case *orchv1alpha1.ConnectRequest_AllocationReport:
		if message.AllocationReport == nil || len(message.AllocationReport.ProtoReflect().GetUnknown()) != 0 || message.AllocationReport.GetBootId() != session.bootID {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("allocation report has the wrong boot id"))
		}
		if len(message.AllocationReport.GetAllocations()) > maxReportedAllocations ||
			len(message.AllocationReport.GetRoutes()) > maxReportedRoutes ||
			len(message.AllocationReport.GetTcpRoutes()) > maxReportedRoutes ||
			len(message.AllocationReport.GetCronJobs()) > maxReportedCronJobs ||
			len(message.AllocationReport.GetJobRuns()) > maxReportedJobRuns {
			return connect.NewError(connect.CodeResourceExhausted, errors.New("allocation report is too large"))
		}
		report, err := allocationReportModel(session.nodeID, message.AllocationReport, s.now().UTC())
		if err != nil {
			return connect.NewError(connect.CodeInvalidArgument, err)
		}
		recorded, err := s.state.RecordAgentReport(ctx, report)
		if err != nil {
			if errors.Is(err, store.ErrStaleAgentReport) {
				return connect.NewError(connect.CodeFailedPrecondition, err)
			}
			return connect.NewError(connect.CodeInvalidArgument, err)
		}
		if recorded && s.reconciler != nil {
			if err := s.reconciler.Reconcile(ctx); err != nil {
				return connect.NewError(connect.CodeInternal, errors.New("reconcile reported agent state"))
			}
		}
	case *orchv1alpha1.ConnectRequest_Heartbeat:
		if message.Heartbeat == nil || len(message.Heartbeat.ProtoReflect().GetUnknown()) != 0 || message.Heartbeat.GetBootId() != session.bootID {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("heartbeat has the wrong boot id"))
		}
		if err := s.state.TouchAgent(ctx, session.nodeID, session.bootID, s.now().UTC()); err != nil {
			return connect.NewError(connect.CodeFailedPrecondition, err)
		}
	case *orchv1alpha1.ConnectRequest_OperationResult:
		if message.OperationResult == nil || len(message.OperationResult.ProtoReflect().GetUnknown()) != 0 {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("operation result contains unknown fields"))
		}
		if err := s.sessions.routeOperation(session, message.OperationResult); err != nil {
			return connect.NewError(connect.CodeInvalidArgument, err)
		}
	default:
		return connect.NewError(connect.CodeInvalidArgument, errors.New("agent message type is required"))
	}
	return nil
}

func rejectUnknownMessage(message proto.Message) error {
	if message == nil || len(message.ProtoReflect().GetUnknown()) != 0 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("agent message contains unknown fields"))
	}
	return nil
}

func validateHello(hello *orchv1alpha1.AgentHello, nodeID string) error {
	if err := rejectUnknownMessage(hello); err != nil {
		return err
	}
	if hello.GetProtocolVersion() != agentProtocolVersion {
		return fmt.Errorf("unsupported agent protocol %q", hello.GetProtocolVersion())
	}
	if hello.GetNodeId() != nodeID {
		return errors.New("Hello node id does not match the authenticated certificate")
	}
	for name, value := range map[string]string{
		"boot id": hello.GetBootId(), "operating system": hello.GetOperatingSystem(),
		"architecture": hello.GetArchitecture(), "runtime version": hello.GetRuntimeVersion(),
		"snapshotter": hello.GetSnapshotter(),
	} {
		if strings.TrimSpace(value) == "" || len(value) > 128 {
			return fmt.Errorf("%s is required and must not exceed 128 bytes", name)
		}
	}
	for name, value := range map[string]string{
		"CNI version": hello.GetCniVersion(), "CNI plugins version": hello.GetCniPluginsVersion(),
		"Caddy version": hello.GetCaddyVersion(),
	} {
		if len(value) > 128 {
			return fmt.Errorf("%s must not exceed 128 bytes", name)
		}
	}
	if (hello.GetCachedAssignmentGeneration() == 0) != (hello.GetCachedAssignmentDigest() == "") {
		return errors.New("cached assignment generation and digest must be supplied together")
	}
	if hello.GetCachedAssignmentDigest() != "" && !validSHA256Digest(hello.GetCachedAssignmentDigest()) {
		return errors.New("cached assignment digest is invalid")
	}
	if err := model.ValidateMeshIdentity(hello.GetAdvertiseAddress(), hello.GetWireguardPublicKey(), hello.GetWireguardListenPort()); err != nil {
		return err
	}
	return nil
}

func validSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == 32
}

func allocationReportModel(nodeID string, report *orchv1alpha1.AllocationReport, now time.Time) (model.AgentReport, error) {
	if report.GetSequence() == 0 || report.GetAssignmentGeneration() == 0 ||
		!validSHA256Digest(report.GetAssignmentDigest()) {
		return model.AgentReport{}, errors.New("allocation report sequence, assignment generation, and digest are required")
	}
	result := model.AgentReport{
		NodeID: nodeID, BootID: report.GetBootId(), Sequence: report.GetSequence(),
		AssignmentGeneration: report.GetAssignmentGeneration(), AssignmentDigest: report.GetAssignmentDigest(),
		ReportedAt: now, Allocations: make([]model.AllocationStatus, 0, len(report.GetAllocations())),
		Routes:    make([]model.RouteStatus, 0, len(report.GetRoutes())),
		TCPRoutes: make([]model.TCPRouteStatus, 0, len(report.GetTcpRoutes())),
		CronJobs:  make([]model.CronJobStatus, 0, len(report.GetCronJobs())),
		JobRuns:   make([]model.JobRunStatus, 0, len(report.GetJobRuns())),
	}
	if network := report.GetNetwork(); network != nil {
		if err := rejectUnknownMessage(network); err != nil {
			return model.AgentReport{}, err
		}
		result.Network = model.NetworkStatus{
			Generation: network.GetGeneration(), State: networkStateModel(network.GetState()),
			ClusterCIDR: network.GetClusterCidr(), NodeSubnet: network.GetNodeSubnet(), Gateway: network.GetGateway(),
			Reason: network.GetReason(), Message: network.GetMessage(), DNSState: network.GetDnsState(),
			IngressState: network.GetIngressState(), DrainingObjects: network.GetDrainingObjects(),
		}
	}
	var previousApplication, previousService, previousID string
	var previousReplica uint32
	for _, status := range report.GetAllocations() {
		if err := rejectUnknownMessage(status); err != nil {
			return model.AgentReport{}, err
		}
		if status.GetUpdatedAt() == nil || status.GetUpdatedAt().CheckValid() != nil ||
			len(status.GetReason()) > 128 || len(status.GetMessage()) > 1024 || status.GetNodeId() != "" ||
			status.GetNodeName() != "" || status.GetPlacementReason() != "" {
			return model.AgentReport{}, fmt.Errorf("allocation %q has an invalid update time", status.GetId())
		}
		if len(result.Allocations) != 0 && !allocationObservationAfter(
			previousApplication, previousService, previousReplica, previousID,
			status.GetApplication(), status.GetService(), status.GetReplica(), status.GetId(),
		) {
			return model.AgentReport{}, errors.New("allocation observations are not in strict canonical order")
		}
		item := model.AllocationStatus{
			ID: status.GetId(), Application: status.GetApplication(), Service: status.GetService(), Replica: status.GetReplica(),
			ApplicationGeneration: status.GetApplicationGeneration(), SpecDigest: status.GetSpecDigest(),
			Phase: allocationPhaseModel(status.GetPhase()), Reason: status.GetReason(), Message: status.GetMessage(),
			RestartCount: status.GetRestartCount(), UpdatedAt: status.GetUpdatedAt().AsTime(),
			HealthStatus: healthStatusModel(status.GetHealthStatus()), Address: status.GetAddress(),
			ActiveSpecDigest: status.GetActiveSpecDigest(),
			ActiveGeneration: status.GetActiveApplicationGeneration(), ActiveRevisionID: status.GetActiveRevisionId(),
			DeclaredPort: status.GetDeclaredPort(),
		}
		if status.GetReadyAt() != nil {
			if status.GetReadyAt().CheckValid() != nil {
				return model.AgentReport{}, fmt.Errorf("allocation %q has an invalid ready time", status.GetId())
			}
			readyAt := status.GetReadyAt().AsTime()
			item.ReadyAt = &readyAt
		}
		if status.GetHasExitCode() {
			exit := status.GetExitCode()
			item.ExitCode = &exit
		}
		result.Allocations = append(result.Allocations, item)
		previousApplication, previousService, previousReplica, previousID =
			item.Application, item.Service, item.Replica, item.ID
	}
	previousDomain := ""
	for _, route := range report.GetRoutes() {
		if err := rejectUnknownMessage(route); err != nil {
			return model.AgentReport{}, err
		}
		if route.GetUpdatedAt() == nil || route.GetUpdatedAt().CheckValid() != nil ||
			len(route.GetReason()) > 128 || len(route.GetMessage()) > 1024 ||
			route.GetNodeId() != "" || route.GetNodeName() != "" ||
			(previousDomain != "" && route.GetDomain() <= previousDomain) {
			return model.AgentReport{}, fmt.Errorf("route observations are not in strict canonical order or contain invalid data")
		}
		item := model.RouteStatus{
			Application: route.GetApplication(), Service: route.GetService(), Domain: route.GetDomain(),
			HTTPS: route.GetHttps(), State: routeStateModel(route.GetState()),
			CertificateState: route.GetCertificateState(), UpstreamCount: route.GetUpstreamCount(),
			Reason: route.GetReason(), Message: route.GetMessage(), UpdatedAt: route.GetUpdatedAt().AsTime(),
		}
		result.Routes = append(result.Routes, item)
		previousDomain = item.Domain
	}
	var previousPort uint32
	for _, route := range report.GetTcpRoutes() {
		if err := rejectUnknownMessage(route); err != nil {
			return model.AgentReport{}, err
		}
		if route.GetUpdatedAt() == nil || route.GetUpdatedAt().CheckValid() != nil || route.GetPublicPort() == 0 ||
			(previousPort != 0 && route.GetPublicPort() <= previousPort) || len(route.GetReason()) > 128 || len(route.GetMessage()) > 1024 ||
			route.GetNodeId() != "" || route.GetNodeName() != "" {
			return model.AgentReport{}, errors.New("TCP route observations are not in strict canonical order or contain invalid data")
		}
		result.TCPRoutes = append(result.TCPRoutes, model.TCPRouteStatus{Application: route.GetApplication(), Service: route.GetService(),
			PublicPort: route.GetPublicPort(), TargetPort: route.GetTargetPort(), State: routeStateModel(route.GetState()),
			UpstreamCount: route.GetUpstreamCount(), Reason: route.GetReason(), Message: route.GetMessage(), UpdatedAt: route.GetUpdatedAt().AsTime()})
		previousPort = route.GetPublicPort()
	}
	previousJob := ""
	for _, status := range report.GetCronJobs() {
		if err := rejectUnknownMessage(status); err != nil {
			return model.AgentReport{}, err
		}
		key := status.GetApplication() + "\x00" + status.GetJob()
		if status.GetUpdatedAt() == nil || status.GetUpdatedAt().CheckValid() != nil || (previousJob != "" && key <= previousJob) {
			return model.AgentReport{}, errors.New("cron job observations are not in strict canonical order or contain invalid data")
		}
		item := model.CronJobStatus{Application: status.GetApplication(), Job: status.GetJob(), SpecDigest: status.GetSpecDigest(),
			ActiveRunID: status.GetActiveRunId(), LastRunID: status.GetLastRunId(), LastState: jobRunStateModel(status.GetLastState()),
			Reason: status.GetReason(), Message: status.GetMessage(), UpdatedAt: status.GetUpdatedAt().AsTime()}
		if status.GetNextRunAt() != nil {
			if status.GetNextRunAt().CheckValid() != nil {
				return model.AgentReport{}, errors.New("cron job next run time is invalid")
			}
			next := status.GetNextRunAt().AsTime()
			item.NextRunAt = &next
		}
		result.CronJobs = append(result.CronJobs, item)
		previousJob = key
	}
	for _, status := range report.GetJobRuns() {
		if err := rejectUnknownMessage(status); err != nil {
			return model.AgentReport{}, err
		}
		if status.GetScheduledAt() == nil || status.GetScheduledAt().CheckValid() != nil || status.GetUpdatedAt() == nil || status.GetUpdatedAt().CheckValid() != nil {
			return model.AgentReport{}, errors.New("cron run observations are not in canonical recent-first order or contain invalid data")
		}
		item := model.JobRunStatus{ID: status.GetId(), Application: status.GetApplication(), Job: status.GetJob(),
			SpecDigest: status.GetSpecDigest(), ScheduledAt: status.GetScheduledAt().AsTime(), State: jobRunStateModel(status.GetState()),
			Reason: status.GetReason(), Message: status.GetMessage(), UpdatedAt: status.GetUpdatedAt().AsTime()}
		if status.GetHasExitCode() {
			value := status.GetExitCode()
			item.ExitCode = &value
		}
		if status.GetStartedAt() != nil {
			if status.GetStartedAt().CheckValid() != nil {
				return model.AgentReport{}, errors.New("cron run start time is invalid")
			}
			value := status.GetStartedAt().AsTime()
			item.StartedAt = &value
		}
		if status.GetFinishedAt() != nil {
			if status.GetFinishedAt().CheckValid() != nil {
				return model.AgentReport{}, errors.New("cron run finish time is invalid")
			}
			value := status.GetFinishedAt().AsTime()
			item.FinishedAt = &value
		}
		result.JobRuns = append(result.JobRuns, item)
	}
	return result, nil
}

func jobRunStateModel(value orchv1alpha1.JobRunState) string {
	switch value {
	case orchv1alpha1.JobRunState_JOB_RUN_STATE_WAITING:
		return model.JobRunWaiting
	case orchv1alpha1.JobRunState_JOB_RUN_STATE_RUNNING:
		return model.JobRunRunning
	case orchv1alpha1.JobRunState_JOB_RUN_STATE_SUCCEEDED:
		return model.JobRunSucceeded
	case orchv1alpha1.JobRunState_JOB_RUN_STATE_FAILED:
		return model.JobRunFailed
	case orchv1alpha1.JobRunState_JOB_RUN_STATE_TIMED_OUT:
		return model.JobRunTimedOut
	case orchv1alpha1.JobRunState_JOB_RUN_STATE_SKIPPED:
		return model.JobRunSkipped
	default:
		return ""
	}
}

func allocationObservationAfter(previousApplication, previousService string, previousReplica uint32, previousID,
	application, service string, replica uint32, id string,
) bool {
	if application != previousApplication {
		return application > previousApplication
	}
	if service != previousService {
		return service > previousService
	}
	if replica != previousReplica {
		return replica > previousReplica
	}
	return id > previousID
}

func allocationPhaseModel(phase orchv1alpha1.AllocationPhase) string {
	switch phase {
	case orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_PENDING:
		return model.AllocationPending
	case orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_PULLING:
		return model.AllocationPulling
	case orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_STARTING:
		return model.AllocationStarting
	case orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_RUNNING:
		return model.AllocationRunning
	case orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_STOPPED:
		return model.AllocationStopped
	case orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_BACKOFF:
		return model.AllocationBackoff
	case orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_FAILED:
		return model.AllocationFailed
	default:
		return ""
	}
}

func healthStatusModel(status orchv1alpha1.HealthStatus) string {
	switch status {
	case orchv1alpha1.HealthStatus_HEALTH_STATUS_NOT_CONFIGURED:
		return model.HealthNotConfigured
	case orchv1alpha1.HealthStatus_HEALTH_STATUS_CHECKING:
		return model.HealthChecking
	case orchv1alpha1.HealthStatus_HEALTH_STATUS_READY:
		return model.HealthReady
	case orchv1alpha1.HealthStatus_HEALTH_STATUS_UNHEALTHY:
		return model.HealthUnhealthy
	default:
		return ""
	}
}

func networkStateModel(status orchv1alpha1.NetworkState) string {
	switch status {
	case orchv1alpha1.NetworkState_NETWORK_STATE_PENDING:
		return model.NetworkPending
	case orchv1alpha1.NetworkState_NETWORK_STATE_READY:
		return model.NetworkReady
	case orchv1alpha1.NetworkState_NETWORK_STATE_FAILED:
		return model.NetworkFailed
	default:
		return ""
	}
}

func routeStateModel(status orchv1alpha1.RouteState) string {
	switch status {
	case orchv1alpha1.RouteState_ROUTE_STATE_PENDING:
		return model.RoutePending
	case orchv1alpha1.RouteState_ROUTE_STATE_ACTIVE:
		return model.RouteActive
	case orchv1alpha1.RouteState_ROUTE_STATE_FAILED:
		return model.RouteFailed
	default:
		return ""
	}
}

func streamReceiveError(err error) error {
	if connect.CodeOf(err) != connect.CodeUnknown {
		return err
	}
	return connect.NewError(connect.CodeUnavailable, errors.New("agent stream closed"))
}
