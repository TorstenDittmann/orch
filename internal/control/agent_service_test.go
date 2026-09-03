package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	"change.codes/torsten/better-swarm/gen/orch/v1alpha1/orchv1alpha1connect"
	agentctl "change.codes/torsten/better-swarm/internal/agent"
	clusterctl "change.codes/torsten/better-swarm/internal/cluster"
	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestAdministratorCannotOpenAgentStream(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dataDir := filepath.Join(root, "controller")
	contextPath := filepath.Join(root, "operator", "context.json")
	address := reserveLoopbackAddress(t)
	endpoint := "https://" + address
	if _, err := clusterctl.Bootstrap(context.Background(), clusterctl.BootstrapOptions{
		Name: "test", Server: endpoint, DataDir: dataDir,
		ContextPath: contextPath, Now: time.Now, Random: rand.Reader,
	}); err != nil {
		t.Fatal(err)
	}
	server, err := Start(context.Background(), Options{DataDir: dataDir, Listen: address})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	if _, err := agentctl.Enroll(context.Background(), agentctl.EnrollOptions{
		Name: "node-1", ContextPath: contextPath, DataDir: filepath.Join(root, "agent"),
		Now: time.Now, Random: rand.Reader,
	}); err != nil {
		t.Fatal(err)
	}
	administrator, err := clusterctl.LoadAdministratorCredentials(contextPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	httpClient := tlsHTTPClient(administrator.RootCAs, administrator.Certificate)
	defer httpClient.CloseIdleConnections()
	client := orchv1alpha1connect.NewAgentServiceClient(httpClient, endpoint)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := client.Connect(ctx)
	err = stream.Send(&orchv1alpha1.ConnectRequest{Message: &orchv1alpha1.ConnectRequest_Hello{Hello: validAgentHello("wrong")}})
	if err == nil {
		_, err = stream.Receive()
	}
	_ = stream.CloseRequest()
	_ = stream.CloseResponse()
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("administrator agent stream = %v, want permission denied", err)
	}
}

func TestValidateHelloRejectsProtocolAndShapeErrors(t *testing.T) {
	t.Parallel()
	valid := validAgentHello("node-one")
	if err := validateHello(valid, "node-one"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		edit func(*orchv1alpha1.AgentHello)
	}{
		{name: "protocol", edit: func(value *orchv1alpha1.AgentHello) { value.ProtocolVersion = "v2" }},
		{name: "node", edit: func(value *orchv1alpha1.AgentHello) { value.NodeId = "node-other" }},
		{name: "boot", edit: func(value *orchv1alpha1.AgentHello) { value.BootId = "" }},
		{name: "cached generation only", edit: func(value *orchv1alpha1.AgentHello) { value.CachedAssignmentGeneration = 1 }},
		{name: "cached digest only", edit: func(value *orchv1alpha1.AgentHello) {
			value.CachedAssignmentDigest = "sha256:" + strings.Repeat("a", 64)
		}},
		{name: "unknown field", edit: func(value *orchv1alpha1.AgentHello) { value.ProtoReflect().SetUnknown([]byte{0x50, 0x01}) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := proto.Clone(valid).(*orchv1alpha1.AgentHello)
			test.edit(candidate)
			if err := validateHello(candidate, "node-one"); err == nil {
				t.Fatal("validateHello() accepted invalid Hello")
			}
		})
	}
}

func TestSessionManagerRejectsHealthyDuplicateAndReplacesStaleSession(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	manager := newSessionManager(func() time.Time { return now })
	firstContext, cancelFirst := context.WithCancel(context.Background())
	first, err := manager.register(firstContext, "node-one", "boot-one", cancelFirst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.register(context.Background(), "node-one", "boot-two", func() {}); err == nil {
		t.Fatal("healthy duplicate session was accepted")
	}
	now = now.Add(agentHeartbeatTimeout)
	second, err := manager.register(context.Background(), "node-one", "boot-two", func() {})
	if err != nil || second == first {
		t.Fatalf("stale replacement = %#v, %v", second, err)
	}
	select {
	case <-firstContext.Done():
	default:
		t.Fatal("replaced session was not canceled")
	}
	manager.unregister(first)
	if !manager.Connected("node-one") {
		t.Fatal("old-session cleanup removed the replacement")
	}
	manager.Close()
}

func TestAllocationReportModelRejectsNonCanonicalOrder(t *testing.T) {
	t.Parallel()
	stamp := time.Now().UTC()
	report := &orchv1alpha1.AllocationReport{
		BootId: "boot-one", Sequence: 1, AssignmentGeneration: 1,
		AssignmentDigest: "sha256:" + strings.Repeat("a", 64),
		Allocations: []*orchv1alpha1.AllocationStatus{
			{Id: "alloc-two", Application: "hello", Service: "worker", Replica: 2, UpdatedAt: timestamppb.New(stamp)},
			{Id: "alloc-one", Application: "hello", Service: "worker", Replica: 1, UpdatedAt: timestamppb.New(stamp)},
		},
	}
	if _, err := allocationReportModel("node-one", report, stamp); err == nil || !strings.Contains(err.Error(), "canonical order") {
		t.Fatalf("allocationReportModel() = %v", err)
	}
	report.Allocations[1].Replica = 2
	report.Allocations[1].Id = "alloc-two"
	if _, err := allocationReportModel("node-one", report, stamp); err == nil || !strings.Contains(err.Error(), "canonical order") {
		t.Fatalf("allocationReportModel(duplicate) = %v", err)
	}
}

func TestOperationFramesAreBoundedAndTerminal(t *testing.T) {
	t.Parallel()
	manager := newSessionManager(time.Now)
	manager.random = bytes.NewReader(make([]byte, 16))
	session, err := manager.register(context.Background(), "node-one", "boot-one", func() {})
	if err != nil {
		t.Fatal(err)
	}
	requestID, frames, cancel, err := manager.beginOperation(context.Background(), "node-one", func(id string) *orchv1alpha1.ConnectResponse {
		return &orchv1alpha1.ConnectResponse{Message: &orchv1alpha1.ConnectResponse_TailLogs{TailLogs: &orchv1alpha1.TailLogsCommand{RequestId: id, AllocationId: "alloc-one", TailLines: 1}}}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if err := manager.routeOperation(session, &orchv1alpha1.AgentOperationResult{Result: &orchv1alpha1.AgentOperationResult_Log{Log: &orchv1alpha1.TailLogsResponse{
		RequestId: requestID, Complete: true,
	}}}); err != nil {
		t.Fatal(err)
	}
	if result, ok := <-frames; !ok || !result.GetLog().GetComplete() {
		t.Fatalf("terminal result = %#v, open=%v", result, ok)
	}
	if _, ok := <-frames; ok {
		t.Fatal("terminal result did not close operation frames")
	}
	oversized := &orchv1alpha1.AgentOperationResult{Result: &orchv1alpha1.AgentOperationResult_Log{Log: &orchv1alpha1.TailLogsResponse{
		RequestId: requestID, Data: make([]byte, operationFrameBytes+1),
	}}}
	if err := manager.routeOperation(session, oversized); err == nil {
		t.Fatal("oversized operation frame was accepted")
	}
	manager.Close()
}

func validAgentHello(nodeID string) *orchv1alpha1.AgentHello {
	return &orchv1alpha1.AgentHello{
		ProtocolVersion: agentProtocolVersion, NodeId: nodeID, BootId: "boot-one",
		OperatingSystem: "linux", Architecture: "amd64", RuntimeVersion: "containerd 2.3.4",
		Snapshotter: "overlayfs", CgroupsV2: true,
	}
}
