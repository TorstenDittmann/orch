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
	agentProtocolVersion  = "v1alpha1-network-1"
	agentHeartbeatTimeout = 45 * time.Second
	operationFrameBytes   = 64 << 10
)

var errAgentDisconnected = errors.New("agent is not connected")

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
		return fmt.Errorf("agent outbound queue is full")
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
	frames := make(chan *orchv1alpha1.AgentOperationResult, 32)
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return "", nil, nil, errAgentDisconnected
	}
	session.operations[requestID] = frames
	session.mu.Unlock()
	if err := enqueueSession(session, build(requestID)); err != nil {
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
				_ = enqueueSession(session, &orchv1alpha1.ConnectResponse{
					Message: &orchv1alpha1.ConnectResponse_CancelOperation{CancelOperation: &orchv1alpha1.CancelOperation{RequestId: requestID}},
				})
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
	defer session.mu.Unlock()
	frames := session.operations[requestID]
	if frames == nil {
		return nil
	}
	select {
	case frames <- result:
	default:
		return fmt.Errorf("operation response queue is full")
	}
	if terminal {
		delete(session.operations, requestID)
		close(frames)
	}
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
	}
	if assignment.Network.Generation != 0 {
		message.Network = &orchv1alpha1.NetworkConfig{
			Generation: assignment.Network.Generation, ClusterCidr: assignment.Network.ClusterCIDR,
			NodeSubnet: assignment.Network.NodeSubnet, Gateway: assignment.Network.Gateway,
			DnsZone: assignment.Network.DNSZone,
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
		item.Rollback = rollbackSpecMessage(allocation.Rollback)
		for _, mount := range allocation.Mounts {
			item.Mounts = append(item.Mounts, &orchv1alpha1.VolumeMount{Name: mount.Name, Target: mount.Target, ReadOnly: mount.ReadOnly})
		}
		message.Allocations = append(message.Allocations, item)
	}
	return message
}

func httpDesiredMessage(http *model.HTTPDesired) *orchv1alpha1.HTTPDesired {
	if http == nil {
		return nil
	}
	return &orchv1alpha1.HTTPDesired{Port: http.Port, Domains: append([]string(nil), http.Domains...), HealthPath: http.HealthPath, Https: http.HTTPS}
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
		NetworkGeneration: rollback.NetworkGeneration, AutoRollback: rollback.AutoRollback,
		StartBeforeStop: rollback.StartBeforeStop,
	}
	for _, mount := range rollback.Mounts {
		message.Mounts = append(message.Mounts, &orchv1alpha1.VolumeMount{Name: mount.Name, Target: mount.Target, ReadOnly: mount.ReadOnly})
	}
	return message
}
