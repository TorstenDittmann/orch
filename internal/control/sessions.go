package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	"change.codes/torsten/better-swarm/internal/model"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	agentProtocolVersion  = "v1alpha1-workloads-5"
	agentHeartbeatTimeout = 45 * time.Second
	operationFrameBytes   = 64 << 10
	operationFrameBuffer  = 32
)

var (
	errAgentDisconnected = errors.New("agent is not connected")
	errAgentBusy         = errors.New("agent outbound queue is full")
)

type sessionManager struct {
	mu       sync.Mutex
	randomMu sync.Mutex
	sessions map[string]*agentSession
	now      func() time.Time
	random   io.Reader
	timeout  time.Duration
}

type agentSession struct {
	mu                       sync.Mutex
	nodeID                   string
	bootID                   string
	send                     chan *orchv1alpha1.ConnectResponse
	cancel                   context.CancelFunc
	lastSeen                 time.Time
	lastAssignmentGeneration uint64
	operations               map[string]chan *orchv1alpha1.AgentOperationResult
	closed                   bool
}

func newSessionManager(now func() time.Time) *sessionManager {
	if now == nil {
		now = time.Now
	}
	return &sessionManager{
		sessions: make(map[string]*agentSession), now: now, random: rand.Reader,
		timeout: agentHeartbeatTimeout,
	}
}

func (m *sessionManager) register(ctx context.Context, nodeID, bootID string, cancel context.CancelFunc) (*agentSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.sessions[nodeID]; current != nil {
		current.mu.Lock()
		stale := m.now().Sub(current.lastSeen) >= m.timeout
		current.mu.Unlock()
		if !stale {
			return nil, fmt.Errorf("an agent session for node %q is already connected", nodeID)
		}
		current.cancel()
		m.disconnectLocked(current)
	}
	session := &agentSession{
		nodeID: nodeID, bootID: bootID, send: make(chan *orchv1alpha1.ConnectResponse, 64),
		cancel: cancel, lastSeen: m.now(), operations: make(map[string]chan *orchv1alpha1.AgentOperationResult),
	}
	m.sessions[nodeID] = session
	return session, nil
}

func (m *sessionManager) unregister(session *agentSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[session.nodeID] != session {
		return
	}
	delete(m.sessions, session.nodeID)
	m.disconnectLocked(session)
}

func (m *sessionManager) Disconnect(nodeID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[nodeID]
	if session == nil {
		return
	}
	delete(m.sessions, nodeID)
	session.cancel()
	m.disconnectLocked(session)
}

func (m *sessionManager) disconnectLocked(session *agentSession) {
	session.mu.Lock()
	defer session.mu.Unlock()
	session.closed = true
	for id, frames := range session.operations {
		delete(session.operations, id)
		close(frames)
	}
}

func (m *sessionManager) Connected(nodeID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[nodeID]
	if session == nil {
		return false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return m.now().Sub(session.lastSeen) < m.timeout
}

func (m *sessionManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for nodeID, session := range m.sessions {
		delete(m.sessions, nodeID)
		session.cancel()
		m.disconnectLocked(session)
	}
}

func (m *sessionManager) touch(session *agentSession) {
	session.mu.Lock()
	session.lastSeen = m.now()
	session.mu.Unlock()
}

func (m *sessionManager) PublishAssignment(assignment model.Assignment) {
	m.mu.Lock()
	session := m.sessions[assignment.NodeID]
	m.mu.Unlock()
	if session == nil {
		return
	}
	_ = enqueueAssignment(session, assignment)
}

func enqueueAssignment(session *agentSession, assignment model.Assignment) error {
	session.mu.Lock()
	if assignment.Generation <= session.lastAssignmentGeneration {
		session.mu.Unlock()
		return nil
	}
	session.lastAssignmentGeneration = assignment.Generation
	session.mu.Unlock()
	return enqueueSession(session, &orchv1alpha1.ConnectResponse{
		Message: &orchv1alpha1.ConnectResponse_Assignment{Assignment: assignmentSnapshotMessage(assignment)},
	})
}

func enqueueSession(session *agentSession, response *orchv1alpha1.ConnectResponse) error {
	select {
	case session.send <- response:
		return nil
	default:
		session.cancel()
		return errAgentBusy
	}
}

// tryEnqueueSession queues a best-effort message for the agent. A full queue
// drops the message instead of disconnecting the agent, so operation traffic
// can never take down the session that carries assignments.
func tryEnqueueSession(session *agentSession, response *orchv1alpha1.ConnectResponse) error {
	select {
	case session.send <- response:
		return nil
	default:
		return errAgentBusy
	}
}

func cancelOperationMessage(requestID string) *orchv1alpha1.ConnectResponse {
	return &orchv1alpha1.ConnectResponse{
		Message: &orchv1alpha1.ConnectResponse_CancelOperation{CancelOperation: &orchv1alpha1.CancelOperation{RequestId: requestID}},
	}
}

func (m *sessionManager) beginOperation(ctx context.Context, nodeID string, build func(string) *orchv1alpha1.ConnectResponse) (string, <-chan *orchv1alpha1.AgentOperationResult, func(), error) {
	m.mu.Lock()
	session := m.sessions[nodeID]
	m.mu.Unlock()
	if session == nil || !m.Connected(nodeID) {
		return "", nil, nil, errAgentDisconnected
	}
	m.randomMu.Lock()
	requestID, err := randomRequestID(m.random)
	m.randomMu.Unlock()
	if err != nil {
		return "", nil, nil, err
	}
	// One slot beyond the frame buffer is reserved for the overflow error.
	frames := make(chan *orchv1alpha1.AgentOperationResult, operationFrameBuffer+1)
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return "", nil, nil, errAgentDisconnected
	}
	session.operations[requestID] = frames
	session.mu.Unlock()
	if err := tryEnqueueSession(session, build(requestID)); err != nil {
		session.mu.Lock()
		delete(session.operations, requestID)
		close(frames)
		session.mu.Unlock()
		return "", nil, nil, err
	}
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			active := false
			session.mu.Lock()
			if current, ok := session.operations[requestID]; ok {
				delete(session.operations, requestID)
				close(current)
				active = true
			}
			session.mu.Unlock()
			if active {
				_ = tryEnqueueSession(session, cancelOperationMessage(requestID))
			}
		})
	}
	return requestID, frames, cancel, nil
}

func (m *sessionManager) routeOperation(session *agentSession, result *orchv1alpha1.AgentOperationResult) error {
	requestID, terminal, size, err := operationMetadata(result)
	if err != nil {
		return err
	}
	if size > operationFrameBytes {
		return fmt.Errorf("operation frame exceeds %d bytes", operationFrameBytes)
	}
	session.mu.Lock()
	frames := session.operations[requestID]
	if frames == nil {
		session.mu.Unlock()
		return nil
	}
	if len(frames) >= operationFrameBuffer {
		// The admin client is not keeping up. Fail only this operation: the
		// reserved slot carries the error, and the agent is asked to stop.
		frames <- &orchv1alpha1.AgentOperationResult{Result: &orchv1alpha1.AgentOperationResult_Error{Error: &orchv1alpha1.OperationError{
			RequestId: requestID, Code: "resource_exhausted", Message: "operation client is not reading responses fast enough",
		}}}
		delete(session.operations, requestID)
		close(frames)
		session.mu.Unlock()
		_ = tryEnqueueSession(session, cancelOperationMessage(requestID))
		return nil
	}
	frames <- result
	if terminal {
		delete(session.operations, requestID)
		close(frames)
	}
	session.mu.Unlock()
	return nil
}

func operationMetadata(result *orchv1alpha1.AgentOperationResult) (requestID string, terminal bool, size int, err error) {
	if result == nil || len(result.ProtoReflect().GetUnknown()) != 0 {
		return "", false, 0, errors.New("operation result is required")
	}
	switch value := result.GetResult().(type) {
	case *orchv1alpha1.AgentOperationResult_Log:
		if value.Log == nil || len(value.Log.ProtoReflect().GetUnknown()) != 0 || value.Log.GetRequestId() == "" {
			return "", false, 0, errors.New("log result is required")
		}
		return value.Log.GetRequestId(), value.Log.GetComplete(), len(value.Log.GetData()), nil
	case *orchv1alpha1.AgentOperationResult_Exec:
		if value.Exec == nil || len(value.Exec.ProtoReflect().GetUnknown()) != 0 || value.Exec.GetRequestId() == "" {
			return "", false, 0, errors.New("exec result is required")
		}
		return value.Exec.GetRequestId(), value.Exec.GetExited(), len(value.Exec.GetData()), nil
	case *orchv1alpha1.AgentOperationResult_Error:
		if value.Error == nil || len(value.Error.ProtoReflect().GetUnknown()) != 0 || value.Error.GetRequestId() == "" {
			return "", false, 0, errors.New("operation error is required")
		}
		return value.Error.GetRequestId(), true, len(value.Error.GetMessage()), nil
	default:
		return "", false, 0, errors.New("operation result type is required")
	}
}

func randomRequestID(source io.Reader) (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(source, value); err != nil {
		return "", fmt.Errorf("generate operation request id: %w", err)
	}
	return "op_" + hex.EncodeToString(value), nil
}

func assignmentSnapshotMessage(assignment model.Assignment) *orchv1alpha1.AssignmentSnapshot {
	message := &orchv1alpha1.AssignmentSnapshot{
		NodeId: assignment.NodeID, Generation: assignment.Generation, Digest: assignment.Digest,
		CreatedAt:   timestamppb.New(assignment.CreatedAt),
		Allocations: make([]*orchv1alpha1.AllocationDesired, 0, len(assignment.Allocations)),
		CronJobs:    make([]*orchv1alpha1.CronJobDesired, 0, len(assignment.CronJobs)),
	}
	if assignment.Network.Generation != 0 {
		message.Network = &orchv1alpha1.NetworkConfig{
			Generation: assignment.Network.Generation, ClusterCidr: assignment.Network.ClusterCIDR,
			NodeSubnet: assignment.Network.NodeSubnet, Gateway: assignment.Network.Gateway,
			DnsZone: assignment.Network.DNSZone, IngressEnabled: assignment.Network.IngressEnabled,
			WireguardListenPort: uint32(assignment.Network.WireGuardListenPort),
		}
		for _, peer := range assignment.Network.Peers {
			message.Network.Peers = append(message.Network.Peers, &orchv1alpha1.WireGuardPeer{
				NodeId: peer.NodeID, PublicKey: peer.PublicKey, Endpoint: peer.Endpoint, AllowedSubnet: peer.AllowedSubnet,
			})
		}
		for _, endpoint := range assignment.Network.Endpoints {
			message.Network.Endpoints = append(message.Network.Endpoints, &orchv1alpha1.ServiceEndpoint{
				AllocationId: endpoint.AllocationID, Application: endpoint.Application, Service: endpoint.Service,
				Address: endpoint.Address, Port: endpoint.Port, NodeId: endpoint.NodeID,
				Domains: append([]string(nil), endpoint.Domains...), Https: endpoint.HTTPS, HttpPublic: endpoint.HTTPPublic,
				TcpPublicPort: endpoint.TCPPublicPort,
			})
		}
	}
	for _, allocation := range assignment.Allocations {
		item := &orchv1alpha1.AllocationDesired{
			Id: allocation.ID, Application: allocation.Application, Service: allocation.Service,
			Replica: allocation.Replica, ApplicationGeneration: allocation.ApplicationGeneration,
			RevisionId: allocation.RevisionID, SpecDigest: allocation.SpecDigest, Image: allocation.Image,
			Command: append([]string(nil), allocation.Command...), Args: append([]string(nil), allocation.Args...),
			Environment: cloneEnvironment(allocation.Environment), RestartPolicy: allocation.RestartPolicy,
			CpuMillis: allocation.CPUMillis, MemoryBytes: allocation.MemoryBytes,
			StartDeadlineSeconds: allocation.StartDeadlineSeconds, AutoRollback: allocation.AutoRollback,
			StartBeforeStop: allocation.StartBeforeStop, NetworkGeneration: allocation.NetworkGeneration,
			MaxSurge: allocation.MaxSurge, MaxUnavailable: allocation.MaxUnavailable,
		}
		item.Http = httpDesiredMessage(allocation.HTTP)
		item.Tcp = tcpDesiredMessage(allocation.TCP)
		item.Udp = udpDesiredMessage(allocation.UDP)
		item.DependsOn = append([]string(nil), allocation.DependsOn...)
		item.SecretEnvironment = secretEnvironmentMessage(allocation.SecretEnvironment)
		item.SecretFiles = secretFilesMessage(allocation.SecretFiles)
		item.RegistryCredential = registryCredentialMessage(allocation.RegistryCredential)
		item.Rollback = rollbackSpecMessage(allocation.Rollback)
		for _, mount := range allocation.Mounts {
			item.Mounts = append(item.Mounts, &orchv1alpha1.VolumeMount{Name: mount.Name, Target: mount.Target, ReadOnly: mount.ReadOnly})
		}
		message.Allocations = append(message.Allocations, item)
	}
	for _, job := range assignment.CronJobs {
		item := &orchv1alpha1.CronJobDesired{
			Application: job.Application, Job: job.Job, ApplicationGeneration: job.ApplicationGeneration,
			RevisionId: job.RevisionID, SpecDigest: job.SpecDigest, Image: job.Image, Schedule: job.Schedule,
			Command: append([]string(nil), job.Command...), Args: append([]string(nil), job.Args...),
			Environment: cloneEnvironment(job.Environment), CpuMillis: job.CPUMillis, MemoryBytes: job.MemoryBytes,
			DependsOn: append([]string(nil), job.DependsOn...), StartDeadlineSeconds: job.StartDeadlineSeconds,
			TimeoutSeconds: job.TimeoutSeconds, NetworkGeneration: job.NetworkGeneration,
		}
		item.RunId, item.CancelRequested = job.RunID, job.CancelRequested
		if job.RunAt != nil {
			item.RunAt = timestamppb.New(*job.RunAt)
		}
		item.SecretEnvironment = secretEnvironmentMessage(job.SecretEnvironment)
		item.SecretFiles = secretFilesMessage(job.SecretFiles)
		item.RegistryCredential = registryCredentialMessage(job.RegistryCredential)
		for _, mount := range job.Mounts {
			item.Mounts = append(item.Mounts, &orchv1alpha1.VolumeMount{Name: mount.Name, Target: mount.Target, ReadOnly: mount.ReadOnly})
		}
		message.CronJobs = append(message.CronJobs, item)
	}
	for _, material := range assignment.Materials {
		message.SecretMaterials = append(message.SecretMaterials, &orchv1alpha1.SecretMaterial{
			Kind: material.Kind, Application: material.Application, Name: material.Name,
			VersionId: material.VersionID, Host: material.Host, Username: material.Username,
			Value: append([]byte(nil), material.Value...),
		})
	}
	return message
}

func secretEnvironmentMessage(input map[string]model.SecretReference) map[string]*orchv1alpha1.SecretReference {
	if len(input) == 0 {
		return nil
	}
	result := make(map[string]*orchv1alpha1.SecretReference, len(input))
	for name, reference := range input {
		result[name] = &orchv1alpha1.SecretReference{Name: reference.Name, VersionId: reference.VersionID}
	}
	return result
}

func secretFilesMessage(input []model.SecretFileDesired) []*orchv1alpha1.SecretFileDesired {
	result := make([]*orchv1alpha1.SecretFileDesired, 0, len(input))
	for _, file := range input {
		result = append(result, &orchv1alpha1.SecretFileDesired{
			Secret: &orchv1alpha1.SecretReference{Name: file.Name, VersionId: file.VersionID}, Target: file.Target,
		})
	}
	return result
}

func registryCredentialMessage(input *model.RegistryCredentialReference) *orchv1alpha1.RegistryCredentialReference {
	if input == nil {
		return nil
	}
	return &orchv1alpha1.RegistryCredentialReference{Host: input.Host, VersionId: input.VersionID, Username: input.Username}
}

func httpDesiredMessage(http *model.HTTPDesired) *orchv1alpha1.HTTPDesired {
	if http == nil {
		return nil
	}
	return &orchv1alpha1.HTTPDesired{Port: http.Port, Public: http.Public, Domains: append([]string(nil), http.Domains...), HealthPath: http.HealthPath, Https: http.HTTPS}
}

func tcpDesiredMessage(tcp *model.TCPDesired) *orchv1alpha1.TCPDesired {
	if tcp == nil {
		return nil
	}
	return &orchv1alpha1.TCPDesired{Port: tcp.Port, PublicPort: tcp.PublicPort, NodePort: tcp.NodePort}
}

func udpDesiredMessage(udp *model.UDPDesired) *orchv1alpha1.UDPDesired {
	if udp == nil {
		return nil
	}
	return &orchv1alpha1.UDPDesired{Port: udp.Port, NodePort: udp.NodePort}
}

func rollbackSpecMessage(rollback *model.RollbackSpec) *orchv1alpha1.RollbackSpec {
	if rollback == nil {
		return nil
	}
	message := &orchv1alpha1.RollbackSpec{
		ApplicationGeneration: rollback.ApplicationGeneration, RevisionId: rollback.RevisionID,
		SpecDigest: rollback.SpecDigest, Image: rollback.Image, Command: append([]string(nil), rollback.Command...),
		Args: append([]string(nil), rollback.Args...), Environment: cloneEnvironment(rollback.Environment),
		RestartPolicy: rollback.RestartPolicy, CpuMillis: rollback.CPUMillis, MemoryBytes: rollback.MemoryBytes,
		StartDeadlineSeconds: rollback.StartDeadlineSeconds, Http: httpDesiredMessage(rollback.HTTP),
		Tcp: tcpDesiredMessage(rollback.TCP), Udp: udpDesiredMessage(rollback.UDP), DependsOn: append([]string(nil), rollback.DependsOn...),
		NetworkGeneration: rollback.NetworkGeneration, AutoRollback: rollback.AutoRollback,
		StartBeforeStop: rollback.StartBeforeStop,
	}
	message.SecretEnvironment = secretEnvironmentMessage(rollback.SecretEnvironment)
	message.SecretFiles = secretFilesMessage(rollback.SecretFiles)
	message.RegistryCredential = registryCredentialMessage(rollback.RegistryCredential)
	for _, mount := range rollback.Mounts {
		message.Mounts = append(message.Mounts, &orchv1alpha1.VolumeMount{Name: mount.Name, Target: mount.Target, ReadOnly: mount.ReadOnly})
	}
	return message
}
