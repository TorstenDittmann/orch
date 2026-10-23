package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/gen/orch/v1alpha1/orchv1alpha1connect"
	"github.com/TorstenDittmann/orch/internal/agent/state"
	"github.com/TorstenDittmann/orch/internal/buildinfo"
	"github.com/TorstenDittmann/orch/internal/model"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// heartbeatInterval is a variable so tests can shorten it.
var heartbeatInterval = 10 * time.Second

const (
	reconcileInterval = 2 * time.Second
	// stableSessionDuration is how long a session must stay up for its loss to
	// start a fresh reconnect backoff instead of continuing an earlier one.
	stableSessionDuration  = 30 * time.Second
	maxReconnectBackoff    = 30 * time.Second
	maxOperationChunk      = 64 << 10
	maxAssignedAllocations = 1000
	maxAssignedCronJobs    = 132
	maxSecretBundleBytes   = 16 << 20
)

type sessionRunner struct {
	credentials            Credentials
	registry               *controllerRegistry
	cache                  *state.Store
	supervisor             *Supervisor
	capabilities           orchruntime.Capabilities
	bootID                 string
	now                    func() time.Time
	random                 io.Reader
	reportSeq              uint64
	assignmentAcknowledged bool
	// ackPending is set while the cached assignment received on this
	// connection has not converged far enough to acknowledge. The reconcile
	// tick retries it and sends the ACK, so a transient runtime fault does not
	// cost the session.
	ackPending     bool
	dataDir        string
	volumes        *volumeBackups
	renewalRetryAt time.Time
	renewalBackoff time.Duration
	usage          *usageSampler
	allocationCPU  *allocationSampler
	// keepalive sends a heartbeat on the connected stream. It is set while a
	// session is connected so a long reconcile, such as a replica given minutes
	// to stop, cannot let the controller time the session out.
	keepalive func(context.Context) error
	// selfUpgrade is set for managed node installs, which run orch node
	// upgrade when the controller sends an UpgradeCommand.
	selfUpgrade       bool
	upgradeStagingDir string
	upgrading         atomic.Bool
}

type controllerMessage struct {
	message *orchv1alpha1.ConnectResponse
	err     error
}

type activeOperation struct {
	cancel context.CancelFunc
}

type agentOperations struct {
	mu     sync.Mutex
	active map[string]*activeOperation
}

func newAgentOperations() *agentOperations {
	return &agentOperations{active: make(map[string]*activeOperation)}
}

func (o *agentOperations) add(id string, cancel context.CancelFunc) (*activeOperation, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, exists := o.active[id]; exists {
		return nil, false
	}
	operation := &activeOperation{cancel: cancel}
	o.active[id] = operation
	return operation, true
}

func (o *agentOperations) finish(id string, operation *activeOperation) {
	o.mu.Lock()
	if o.active[id] == operation {
		delete(o.active, id)
	}
	o.mu.Unlock()
	operation.cancel()
}

func (o *agentOperations) cancel(id string) {
	o.mu.Lock()
	operation := o.active[id]
	if operation != nil {
		delete(o.active, id)
	}
	o.mu.Unlock()
	if operation != nil {
		operation.cancel()
	}
}

func (o *agentOperations) cancelAll() {
	o.mu.Lock()
	active := o.active
	o.active = make(map[string]*activeOperation)
	o.mu.Unlock()
	for _, operation := range active {
		operation.cancel()
	}
}

func (r *sessionRunner) run(ctx context.Context) error {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := r.reconcileCached(ctx); err != nil {
			return err
		}
		renewed, err := r.maybeRenewCertificate(ctx)
		if err != nil {
			r.noteRenewalFailure()
		} else if renewed {
			r.renewalRetryAt = time.Time{}
			r.renewalBackoff = 0
		}
		if r.certificateExpired() {
			// An expired identity cannot reconnect or renew, but cached workload
			// supervision remains useful and must not stop with the control plane.
			if r.renewalRetryAt.IsZero() {
				r.noteRenewalFailure()
			}
			if err := r.waitToReconnect(ctx, r.certificateRenewalDelay()); err != nil {
				return err
			}
			continue
		}
		started := time.Now()
		err = r.connect(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if connect.CodeOf(err) == connect.CodePermissionDenied || connect.CodeOf(err) == connect.CodeUnauthenticated {
			return err
		}
		backoff = reconnectBackoff(backoff, err, time.Since(started))
		jitter, err := reconnectJitter(r.random, backoff/2)
		if err != nil {
			return fmt.Errorf("generate reconnect jitter: %w", err)
		}
		if err := r.waitToReconnect(ctx, backoff+jitter); err != nil {
			return err
		}
		backoff = min(backoff*2, maxReconnectBackoff)
	}
}

// reconnectBackoff returns the delay before the next connection attempt. A
// session that ended cleanly or stayed up for a while was healthy, so its loss
// starts over at one second; otherwise one brief failure after an earlier run of
// them would keep the node offline for the full maximum.
func reconnectBackoff(current time.Duration, err error, lasted time.Duration) time.Duration {
	if err == nil || lasted >= stableSessionDuration {
		return time.Second
	}
	return current
}

func (r *sessionRunner) waitToReconnect(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return nil
		case <-ticker.C:
			if err := r.reconcileCached(ctx); err != nil {
				return err
			}
		}
	}
}

func (r *sessionRunner) heartbeatRequest(ctx context.Context) *orchv1alpha1.ConnectRequest {
	assignmentGeneration := uint64(0)
	if assignment, err := r.cache.Assignment(ctx); err == nil {
		assignmentGeneration = assignment.Generation
	}
	return &orchv1alpha1.ConnectRequest{Message: &orchv1alpha1.ConnectRequest_Heartbeat{Heartbeat: &orchv1alpha1.AgentHeartbeat{
		BootId: r.bootID, AcknowledgedAssignmentGeneration: assignmentGeneration, Usage: r.usage.Sample(),
		Allocations: r.allocationCPU.Sample(ctx),
	}}}
}

// reconcile runs one supervisor pass and keeps the session alive with
// heartbeats while it takes longer than a heartbeat interval.
func (r *sessionRunner) reconcile(ctx context.Context, assignment model.Assignment) ([]model.AllocationStatus, error) {
	if r.keepalive == nil {
		return r.supervisor.Reconcile(ctx, assignment)
	}
	type result struct {
		statuses []model.AllocationStatus
		err      error
	}
	done := make(chan result, 1)
	go func() {
		statuses, err := r.supervisor.Reconcile(ctx, assignment)
		done <- result{statuses, err}
	}()
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	keepalive := r.keepalive
	for {
		select {
		case finished := <-done:
			return finished.statuses, finished.err
		case <-ticker.C:
			if keepalive == nil {
				continue
			}
			if err := keepalive(ctx); err != nil {
				// The stream is gone; the loop sees that on its next send. The
				// pass itself still has to finish before the agent moves on.
				log.Printf("agent: heartbeat during reconcile: %v", err)
				keepalive = nil
			}
		}
	}
}

func (r *sessionRunner) reconcileCached(ctx context.Context) error {
	assignment, err := r.cache.Assignment(ctx)
	if errors.Is(err, state.ErrNoAssignment) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read cached assignment while disconnected: %w", err)
	}
	if _, err := r.supervisor.Reconcile(ctx, assignment); err != nil && ctx.Err() == nil {
		// A reconcile fault, even a persistent one, must not stop the agent:
		// only a connected agent can receive the assignment that repairs it.
		// The next tick retries the pass.
		log.Printf("agent: reconcile cached assignment while disconnected: %v", err)
	}
	return nil
}

func reconnectJitter(source io.Reader, maximum time.Duration) (time.Duration, error) {
	if maximum <= 0 {
		return 0, nil
	}
	if source == nil {
		source = rand.Reader
	}
	value, err := rand.Int(source, big.NewInt(int64(maximum)+1))
	if err != nil {
		return 0, err
	}
	return time.Duration(value.Int64()), nil
}

func (r *sessionRunner) connect(ctx context.Context) error {
	r.assignmentAcknowledged, r.ackPending = false, false
	if err := r.writeLocalStatus(nil); err != nil {
		return err
	}
	defer func() { r.assignmentAcknowledged = false; _ = r.writeLocalStatus(nil) }()
	tlsConfig := r.agentTLSConfig()
	transport := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		DialContext:       (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, TLSClientConfig: tlsConfig, TLSHandshakeTimeout: 5 * time.Second,
		IdleConnTimeout: 60 * time.Second,
	}
	httpClient := &http.Client{Transport: transport}
	defer transport.CloseIdleConnections()
	client := orchv1alpha1connect.NewAgentServiceClient(httpClient, r.credentials.Config.Server,
		connect.WithSendMaxBytes(20<<20), connect.WithReadMaxBytes(20<<20))
	stream := client.Connect(ctx)
	cached, cachedErr := r.cache.Assignment(ctx)
	if cachedErr != nil && !errors.Is(cachedErr, state.ErrNoAssignment) {
		return fmt.Errorf("read cached assignment: %w", cachedErr)
	}
	hello := &orchv1alpha1.AgentHello{
		ProtocolVersion: "v1alpha1-workloads-5", NodeId: r.credentials.Config.NodeID, BootId: r.bootID,
		OperatingSystem: r.capabilities.OperatingSystem, Architecture: r.capabilities.Architecture,
		RuntimeVersion: r.capabilities.RuntimeVersion, Snapshotter: r.capabilities.Snapshotter,
		CgroupsV2: r.capabilities.CgroupsV2, CniVersion: r.capabilities.CNIVersion,
		CniPluginsVersion: r.capabilities.CNIPluginsVersion, Nftables: r.capabilities.Nftables,
		CaddyVersion: r.capabilities.CaddyVersion, DnsAvailable: r.capabilities.DNSAvailable,
		TotalCpuMillis: r.capabilities.TotalCPUMillis, TotalMemoryBytes: r.capabilities.TotalMemoryBytes,
		AllocatableCpuMillis: r.capabilities.AllocatableCPUMillis, AllocatableMemoryBytes: r.capabilities.AllocatableMemoryBytes,
		AdvertiseAddress: r.capabilities.AdvertiseAddress, WireguardPublicKey: r.capabilities.WireGuardPublicKey,
		WireguardListenPort: r.capabilities.WireGuardListenPort,
		AgentVersion:        buildinfo.Version, AgentBuildId: buildinfo.BuildID, SelfUpgrade: r.selfUpgrade,
	}
	if cachedErr == nil {
		hello.CachedAssignmentGeneration = cached.Generation
		hello.CachedAssignmentDigest = cached.Digest
	}
	if err := stream.Send(&orchv1alpha1.ConnectRequest{Message: &orchv1alpha1.ConnectRequest_Hello{Hello: hello}}); err != nil {
		return err
	}
	received := make(chan controllerMessage, 1)
	receiveContext, cancelReceive := context.WithCancel(ctx)
	defer cancelReceive()
	go receiveControllerMessages(receiveContext, stream, received)
	// Control messages (heartbeats, ACKs, reports) and operation frames use
	// separate queues so a large log tail or exec stream can never fill the
	// queue that keeps the session alive.
	outbound := make(chan *orchv1alpha1.ConnectRequest, 64)
	operationOutbound := make(chan *orchv1alpha1.ConnectRequest, 64)
	operations := newAgentOperations()
	defer func() {
		operations.cancelAll()
		_ = stream.CloseRequest()
		_ = stream.CloseResponse()
	}()
	// Cached state may have been committed before reconciliation failed and
	// therefore never acknowledged. Wait for the controller's assignment and
	// send its ACK before publishing reports on this connection.
	heartbeat := time.NewTicker(heartbeatInterval)
	reconcile := time.NewTicker(reconcileInterval)
	renewal := time.NewTimer(r.certificateRenewalDelay())
	defer heartbeat.Stop()
	defer reconcile.Stop()
	defer renewal.Stop()
	// Only this loop sends on the stream, and it is blocked while a reconcile
	// runs, so sending the keepalive directly cannot interleave with it.
	r.keepalive = func(ctx context.Context) error { return r.sendRequest(stream, r.heartbeatRequest(ctx)) }
	defer func() { r.keepalive = nil }()
	for {
		// Control messages always go out before queued operation frames.
		if sent, err := r.sendControl(stream, outbound); err != nil {
			return err
		} else if sent {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case request := <-outbound:
			if err := r.sendRequest(stream, request); err != nil {
				return err
			}
		case request := <-operationOutbound:
			if err := r.sendRequest(stream, request); err != nil {
				return err
			}
		case item := <-received:
			if item.err != nil {
				return item.err
			}
			if err := r.handleControllerMessage(ctx, item.message, outbound, operationOutbound, operations); err != nil {
				return err
			}
		case <-heartbeat.C:
			if err := enqueueAgentRequest(outbound, r.heartbeatRequest(ctx)); err != nil {
				return err
			}
		case <-reconcile.C:
			assignment, err := r.cache.Assignment(ctx)
			if errors.Is(err, state.ErrNoAssignment) {
				continue
			}
			if err != nil {
				return fmt.Errorf("read cached assignment: %w", err)
			}
			statuses, err := r.reconcile(ctx, assignment)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				// Stay connected and retry on the next tick. Per-allocation
				// failures still produce complete statuses worth reporting.
				log.Printf("agent: reconcile cached assignment: %v", err)
				if !errors.Is(err, ErrAllocationReconcile) {
					continue
				}
			}
			if r.ackPending {
				if err := r.enqueueAck(outbound, assignment); err != nil {
					return err
				}
			}
			if r.assignmentAcknowledged {
				if err := r.enqueueReport(outbound, assignment, statuses); err != nil {
					return err
				}
			}
		case <-renewal.C:
			return errCertificateRenewalDue
		}
	}
}

type requestSender interface {
	Send(*orchv1alpha1.ConnectRequest) error
}

// sendControl sends one queued control message without blocking and reports
// whether one was sent.
func (r *sessionRunner) sendControl(stream requestSender, outbound <-chan *orchv1alpha1.ConnectRequest) (bool, error) {
	select {
	case request := <-outbound:
		return true, r.sendRequest(stream, request)
	default:
		return false, nil
	}
}

func (r *sessionRunner) sendRequest(stream requestSender, request *orchv1alpha1.ConnectRequest) error {
	if err := stream.Send(request); err != nil {
		return err
	}
	if request.GetAssignmentAck() != nil {
		r.assignmentAcknowledged = true
	}
	if report := request.GetAllocationReport(); report != nil {
		if err := r.writeLocalStatus(report); err != nil {
			return err
		}
	}
	return nil
}

func (r *sessionRunner) handleControllerMessage(ctx context.Context, response *orchv1alpha1.ConnectResponse, outbound, operationOutbound chan<- *orchv1alpha1.ConnectRequest, operations *agentOperations) error {
	if response == nil || len(response.ProtoReflect().GetUnknown()) != 0 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("controller message is invalid or contains unknown fields"))
	}
	switch message := response.GetMessage().(type) {
	case *orchv1alpha1.ConnectResponse_Assignment:
		if err := rejectAssignmentUnknownFields(message.Assignment); err != nil {
			return err
		}
		assignment, err := assignmentModel(message.Assignment, r.credentials.Config.NodeID)
		if err != nil {
			return connect.NewError(connect.CodeInvalidArgument, err)
		}
		if _, err := r.cache.CommitAssignment(ctx, assignment); err != nil {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("durably cache assignment: %w", err))
		}
		r.volumes.assignmentChanged()
		statuses, err := r.reconcile(ctx, assignment)
		if err != nil && !errors.Is(err, ErrAllocationReconcile) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Hold the ACK and stay connected: the reconcile tick retries the
			// cached assignment and acknowledges it once the pass converges.
			// Reports wait too, since the controller rejects a report for an
			// assignment it has not seen acknowledged.
			log.Printf("agent: reconcile assignment %d: %v", assignment.Generation, err)
			r.ackPending = true
			return nil
		}
		if err != nil {
			// The pass converged for the assignment as a whole; the failed
			// allocations are reported as reconcile_failed and retried.
			log.Printf("agent: reconcile assignment: %v", err)
		}
		if err := r.enqueueAck(outbound, assignment); err != nil {
			return err
		}
		return r.enqueueReport(outbound, assignment, statuses)
	case *orchv1alpha1.ConnectResponse_TailLogs:
		command := message.TailLogs
		if command == nil || len(command.ProtoReflect().GetUnknown()) != 0 || command.GetRequestId() == "" ||
			len(command.GetRequestId()) > 128 || command.GetAllocationId() == "" || command.GetTailLines() == 0 || command.GetTailLines() > 10_000 {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid log command"))
		}
		operationContext, cancel := context.WithCancel(ctx)
		operation, added := operations.add(command.GetRequestId(), cancel)
		if !added {
			cancel()
			return connect.NewError(connect.CodeAlreadyExists, errors.New("operation request id is already active"))
		}
		go func() {
			defer operations.finish(command.GetRequestId(), operation)
			r.runTailLogs(operationContext, command, operationOutbound)
		}()
	case *orchv1alpha1.ConnectResponse_TailCronLogs:
		command := message.TailCronLogs
		if command == nil || len(command.ProtoReflect().GetUnknown()) != 0 || command.GetRequestId() == "" ||
			command.GetRunId() == "" || command.GetTailLines() == 0 || command.GetTailLines() > 10_000 {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid cron log command"))
		}
		operationContext, cancel := context.WithCancel(ctx)
		operation, added := operations.add(command.GetRequestId(), cancel)
		if !added {
			cancel()
			return connect.NewError(connect.CodeAlreadyExists, errors.New("operation request id is already active"))
		}
		go func() {
			defer operations.finish(command.GetRequestId(), operation)
			r.runTailCronLogs(operationContext, command, operationOutbound)
		}()
	case *orchv1alpha1.ConnectResponse_Exec:
		command := message.Exec
		if command == nil || len(command.ProtoReflect().GetUnknown()) != 0 || command.GetRequestId() == "" ||
			len(command.GetRequestId()) > 128 || command.GetAllocationId() == "" || len(command.GetCommand()) == 0 || len(command.GetCommand()) > 64 {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid exec command"))
		}
		commandBytes := 0
		for _, argument := range command.GetCommand() {
			if argument == "" || strings.ContainsRune(argument, 0) {
				return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid exec command argument"))
			}
			commandBytes += len(argument)
		}
		if commandBytes > 8<<10 {
			return connect.NewError(connect.CodeResourceExhausted, errors.New("exec command exceeds 8 KiB"))
		}
		timeout := time.Duration(command.GetTimeoutSeconds()) * time.Second
		if timeout <= 0 || timeout > 5*time.Minute {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid exec timeout"))
		}
		operationContext, cancel := context.WithTimeout(ctx, timeout)
		operation, added := operations.add(command.GetRequestId(), cancel)
		if !added {
			cancel()
			return connect.NewError(connect.CodeAlreadyExists, errors.New("operation request id is already active"))
		}
		go func() {
			defer operations.finish(command.GetRequestId(), operation)
			r.runExec(operationContext, ctx, command, operationOutbound)
		}()
	case *orchv1alpha1.ConnectResponse_Volume:
		command := message.Volume
		if command == nil || len(command.ProtoReflect().GetUnknown()) != 0 || command.GetRequestId() == "" ||
			len(command.GetRequestId()) > 128 || command.GetAction() == orchv1alpha1.VolumeAction_VOLUME_ACTION_UNSPECIFIED ||
			!validVolumeName(command.GetApplication()) || !validVolumeName(command.GetVolume()) || r.volumes == nil {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid volume command"))
		}
		operationContext, cancel := context.WithCancel(ctx)
		operation, added := operations.add(command.GetRequestId(), cancel)
		if !added {
			cancel()
			return connect.NewError(connect.CodeAlreadyExists, errors.New("operation request id is already active"))
		}
		go func() {
			defer operations.finish(command.GetRequestId(), operation)
			r.runVolume(operationContext, command, operationOutbound)
		}()
	case *orchv1alpha1.ConnectResponse_Restart:
		command := message.Restart
		if command == nil || len(command.ProtoReflect().GetUnknown()) != 0 || command.GetRequestId() == "" ||
			len(command.GetRequestId()) > 128 || command.GetAllocationId() == "" {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid restart command"))
		}
		operationContext, cancel := context.WithCancel(ctx)
		operation, added := operations.add(command.GetRequestId(), cancel)
		if !added {
			cancel()
			return connect.NewError(connect.CodeAlreadyExists, errors.New("operation request id is already active"))
		}
		go func() {
			defer operations.finish(command.GetRequestId(), operation)
			r.runRestart(operationContext, command, operationOutbound)
		}()
	case *orchv1alpha1.ConnectResponse_Upgrade:
		return r.handleUpgrade(ctx, message.Upgrade)
	case *orchv1alpha1.ConnectResponse_CancelOperation:
		if message.CancelOperation == nil || len(message.CancelOperation.ProtoReflect().GetUnknown()) != 0 || message.CancelOperation.GetRequestId() == "" {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid operation cancellation"))
		}
		operations.cancel(message.CancelOperation.GetRequestId())
	default:
		return connect.NewError(connect.CodeInvalidArgument, errors.New("controller message type is required"))
	}
	return nil
}

func (r *sessionRunner) enqueueReport(outbound chan<- *orchv1alpha1.ConnectRequest, assignment model.Assignment, statuses []model.AllocationStatus) error {
	r.reportSeq++
	routes, networkStatus := r.supervisor.Observations()
	tcpRoutes := r.supervisor.TCPObservations()
	nodePorts := r.supervisor.NodePortObservations()
	cronJobs, jobRuns := r.supervisor.CronObservations()
	report := &orchv1alpha1.AllocationReport{
		BootId: r.bootID, Sequence: r.reportSeq, AssignmentGeneration: assignment.Generation,
		AssignmentDigest: assignment.Digest, Allocations: make([]*orchv1alpha1.AllocationStatus, 0, len(statuses)),
		Routes:    make([]*orchv1alpha1.RouteStatus, 0, len(routes)),
		TcpRoutes: make([]*orchv1alpha1.TCPRouteStatus, 0, len(tcpRoutes)),
		CronJobs:  make([]*orchv1alpha1.CronJobStatus, 0, len(cronJobs)),
		JobRuns:   make([]*orchv1alpha1.JobRunStatus, 0, len(jobRuns)),
	}
	for _, status := range statuses {
		item := &orchv1alpha1.AllocationStatus{
			Id: status.ID, Application: status.Application, Service: status.Service, Replica: status.Replica,
			ApplicationGeneration: status.ApplicationGeneration, SpecDigest: status.SpecDigest,
			Phase: allocationPhaseProto(status.Phase), Reason: status.Reason, Message: status.Message,
			RestartCount: status.RestartCount, UpdatedAt: timestamppb.New(status.UpdatedAt),
			HealthStatus: healthStatusProto(status.HealthStatus), Address: status.Address,
			ActiveSpecDigest:            status.ActiveSpecDigest,
			ActiveApplicationGeneration: status.ActiveGeneration, ActiveRevisionId: status.ActiveRevisionID,
			DeclaredPort: status.DeclaredPort,
		}
		if status.ExitCode != nil {
			item.HasExitCode = true
			item.ExitCode = *status.ExitCode
		}
		if status.ReadyAt != nil {
			item.ReadyAt = timestamppb.New(*status.ReadyAt)
		}
		report.Allocations = append(report.Allocations, item)
	}
	for _, route := range routes {
		report.Routes = append(report.Routes, &orchv1alpha1.RouteStatus{
			Application: route.Application, Service: route.Service, Domain: route.Domain,
			Https: route.HTTPS, State: routeStateProto(route.State), CertificateState: route.CertificateState,
			UpstreamCount: route.UpstreamCount, Reason: route.Reason, Message: route.Message,
			UpdatedAt: timestamppb.New(route.UpdatedAt),
		})
	}
	for _, route := range tcpRoutes {
		report.TcpRoutes = append(report.TcpRoutes, &orchv1alpha1.TCPRouteStatus{
			Application: route.Application, Service: route.Service, PublicPort: route.PublicPort,
			TargetPort: route.TargetPort, State: routeStateProto(route.State), UpstreamCount: route.UpstreamCount,
			Reason: route.Reason, Message: route.Message, UpdatedAt: timestamppb.New(route.UpdatedAt),
		})
	}
	for _, port := range nodePorts {
		report.NodePorts = append(report.NodePorts, &orchv1alpha1.NodePortStatus{
			Application: port.Application, Service: port.Service, AllocationId: port.AllocationID,
			Protocol: port.Protocol, NodePort: port.NodePort, TargetPort: port.TargetPort,
			State: routeStateProto(port.State), Reason: port.Reason, Message: port.Message,
			UpdatedAt: timestamppb.New(port.UpdatedAt),
		})
	}
	for _, job := range cronJobs {
		item := &orchv1alpha1.CronJobStatus{Application: job.Application, Job: job.Job, SpecDigest: job.SpecDigest,
			ActiveRunId: job.ActiveRunID, LastRunId: job.LastRunID, LastState: jobRunStateProto(job.LastState),
			Reason: job.Reason, Message: job.Message, UpdatedAt: timestamppb.New(job.UpdatedAt)}
		if job.NextRunAt != nil {
			item.NextRunAt = timestamppb.New(*job.NextRunAt)
		}
		report.CronJobs = append(report.CronJobs, item)
	}
	for _, run := range jobRuns {
		item := &orchv1alpha1.JobRunStatus{Id: run.ID, Application: run.Application, Job: run.Job,
			SpecDigest: run.SpecDigest, ScheduledAt: timestamppb.New(run.ScheduledAt), State: jobRunStateProto(run.State),
			Reason: run.Reason, Message: run.Message, UpdatedAt: timestamppb.New(run.UpdatedAt)}
		if run.ExitCode != nil {
			item.HasExitCode = true
			item.ExitCode = *run.ExitCode
		}
		if run.StartedAt != nil {
			item.StartedAt = timestamppb.New(*run.StartedAt)
		}
		if run.FinishedAt != nil {
			item.FinishedAt = timestamppb.New(*run.FinishedAt)
		}
		report.JobRuns = append(report.JobRuns, item)
	}
	for _, failure := range r.volumes.failures(assignment) {
		report.VolumeBackupFailures = append(report.VolumeBackupFailures, &orchv1alpha1.VolumeBackupFailure{
			Application: failure.Application, Volume: failure.Volume, FailedAt: timestamppb.New(failure.FailedAt), Message: failure.Message,
		})
	}
	if networkStatus.Generation != 0 {
		report.Network = &orchv1alpha1.NetworkStatus{
			Generation: networkStatus.Generation, State: networkStateProto(networkStatus.State),
			ClusterCidr: networkStatus.ClusterCIDR, NodeSubnet: networkStatus.NodeSubnet,
			Gateway: networkStatus.Gateway, Reason: networkStatus.Reason, Message: networkStatus.Message,
			DnsState: networkStatus.DNSState, IngressState: networkStatus.IngressState,
			DrainingObjects: networkStatus.DrainingObjects,
		}
	}
	return enqueueAgentRequest(outbound, &orchv1alpha1.ConnectRequest{Message: &orchv1alpha1.ConnectRequest_AllocationReport{AllocationReport: report}})
}

func (r *sessionRunner) runTailLogs(ctx context.Context, command *orchv1alpha1.TailLogsCommand, outbound chan<- *orchv1alpha1.ConnectRequest) {
	allocation, err := r.cachedAllocation(ctx, command.GetAllocationId())
	if err != nil {
		enqueueOperationError(ctx, outbound, command.GetRequestId(), "not_found", "allocation is not in the cached assignment")
		return
	}
	readLogs, unavailable := r.supervisor.ReadLogs, "running allocation logs are unavailable"
	if command.GetPrevious() {
		readLogs, unavailable = r.supervisor.ReadPreviousLogs, "no earlier instance of this replica left logs on its node in the last 24 hours"
	}
	batch, err := readLogs(ctx, allocation, command.GetTailLines(), command.GetCursor())
	if err != nil {
		enqueueOperationError(ctx, outbound, command.GetRequestId(), "not_found", unavailable)
		return
	}
	var finalOffset uint64
	for _, frame := range batch.Frames {
		if frame.Offset > finalOffset {
			finalOffset = frame.Offset
		}
		for len(frame.Data) > 0 {
			size := min(len(frame.Data), maxOperationChunk)
			chunk := append([]byte(nil), frame.Data[:size]...)
			frame.Data = frame.Data[size:]
			response := &orchv1alpha1.TailLogsResponse{
				RequestId: command.GetRequestId(), Stream: logStreamProto(frame.Stream), Data: chunk,
				Offset: frame.Offset, Truncated: frame.Truncated,
			}
			select {
			case <-ctx.Done():
				return
			case outbound <- operationResult(&orchv1alpha1.AgentOperationResult_Log{Log: response}):
			}
		}
	}
	select {
	case <-ctx.Done():
	case outbound <- operationResult(&orchv1alpha1.AgentOperationResult_Log{Log: &orchv1alpha1.TailLogsResponse{RequestId: command.GetRequestId(), Offset: finalOffset, Complete: true, NextCursor: batch.NextCursor, CursorReset: batch.CursorReset, RuntimeExited: batch.Exited}}):
	}
}

func (r *sessionRunner) runTailCronLogs(ctx context.Context, command *orchv1alpha1.TailCronLogsCommand, outbound chan<- *orchv1alpha1.ConnectRequest) {
	batch, err := r.supervisor.ReadCronLogs(ctx, command.GetRunId(), command.GetTailLines(), command.GetCursor())
	if err != nil {
		enqueueOperationError(ctx, outbound, command.GetRequestId(), "not_found", "cron run logs are unavailable")
		return
	}
	var finalOffset uint64
	for _, frame := range batch.Frames {
		if frame.Offset > finalOffset {
			finalOffset = frame.Offset
		}
		for len(frame.Data) > 0 {
			size := min(len(frame.Data), maxOperationChunk)
			chunk := append([]byte(nil), frame.Data[:size]...)
			frame.Data = frame.Data[size:]
			select {
			case <-ctx.Done():
				return
			case outbound <- operationResult(&orchv1alpha1.AgentOperationResult_Log{Log: &orchv1alpha1.TailLogsResponse{
				RequestId: command.GetRequestId(), Stream: logStreamProto(frame.Stream), Data: chunk,
				Offset: frame.Offset, Truncated: frame.Truncated,
			}}):
			}
		}
	}
	select {
	case <-ctx.Done():
	case outbound <- operationResult(&orchv1alpha1.AgentOperationResult_Log{Log: &orchv1alpha1.TailLogsResponse{RequestId: command.GetRequestId(), Offset: finalOffset, Complete: true, NextCursor: batch.NextCursor, CursorReset: batch.CursorReset, RuntimeExited: batch.Exited}}):
	}
}

func (r *sessionRunner) runExec(ctx, sessionContext context.Context, command *orchv1alpha1.ExecCommand, outbound chan<- *orchv1alpha1.ConnectRequest) {
	allocation, err := r.cachedAllocation(ctx, command.GetAllocationId())
	if err != nil {
		enqueueOperationError(ctx, outbound, command.GetRequestId(), "not_found", "allocation is not in the cached assignment")
		return
	}
	frames, err := r.supervisor.Exec(ctx, allocation, command.GetCommand())
	if err != nil {
		enqueueOperationError(ctx, outbound, command.GetRequestId(), "not_found", "running allocation is unavailable")
		return
	}
	terminal := false
	for frame := range frames {
		data := frame.Data
		if len(data) == 0 {
			data = nil
		}
		for len(data) > 0 || (frame.Exited && !terminal) {
			size := min(len(data), maxOperationChunk)
			chunk := append([]byte(nil), data[:size]...)
			data = data[size:]
			exited := frame.Exited && len(data) == 0
			response := &orchv1alpha1.ExecResponse{
				RequestId: command.GetRequestId(), Stream: logStreamProto(frame.Stream), Data: chunk,
				Exited: exited, ExitCode: frame.ExitCode,
			}
			select {
			case <-ctx.Done():
				return
			case outbound <- operationResult(&orchv1alpha1.AgentOperationResult_Exec{Exec: response}):
			}
			if exited {
				terminal = true
			}
			if len(data) == 0 {
				break
			}
		}
	}
	if !terminal && ctx.Err() != nil {
		enqueueOperationError(sessionContext, outbound, command.GetRequestId(), "timeout", "exec command timed out")
	}
}

func receiveControllerMessages(ctx context.Context, stream *connect.BidiStreamForClient[orchv1alpha1.ConnectRequest, orchv1alpha1.ConnectResponse], output chan<- controllerMessage) {
	for {
		message, err := stream.Receive()
		select {
		case <-ctx.Done():
			return
		case output <- controllerMessage{message: message, err: err}:
		}
		if err != nil {
			return
		}
	}
}

// enqueueAck acknowledges an assignment. Call it only after the durable
// assignment has actually converged: that makes a final empty-assignment ACK a
// reliable removal fence.
func (r *sessionRunner) enqueueAck(outbound chan<- *orchv1alpha1.ConnectRequest, assignment model.Assignment) error {
	if err := enqueueAgentRequest(outbound, &orchv1alpha1.ConnectRequest{Message: &orchv1alpha1.ConnectRequest_AssignmentAck{AssignmentAck: &orchv1alpha1.AssignmentAck{
		BootId: r.bootID, Generation: assignment.Generation, Digest: assignment.Digest,
	}}}); err != nil {
		return err
	}
	r.ackPending = false
	return nil
}

func enqueueAgentRequest(outbound chan<- *orchv1alpha1.ConnectRequest, request *orchv1alpha1.ConnectRequest) error {
	select {
	case outbound <- request:
		return nil
	default:
		return connect.NewError(connect.CodeResourceExhausted, errors.New("agent outbound queue is full"))
	}
}

func rejectAssignmentUnknownFields(snapshot *orchv1alpha1.AssignmentSnapshot) error {
	if snapshot == nil || len(snapshot.ProtoReflect().GetUnknown()) != 0 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("assignment contains unknown fields"))
	}
	for _, allocation := range snapshot.GetAllocations() {
		if allocation == nil || len(allocation.ProtoReflect().GetUnknown()) != 0 {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("assignment allocation contains unknown fields"))
		}
		for _, mount := range allocation.GetMounts() {
			if mount == nil || len(mount.ProtoReflect().GetUnknown()) != 0 {
				return connect.NewError(connect.CodeInvalidArgument, errors.New("assignment volume mount contains unknown fields"))
			}
		}
		for _, backup := range allocation.GetVolumeBackups() {
			if backup == nil || len(backup.ProtoReflect().GetUnknown()) != 0 {
				return connect.NewError(connect.CodeInvalidArgument, errors.New("assignment volume backup contains unknown fields"))
			}
		}
		if err := rejectSecretReferences(allocation.GetSecretEnvironment(), allocation.GetSecretFiles(), allocation.GetRegistryCredential()); err != nil {
			return err
		}
		if allocation.GetHttp() != nil && len(allocation.GetHttp().ProtoReflect().GetUnknown()) != 0 {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("assignment HTTP intent contains unknown fields"))
		}
		if allocation.GetTcp() != nil && len(allocation.GetTcp().ProtoReflect().GetUnknown()) != 0 {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("assignment TCP intent contains unknown fields"))
		}
		if allocation.GetRollback() != nil {
			if len(allocation.GetRollback().ProtoReflect().GetUnknown()) != 0 {
				return connect.NewError(connect.CodeInvalidArgument, errors.New("assignment rollback spec contains unknown fields"))
			}
			if allocation.GetRollback().GetHttp() != nil && len(allocation.GetRollback().GetHttp().ProtoReflect().GetUnknown()) != 0 {
				return connect.NewError(connect.CodeInvalidArgument, errors.New("assignment rollback HTTP intent contains unknown fields"))
			}
			if allocation.GetRollback().GetTcp() != nil && len(allocation.GetRollback().GetTcp().ProtoReflect().GetUnknown()) != 0 {
				return connect.NewError(connect.CodeInvalidArgument, errors.New("assignment rollback TCP intent contains unknown fields"))
			}
			for _, mount := range allocation.GetRollback().GetMounts() {
				if mount == nil || len(mount.ProtoReflect().GetUnknown()) != 0 {
					return connect.NewError(connect.CodeInvalidArgument, errors.New("assignment rollback mount contains unknown fields"))
				}
			}
			if err := rejectSecretReferences(allocation.GetRollback().GetSecretEnvironment(), allocation.GetRollback().GetSecretFiles(), allocation.GetRollback().GetRegistryCredential()); err != nil {
				return err
			}
		}
	}
	for _, job := range snapshot.GetCronJobs() {
		if job == nil || len(job.ProtoReflect().GetUnknown()) != 0 {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("assignment cron job contains unknown fields"))
		}
		if err := rejectSecretReferences(job.GetSecretEnvironment(), job.GetSecretFiles(), job.GetRegistryCredential()); err != nil {
			return err
		}
	}
	for _, material := range snapshot.GetSecretMaterials() {
		if material == nil || len(material.ProtoReflect().GetUnknown()) != 0 {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("assignment secret material contains unknown fields"))
		}
	}
	if snapshot.GetNetwork() != nil && len(snapshot.GetNetwork().ProtoReflect().GetUnknown()) != 0 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("assignment network contains unknown fields"))
	}
	return nil
}

func rejectSecretReferences(environment map[string]*orchv1alpha1.SecretReference, files []*orchv1alpha1.SecretFileDesired, registry *orchv1alpha1.RegistryCredentialReference) error {
	for _, reference := range environment {
		if reference == nil || len(reference.ProtoReflect().GetUnknown()) != 0 {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("assignment secret reference contains unknown fields"))
		}
	}
	for _, file := range files {
		if file == nil || len(file.ProtoReflect().GetUnknown()) != 0 || file.GetSecret() == nil || len(file.GetSecret().ProtoReflect().GetUnknown()) != 0 {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("assignment secret file contains unknown fields"))
		}
	}
	if registry != nil && len(registry.ProtoReflect().GetUnknown()) != 0 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("assignment registry credential reference contains unknown fields"))
	}
	return nil
}

func (r *sessionRunner) cachedAllocation(ctx context.Context, id string) (model.AllocationDesired, error) {
	assignment, err := r.cache.Assignment(ctx)
	if err != nil {
		return model.AllocationDesired{}, err
	}
	for _, allocation := range assignment.Allocations {
		if allocation.ID == id {
			return allocation, nil
		}
	}
	return model.AllocationDesired{}, orchruntime.ErrNotFound
}

func assignmentModel(snapshot *orchv1alpha1.AssignmentSnapshot, nodeID string) (model.Assignment, error) {
	if snapshot == nil || snapshot.GetNodeId() != nodeID || snapshot.GetGeneration() == 0 || snapshot.GetDigest() == "" ||
		snapshot.GetCreatedAt() == nil || snapshot.GetCreatedAt().CheckValid() != nil {
		return model.Assignment{}, errors.New("assignment snapshot is incomplete or for another node")
	}
	if len(snapshot.GetAllocations()) > maxAssignedAllocations || len(snapshot.GetCronJobs()) > maxAssignedCronJobs {
		return model.Assignment{}, fmt.Errorf("assignment exceeds the %d allocation limit", maxAssignedAllocations)
	}
	assignment := model.Assignment{
		NodeID: nodeID, Generation: snapshot.GetGeneration(), Digest: snapshot.GetDigest(),
		CreatedAt: snapshot.GetCreatedAt().AsTime(), Allocations: make([]model.AllocationDesired, 0, len(snapshot.GetAllocations())),
		CronJobs: make([]model.CronJobDesired, 0, len(snapshot.GetCronJobs())),
	}
	if network := snapshot.GetNetwork(); network != nil {
		assignment.Network = model.NetworkConfig{Generation: network.GetGeneration(), ClusterCIDR: network.GetClusterCidr(),
			NodeSubnet: network.GetNodeSubnet(), Gateway: network.GetGateway(), DNSZone: network.GetDnsZone(),
			IngressEnabled: network.GetIngressEnabled(), WireGuardListenPort: uint16(network.GetWireguardListenPort())}
		for _, peer := range network.GetPeers() {
			if peer == nil || len(peer.ProtoReflect().GetUnknown()) != 0 {
				return model.Assignment{}, errors.New("assignment WireGuard peer contains unknown fields")
			}
			assignment.Network.Peers = append(assignment.Network.Peers, model.WireGuardPeer{
				NodeID: peer.GetNodeId(), PublicKey: peer.GetPublicKey(), Endpoint: peer.GetEndpoint(), AllowedSubnet: peer.GetAllowedSubnet(),
			})
		}
		for _, endpoint := range network.GetEndpoints() {
			if endpoint == nil || len(endpoint.ProtoReflect().GetUnknown()) != 0 {
				return model.Assignment{}, errors.New("assignment service endpoint contains unknown fields")
			}
			assignment.Network.Endpoints = append(assignment.Network.Endpoints, model.ServiceEndpoint{
				AllocationID: endpoint.GetAllocationId(), Application: endpoint.GetApplication(), Service: endpoint.GetService(),
				Address: endpoint.GetAddress(), Port: endpoint.GetPort(), NodeID: endpoint.GetNodeId(),
				Domains: append([]string(nil), endpoint.GetDomains()...), HTTPS: endpoint.GetHttps(), HTTPPublic: endpoint.GetHttpPublic(),
				TCPPublicPort: endpoint.GetTcpPublicPort(), HTTPNodeLocal: endpoint.GetHttpNodeLocal(),
			})
		}
		if err := model.ValidateNodeNetwork(assignment.Network); err != nil {
			return model.Assignment{}, errors.New("assignment network is invalid")
		}
		if len(assignment.Network.Peers) != 0 {
			configuration := model.WireGuardConfig{Generation: assignment.Network.Generation,
				ClusterCIDR: assignment.Network.ClusterCIDR, NodeSubnet: assignment.Network.NodeSubnet,
				ListenPort: assignment.Network.WireGuardListenPort, Peers: assignment.Network.Peers}
			if err := model.ValidateWireGuardConfig(configuration); err != nil {
				return model.Assignment{}, fmt.Errorf("assignment WireGuard configuration is invalid: %w", err)
			}
		}
		if err := model.ValidateServiceEndpoints(assignment.Network); err != nil {
			return model.Assignment{}, fmt.Errorf("assignment service endpoint catalog is invalid: %w", err)
		}
	}
	previousID := ""
	for _, item := range snapshot.GetAllocations() {
		if item == nil || item.GetId() == "" || item.GetApplication() == "" || item.GetService() == "" || item.GetReplica() == 0 || item.GetSpecDigest() == "" {
			return model.Assignment{}, errors.New("assignment contains an incomplete allocation")
		}
		if previousID != "" && item.GetId() <= previousID {
			return model.Assignment{}, errors.New("assignment allocations are not in strict canonical order")
		}
		previousID = item.GetId()
		allocation := model.AllocationDesired{
			ID: item.GetId(), Application: item.GetApplication(), Service: item.GetService(), Replica: item.GetReplica(),
			ApplicationGeneration: item.GetApplicationGeneration(), RevisionID: item.GetRevisionId(),
			SpecDigest: item.GetSpecDigest(), Image: item.GetImage(), Command: append([]string(nil), item.GetCommand()...),
			Args: append([]string(nil), item.GetArgs()...), Environment: cloneStringMap(item.GetEnvironment()),
			RestartPolicy: item.GetRestartPolicy(), CPUMillis: item.GetCpuMillis(), MemoryBytes: item.GetMemoryBytes(),
			StartDeadlineSeconds: item.GetStartDeadlineSeconds(), AutoRollback: item.GetAutoRollback(), StartBeforeStop: item.GetStartBeforeStop(),
			NetworkGeneration: item.GetNetworkGeneration(), MaxSurge: item.GetMaxSurge(), MaxUnavailable: item.GetMaxUnavailable(),
			DependsOn: append([]string(nil), item.GetDependsOn()...), StopGraceSeconds: item.GetStopGraceSeconds(), Privileged: item.GetPrivileged(),
		}
		allocation.SecretEnvironment = secretEnvironmentModel(item.GetSecretEnvironment())
		allocation.SecretFiles = secretFilesModel(item.GetSecretFiles())
		allocation.RegistryCredential = registryCredentialModel(item.GetRegistryCredential())
		for _, backup := range item.GetVolumeBackups() {
			var before []string
			if len(backup.GetBefore()) != 0 {
				before = append(before, backup.GetBefore()...)
			}
			allocation.VolumeBackups = append(allocation.VolumeBackups, model.VolumeBackupDesired{
				Volume: backup.GetVolume(), Schedule: backup.GetSchedule(), Keep: backup.GetKeep(), Before: before,
			})
		}
		if desiredHTTP := item.GetHttp(); desiredHTTP != nil {
			allocation.HTTP = &model.HTTPDesired{Port: desiredHTTP.GetPort(), Public: desiredHTTP.GetPublic(), Domains: append([]string(nil), desiredHTTP.GetDomains()...), HealthPath: desiredHTTP.GetHealthPath(), HTTPS: desiredHTTP.GetHttps(), NodeLocal: desiredHTTP.GetNodeLocal()}
		}
		if desiredTCP := item.GetTcp(); desiredTCP != nil {
			allocation.TCP = &model.TCPDesired{Port: desiredTCP.GetPort(), PublicPort: desiredTCP.GetPublicPort(), NodePort: desiredTCP.GetNodePort()}
		}
		if desiredUDP := item.GetUdp(); desiredUDP != nil {
			allocation.UDP = &model.UDPDesired{Port: desiredUDP.GetPort(), NodePort: desiredUDP.GetNodePort()}
		}
		for _, mount := range item.GetMounts() {
			allocation.Mounts = append(allocation.Mounts, model.VolumeMount{Name: mount.GetName(), Target: mount.GetTarget(), ReadOnly: mount.GetReadOnly()})
		}
		if rollback := item.GetRollback(); rollback != nil {
			allocation.Rollback = &model.RollbackSpec{
				ApplicationGeneration: rollback.GetApplicationGeneration(), RevisionID: rollback.GetRevisionId(),
				SpecDigest: rollback.GetSpecDigest(), Image: rollback.GetImage(), Command: append([]string(nil), rollback.GetCommand()...),
				Args: append([]string(nil), rollback.GetArgs()...), Environment: cloneStringMap(rollback.GetEnvironment()),
				RestartPolicy: rollback.GetRestartPolicy(), CPUMillis: rollback.GetCpuMillis(), MemoryBytes: rollback.GetMemoryBytes(),
				StartDeadlineSeconds: rollback.GetStartDeadlineSeconds(), NetworkGeneration: rollback.GetNetworkGeneration(),
				AutoRollback: rollback.GetAutoRollback(), StartBeforeStop: rollback.GetStartBeforeStop(),
				DependsOn: append([]string(nil), rollback.GetDependsOn()...), StopGraceSeconds: rollback.GetStopGraceSeconds(), Privileged: rollback.GetPrivileged(),
			}
			allocation.Rollback.SecretEnvironment = secretEnvironmentModel(rollback.GetSecretEnvironment())
			allocation.Rollback.SecretFiles = secretFilesModel(rollback.GetSecretFiles())
			allocation.Rollback.RegistryCredential = registryCredentialModel(rollback.GetRegistryCredential())
			if rollback.GetHttp() != nil {
				allocation.Rollback.HTTP = &model.HTTPDesired{Port: rollback.GetHttp().GetPort(), Public: rollback.GetHttp().GetPublic(), Domains: append([]string(nil), rollback.GetHttp().GetDomains()...), HealthPath: rollback.GetHttp().GetHealthPath(), HTTPS: rollback.GetHttp().GetHttps(), NodeLocal: rollback.GetHttp().GetNodeLocal()}
			}
			if rollback.GetTcp() != nil {
				allocation.Rollback.TCP = &model.TCPDesired{Port: rollback.GetTcp().GetPort(), PublicPort: rollback.GetTcp().GetPublicPort(), NodePort: rollback.GetTcp().GetNodePort()}
			}
			if rollback.GetUdp() != nil {
				allocation.Rollback.UDP = &model.UDPDesired{Port: rollback.GetUdp().GetPort(), NodePort: rollback.GetUdp().GetNodePort()}
			}
			for _, mount := range rollback.GetMounts() {
				allocation.Rollback.Mounts = append(allocation.Rollback.Mounts, model.VolumeMount{Name: mount.GetName(), Target: mount.GetTarget(), ReadOnly: mount.GetReadOnly()})
			}
		}
		if err := orchruntime.ValidateAllocation(allocation); err != nil {
			return model.Assignment{}, fmt.Errorf("assignment allocation %q is invalid: %w", allocation.ID, err)
		}
		legacy, err := model.IsPlan4Allocation(allocation)
		if err != nil {
			return model.Assignment{}, fmt.Errorf("validate legacy allocation %q: %w", allocation.ID, err)
		}
		legacyPreflight := legacy && allocation.NetworkGeneration == 0 && assignment.Network.Generation != 0
		if allocation.NetworkGeneration != assignment.Network.Generation && !legacyPreflight {
			return model.Assignment{}, fmt.Errorf("assignment allocation %q does not match the assignment network generation", allocation.ID)
		}
		assignment.Allocations = append(assignment.Allocations, allocation)
	}
	previousJob := ""
	for _, item := range snapshot.GetCronJobs() {
		key := item.GetApplication() + "\x00" + item.GetJob()
		if item.GetApplication() == "" || item.GetJob() == "" || item.GetApplicationGeneration() == 0 ||
			item.GetRevisionId() == "" || item.GetSpecDigest() == "" || (previousJob != "" && key <= previousJob) {
			return model.Assignment{}, errors.New("assignment contains an incomplete or unordered cron job")
		}
		job := model.CronJobDesired{Application: item.GetApplication(), Job: item.GetJob(),
			ApplicationGeneration: item.GetApplicationGeneration(), RevisionID: item.GetRevisionId(),
			SpecDigest: item.GetSpecDigest(), Image: item.GetImage(), Schedule: item.GetSchedule(),
			Command: append([]string(nil), item.GetCommand()...), Args: append([]string(nil), item.GetArgs()...),
			Environment: cloneStringMap(item.GetEnvironment()), CPUMillis: item.GetCpuMillis(), MemoryBytes: item.GetMemoryBytes(),
			DependsOn: append([]string(nil), item.GetDependsOn()...), StartDeadlineSeconds: item.GetStartDeadlineSeconds(),
			TimeoutSeconds: item.GetTimeoutSeconds(), NetworkGeneration: item.GetNetworkGeneration(),
			SecretEnvironment: secretEnvironmentModel(item.GetSecretEnvironment()),
			SecretFiles:       secretFilesModel(item.GetSecretFiles()), RegistryCredential: registryCredentialModel(item.GetRegistryCredential())}
		job.RunID, job.CancelRequested = item.GetRunId(), item.GetCancelRequested()
		for _, mount := range item.GetMounts() {
			job.Mounts = append(job.Mounts, model.VolumeMount{Name: mount.GetName(), Target: mount.GetTarget(), ReadOnly: mount.GetReadOnly()})
		}
		if item.GetRunAt() != nil {
			if item.GetRunAt().CheckValid() != nil || len(item.GetRunAt().ProtoReflect().GetUnknown()) != 0 {
				return model.Assignment{}, errors.New("invalid one-off run time")
			}
			at := item.GetRunAt().AsTime()
			job.RunAt = &at
		}
		if err := orchruntime.ValidateCronJob(job); err != nil || job.NetworkGeneration != assignment.Network.Generation {
			return model.Assignment{}, fmt.Errorf("assignment cron job %q is invalid", key)
		}
		assignment.CronJobs = append(assignment.CronJobs, job)
		previousJob = key
	}
	seenMaterials := make(map[string]struct{}, len(snapshot.GetSecretMaterials()))
	totalMaterialBytes := 0
	for _, item := range snapshot.GetSecretMaterials() {
		if item == nil || item.GetVersionId() == "" || len(item.GetValue()) == 0 || len(item.GetValue()) > 1<<20 {
			return model.Assignment{}, errors.New("assignment contains invalid secret material")
		}
		totalMaterialBytes += len(item.GetValue())
		if totalMaterialBytes > maxSecretBundleBytes {
			return model.Assignment{}, errors.New("assignment secret material bundle is too large")
		}
		material := model.SecretMaterial{Kind: item.GetKind(), Application: item.GetApplication(), Name: item.GetName(),
			VersionID: item.GetVersionId(), Host: item.GetHost(), Username: item.GetUsername(), Value: append([]byte(nil), item.GetValue()...)}
		key := material.Kind + "\x00" + material.Application + "\x00" + material.Name + "\x00" + material.Host + "\x00" + material.VersionID
		if _, exists := seenMaterials[key]; exists ||
			(material.Kind == model.SecretMaterialApplication && (material.Application == "" || material.Name == "" || material.Host != "")) ||
			(material.Kind == model.SecretMaterialRegistry && (material.Host == "" || material.Application != "" || material.Name != "")) ||
			(material.Kind != model.SecretMaterialApplication && material.Kind != model.SecretMaterialRegistry) {
			return model.Assignment{}, errors.New("assignment contains invalid or duplicate secret material")
		}
		seenMaterials[key] = struct{}{}
		assignment.Materials = append(assignment.Materials, material)
	}
	return assignment, nil
}

func secretEnvironmentModel(input map[string]*orchv1alpha1.SecretReference) map[string]model.SecretReference {
	if len(input) == 0 {
		return nil
	}
	result := make(map[string]model.SecretReference, len(input))
	for key, item := range input {
		if item != nil {
			result[key] = model.SecretReference{Name: item.GetName(), VersionID: item.GetVersionId()}
		}
	}
	return result
}

func secretFilesModel(input []*orchv1alpha1.SecretFileDesired) []model.SecretFileDesired {
	result := make([]model.SecretFileDesired, 0, len(input))
	for _, item := range input {
		if item != nil && item.GetSecret() != nil {
			result = append(result, model.SecretFileDesired{SecretReference: model.SecretReference{
				Name: item.GetSecret().GetName(), VersionID: item.GetSecret().GetVersionId()}, Target: item.GetTarget()})
		}
	}
	return result
}

func registryCredentialModel(input *orchv1alpha1.RegistryCredentialReference) *model.RegistryCredentialReference {
	if input == nil {
		return nil
	}
	return &model.RegistryCredentialReference{Host: input.GetHost(), VersionID: input.GetVersionId(), Username: input.GetUsername()}
}

func jobRunStateProto(value string) orchv1alpha1.JobRunState {
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

func operationResult(result any) *orchv1alpha1.ConnectRequest {
	operation := &orchv1alpha1.AgentOperationResult{}
	switch value := result.(type) {
	case *orchv1alpha1.AgentOperationResult_Log:
		operation.Result = value
	case *orchv1alpha1.AgentOperationResult_Exec:
		operation.Result = value
	case *orchv1alpha1.AgentOperationResult_Error:
		operation.Result = value
	case *orchv1alpha1.AgentOperationResult_Volume:
		operation.Result = value
	case *orchv1alpha1.AgentOperationResult_Restart:
		operation.Result = value
	default:
		panic("unsupported agent operation result")
	}
	return &orchv1alpha1.ConnectRequest{Message: &orchv1alpha1.ConnectRequest_OperationResult{OperationResult: operation}}
}

func enqueueOperationError(ctx context.Context, outbound chan<- *orchv1alpha1.ConnectRequest, requestID, code, message string) {
	select {
	case <-ctx.Done():
	case outbound <- operationResult(&orchv1alpha1.AgentOperationResult_Error{Error: &orchv1alpha1.OperationError{RequestId: requestID, Code: code, Message: message}}):
	}
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func allocationPhaseProto(phase string) orchv1alpha1.AllocationPhase {
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

func healthStatusProto(status string) orchv1alpha1.HealthStatus {
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

func networkStateProto(status string) orchv1alpha1.NetworkState {
	switch status {
	case model.NetworkPending:
		return orchv1alpha1.NetworkState_NETWORK_STATE_PENDING
	case model.NetworkReady:
		return orchv1alpha1.NetworkState_NETWORK_STATE_READY
	case model.NetworkFailed:
		return orchv1alpha1.NetworkState_NETWORK_STATE_FAILED
	default:
		return orchv1alpha1.NetworkState_NETWORK_STATE_UNSPECIFIED
	}
}

func routeStateProto(status string) orchv1alpha1.RouteState {
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

func logStreamProto(stream string) orchv1alpha1.LogStream {
	if stream == "stderr" {
		return orchv1alpha1.LogStream_LOG_STREAM_STDERR
	}
	return orchv1alpha1.LogStream_LOG_STREAM_STDOUT
}
