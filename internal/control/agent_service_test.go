package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/gen/orch/v1alpha1/orchv1alpha1connect"
	agentctl "github.com/TorstenDittmann/orch/internal/agent"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/security"
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

func TestAgentCertificateRenewalPromotesOnReconnect(t *testing.T) {
	root := t.TempDir()
	controllerDir := filepath.Join(root, "controller")
	contextPath := filepath.Join(root, "operator", "context.json")
	address := reserveLoopbackAddress(t)
	endpoint := "https://" + address
	if _, err := clusterctl.Bootstrap(context.Background(), clusterctl.BootstrapOptions{Name: "test", Server: endpoint,
		DataDir: controllerDir, ContextPath: contextPath, Now: time.Now, Random: rand.Reader}); err != nil {
		t.Fatal(err)
	}
	server, err := Start(context.Background(), Options{DataDir: controllerDir, Listen: address})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	agentDir := filepath.Join(root, "agent")
	if _, err := agentctl.Enroll(context.Background(), agentctl.EnrollOptions{Name: "node-1", ContextPath: contextPath, DataDir: agentDir, Now: time.Now, Random: rand.Reader}); err != nil {
		t.Fatal(err)
	}
	credentials, err := agentctl.Load(agentDir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	csr, hash, err := security.CreateAgentCertificateRequest(credentials.Config.NodeName, credentials.PrivateKeyPEM, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	currentHash, err := security.CertificatePublicKeyHash(credentials.Certificate.Leaf)
	if err != nil || hash != currentHash {
		t.Fatalf("renewal key hash = %q, %v; want %q", hash, err, currentHash)
	}
	oldHTTP := tlsHTTPClient(credentials.RootCAs, credentials.Certificate)
	defer oldHTTP.CloseIdleConnections()
	oldClient := orchv1alpha1connect.NewAgentServiceClient(oldHTTP, endpoint)
	wrongCSR, _, _, err := security.GenerateAgentCertificateRequest("node-1", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oldClient.RenewCertificate(context.Background(), connect.NewRequest(&orchv1alpha1.RenewCertificateRequest{CertificateRequestDer: wrongCSR, IdempotencyKey: "renew-wrong-key-one"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("wrong-key renewal = %v", err)
	}
	renewRequest := &orchv1alpha1.RenewCertificateRequest{CertificateRequestDer: csr, IdempotencyKey: "renew-certificate-one"}
	renewed, err := oldClient.RenewCertificate(context.Background(), connect.NewRequest(renewRequest))
	if err != nil || renewed.Msg.GetNodeId() != credentials.Config.NodeID {
		t.Fatalf("RenewCertificate() = %#v, %v", renewed, err)
	}
	replayed, err := oldClient.RenewCertificate(context.Background(), connect.NewRequest(renewRequest))
	if err != nil || !proto.Equal(replayed.Msg, renewed.Msg) {
		t.Fatalf("renewal replay = %#v, %v", replayed, err)
	}
	admin, err := controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.CloseIdleConnections()
	nodes, err := admin.ListNodes(context.Background())
	if err != nil || len(nodes) != 1 || nodes[0].GetCertificateState() != model.CertificateRenewalPending {
		t.Fatalf("pending nodes = %#v, %v", nodes, err)
	}
	newPair, err := tls.X509KeyPair(renewed.Msg.GetAgentCertificatePem(), credentials.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	newHTTP := tlsHTTPClient(credentials.RootCAs, newPair)
	defer newHTTP.CloseIdleConnections()
	newClient := orchv1alpha1connect.NewAgentServiceClient(newHTTP, endpoint)
	if _, err := newClient.RenewCertificate(context.Background(), connect.NewRequest(&orchv1alpha1.RenewCertificateRequest{CertificateRequestDer: csr, IdempotencyKey: "renew-pending-cert-one"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("pending-certificate renewal = %v", err)
	}
	streamContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newClient.Connect(streamContext)
	if err := stream.Send(&orchv1alpha1.ConnectRequest{Message: &orchv1alpha1.ConnectRequest_Hello{Hello: validAgentHello(credentials.Config.NodeID)}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		nodes, err = admin.ListNodes(context.Background())
		if err == nil && len(nodes) == 1 && nodes[0].GetCertificateState() == model.CertificateHealthy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("certificate was not activated: %#v, %v", nodes, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := oldClient.RenewCertificate(context.Background(), connect.NewRequest(&orchv1alpha1.RenewCertificateRequest{CertificateRequestDer: csr, IdempotencyKey: "renew-certificate-old"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("retired certificate renewal = %v", err)
	}
	_ = stream.CloseRequest()
	_ = stream.CloseResponse()
}

func TestAgentStreamSurvivesReconcileFailure(t *testing.T) {
	root := t.TempDir()
	controllerDir := filepath.Join(root, "controller")
	contextPath := filepath.Join(root, "operator", "context.json")
	address := reserveLoopbackAddress(t)
	endpoint := "https://" + address
	if _, err := clusterctl.Bootstrap(context.Background(), clusterctl.BootstrapOptions{Name: "test", Server: endpoint,
		DataDir: controllerDir, ContextPath: contextPath, Now: time.Now, Random: rand.Reader}); err != nil {
		t.Fatal(err)
	}
	server, err := Start(context.Background(), Options{DataDir: controllerDir, Listen: address})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	agentDir := filepath.Join(root, "agent")
	if _, err := agentctl.Enroll(context.Background(), agentctl.EnrollOptions{Name: "node-1", ContextPath: contextPath, DataDir: agentDir, Now: time.Now, Random: rand.Reader}); err != nil {
		t.Fatal(err)
	}
	credentials, err := agentctl.Load(agentDir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Enrollment published an assignment. Corrupt durable cluster state so
	// every later Reconcile fails before it can publish anything.
	db, err := sql.Open("sqlite", filepath.Join(controllerDir, clusterctl.DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	_, corruptErr := db.Exec(`UPDATE clusters SET created_at = 'invalid'`)
	closeErr := db.Close()
	if corruptErr != nil || closeErr != nil {
		t.Fatalf("corrupt cluster record: %v, %v", corruptErr, closeErr)
	}

	httpClient := tlsHTTPClient(credentials.RootCAs, credentials.Certificate)
	defer httpClient.CloseIdleConnections()
	client := orchv1alpha1connect.NewAgentServiceClient(httpClient, endpoint)
	streamContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := client.Connect(streamContext)
	defer func() {
		_ = stream.CloseRequest()
		_ = stream.CloseResponse()
	}()
	nodeID := credentials.Config.NodeID
	if err := stream.Send(&orchv1alpha1.ConnectRequest{Message: &orchv1alpha1.ConnectRequest_Hello{Hello: validAgentHello(nodeID)}}); err != nil {
		t.Fatal(err)
	}
	response, err := stream.Receive()
	if err != nil || response.GetAssignment() == nil {
		t.Fatalf("assignment after failed hello reconcile = %#v, %v", response, err)
	}
	assignment := response.GetAssignment()
	network := assignment.GetNetwork()
	ingressState := model.SubsystemNotConfigured
	if network.GetIngressEnabled() {
		ingressState = model.SubsystemReady
	}
	messages := []*orchv1alpha1.ConnectRequest{
		{Message: &orchv1alpha1.ConnectRequest_AssignmentAck{AssignmentAck: &orchv1alpha1.AssignmentAck{
			BootId: "boot-one", Generation: assignment.GetGeneration(), Digest: assignment.GetDigest()}}},
		{Message: &orchv1alpha1.ConnectRequest_AllocationReport{AllocationReport: &orchv1alpha1.AllocationReport{
			BootId: "boot-one", Sequence: 1, AssignmentGeneration: assignment.GetGeneration(), AssignmentDigest: assignment.GetDigest(),
			Network: &orchv1alpha1.NetworkStatus{Generation: network.GetGeneration(), State: orchv1alpha1.NetworkState_NETWORK_STATE_READY,
				ClusterCidr: network.GetClusterCidr(), NodeSubnet: network.GetNodeSubnet(), Gateway: network.GetGateway(),
				DnsState: model.SubsystemReady, IngressState: ingressState}}}},
		{Message: &orchv1alpha1.ConnectRequest_Heartbeat{Heartbeat: &orchv1alpha1.AgentHeartbeat{
			BootId: "boot-one", AcknowledgedAssignmentGeneration: assignment.GetGeneration(),
			Usage: &orchv1alpha1.NodeResourceUsage{CpuUsedMillis: 250, MemoryUsedBytes: 1 << 30, DiskUsedBytes: 5 << 30, DiskTotalBytes: 20 << 30}}}},
	}
	for _, message := range messages {
		if err := stream.Send(message); err != nil {
			t.Fatalf("send %T: %v", message.GetMessage(), err)
		}
	}
	received := make(chan error, 1)
	go func() {
		_, err := stream.Receive()
		received <- err
	}()
	select {
	case err := <-received:
		t.Fatalf("agent stream ended after a failed reconcile: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	admin, err := controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.CloseIdleConnections()
	nodes, err := admin.ListNodes(context.Background())
	if err != nil || len(nodes) != 1 || nodes[0].GetConnectionStatus() != orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_CONNECTED {
		t.Fatalf("nodes after failed reconcile = %#v, %v", nodes, err)
	}
	if usage := nodes[0].GetUsage(); usage.GetCpuUsedMillis() != 250 || usage.GetDiskTotalBytes() != 20<<30 {
		t.Fatalf("node usage = %#v", usage)
	}
	history, err := admin.GetNodeUsage(context.Background(), "node-1")
	if err != nil || len(history.GetSamples()) != 1 || history.GetSamples()[0].GetMemoryUsedBytes() != 1<<30 || history.GetStepSeconds() != 300 {
		t.Fatalf("node usage history = %#v, %v", history, err)
	}
	if _, err := admin.GetNodeUsage(context.Background(), "missing"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("missing node usage = %v", err)
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
		{name: "invalid cached digest", edit: func(value *orchv1alpha1.AgentHello) {
			value.CachedAssignmentGeneration = 1
			value.CachedAssignmentDigest = "sha256:" + strings.Repeat("z", 64)
		}},
		{name: "oversized CNI version", edit: func(value *orchv1alpha1.AgentHello) {
			value.CniVersion = strings.Repeat("x", 129)
		}},
		{name: "oversized CNI plugins version", edit: func(value *orchv1alpha1.AgentHello) {
			value.CniPluginsVersion = strings.Repeat("x", 129)
		}},
		{name: "oversized Caddy version", edit: func(value *orchv1alpha1.AgentHello) {
			value.CaddyVersion = strings.Repeat("x", 129)
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

func TestStalledOperationConsumerFailsOnlyThatOperation(t *testing.T) {
	t.Parallel()
	manager := newSessionManager(time.Now)
	manager.random = bytes.NewReader(make([]byte, 32))
	sessionCancelled := false
	session, err := manager.register(context.Background(), "node-one", "boot-one", func() { sessionCancelled = true })
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
	<-session.send
	// Nobody reads frames: the admin client has stalled.
	for range operationFrameBuffer + 8 {
		if err := manager.routeOperation(session, &orchv1alpha1.AgentOperationResult{Result: &orchv1alpha1.AgentOperationResult_Log{Log: &orchv1alpha1.TailLogsResponse{
			RequestId: requestID, Data: []byte("line\n"),
		}}}); err != nil {
			t.Fatalf("routeOperation() ended the agent stream: %v", err)
		}
	}
	received := 0
	var last *orchv1alpha1.AgentOperationResult
	for result := range frames {
		received++
		last = result
	}
	if received != operationFrameBuffer+1 || last.GetError().GetCode() != "resource_exhausted" {
		t.Fatalf("received %d frames, last = %v", received, last)
	}
	if code := connect.CodeOf(agentOperationError(last.GetError())); code != connect.CodeResourceExhausted {
		t.Fatalf("admin error code = %v", code)
	}
	select {
	case message := <-session.send:
		if message.GetCancelOperation().GetRequestId() != requestID {
			t.Fatalf("agent message = %v", message)
		}
	default:
		t.Fatal("overflowed operation was not cancelled on the agent")
	}
	// A full outbound queue drops best-effort operation messages instead of
	// disconnecting the agent.
	for len(session.send) < cap(session.send) {
		session.send <- &orchv1alpha1.ConnectResponse{}
	}
	if _, _, _, err := manager.beginOperation(context.Background(), "node-one", func(id string) *orchv1alpha1.ConnectResponse {
		return &orchv1alpha1.ConnectResponse{Message: &orchv1alpha1.ConnectResponse_TailLogs{TailLogs: &orchv1alpha1.TailLogsCommand{RequestId: id, AllocationId: "alloc-one", TailLines: 1}}}
	}); !errors.Is(err, errAgentBusy) {
		t.Fatalf("beginOperation(full queue) = %v", err)
	}
	if sessionCancelled || !manager.Connected("node-one") {
		t.Fatal("operation backpressure disconnected the agent session")
	}
	manager.Close()
}
