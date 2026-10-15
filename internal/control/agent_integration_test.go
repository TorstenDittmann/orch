package control

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	agentctl "github.com/TorstenDittmann/orch/internal/agent"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/discovery"
	"github.com/TorstenDittmann/orch/internal/ingress"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

func TestLoopbackAgentRunsOperatesAdoptsAndRemovesWorkload(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback lifecycle test")
	}
	address := reserveLoopbackAddress(t)
	root := t.TempDir()
	controllerDir := filepath.Join(root, "controller")
	contextPath := filepath.Join(root, "operator", "context.json")
	endpoint := "https://" + address
	if _, err := clusterctl.Bootstrap(context.Background(), clusterctl.BootstrapOptions{
		Name: "test", Server: endpoint, DataDir: controllerDir, ContextPath: contextPath,
		Now: time.Now, Random: rand.Reader,
	}); err != nil {
		t.Fatal(err)
	}
	server, err := Start(context.Background(), Options{DataDir: controllerDir, Listen: address})
	if err != nil {
		t.Fatal(err)
	}
	serverRunning := true
	t.Cleanup(func() {
		if serverRunning {
			_ = server.Shutdown(context.Background())
		}
	})
	remote, err := controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.CloseIdleConnections()
	network, err := remote.ConfigureNetwork(context.Background(), "10.96.0.0/16", "integration-network-key-one")
	if err != nil || !network.GetChanged() || network.GetCidr() != "10.96.0.0/16" || network.GetGeneration() != 2 {
		t.Fatalf("ConfigureNetwork() = %#v, %v", network, err)
	}
	replay, err := remote.ConfigureNetwork(context.Background(), "10.96.0.0/16", "integration-network-key-one")
	if err != nil || replay.GetChanged() != network.GetChanged() || replay.GetGeneration() != network.GetGeneration() {
		t.Fatalf("ConfigureNetwork() replay = %#v, %v", replay, err)
	}
	agentDir := filepath.Join(root, "agent")
	if result, err := agentctl.Enroll(context.Background(), agentctl.EnrollOptions{
		Name: "node-1", ContextPath: contextPath, DataDir: agentDir, Now: time.Now, Random: rand.Reader,
	}); err != nil || !result.Created {
		t.Fatalf("Enroll() = %#v, %v", result, err)
	}

	manifest := mustCanonicalConfig(t, newTestManifest("hello", map[string]config.Service{
		"worker": {
			Image: executableImage, Command: []string{"/bin/sleep"}, Args: []string{"3600"}, Restart: "always",
		},
	}))
	if _, err := remote.ApplyApplication(context.Background(), manifest, "integration-deploy-key-one"); err != nil {
		t.Fatal(err)
	}

	firstRuntime := orchruntime.NewFake()
	firstAgentContext, stopFirstAgent := context.WithCancel(context.Background())
	firstAgentDone := make(chan error, 1)
	go func() {
		firstAgentDone <- agentctl.Serve(firstAgentContext, agentctl.ServeOptions{
			DataDir: agentDir, Snapshotter: "overlayfs", Runtime: firstRuntime, Discovery: discovery.NewFake(), Ingress: &ingress.Fake{},
			Now: time.Now, Random: rand.Reader,
		})
	}()
	defer func() {
		stopFirstAgent()
		select {
		case <-firstAgentDone:
		default:
		}
	}()
	waitForApplicationStatus(t, remote, "hello", orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_RUNNING)
	waitForNodeConnection(t, remote, orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_CONNECTED)
	waitForNetworkFrozen(t, remote, "10.96.0.0/16", 2)
	objects := firstRuntime.SnapshotObjects()
	if len(objects) != 1 {
		t.Fatalf("runtime objects = %#v", objects)
	}
	var object orchruntime.Object
	for _, item := range objects {
		object = item
	}
	firstRuntime.SetLogs(object.ID, []orchruntime.LogFrame{
		{Stream: "stdout", Data: []byte("known stdout\n"), Offset: 13},
		{Stream: "stderr", Data: []byte("known stderr\n"), Offset: 13},
	})
	logContext, cancelLogs := context.WithTimeout(context.Background(), 5*time.Second)
	logs, err := remote.TailLogs(logContext, "hello", "worker", 1, 100)
	if err != nil {
		cancelLogs()
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	for logs.Receive() {
		frame := logs.Msg()
		if frame.GetStream() == orchv1alpha1.LogStream_LOG_STREAM_STDERR {
			stderr.Write(frame.GetData())
		} else {
			stdout.Write(frame.GetData())
		}
	}
	cancelLogs()
	if err := logs.Err(); err != nil || stdout.String() != "known stdout\n" || stderr.String() != "known stderr\n" {
		t.Fatalf("logs = stdout %q stderr %q, %v", stdout.String(), stderr.String(), err)
	}

	firstRuntime.SetExecFrames(object.ID, "/bin/echo", []orchruntime.ExecFrame{
		{Stream: "stdout", Data: []byte("exec ok\n")}, {Exited: true, ExitCode: 0},
	})
	execContext, cancelExec := context.WithTimeout(context.Background(), 5*time.Second)
	execStream, err := remote.Exec(execContext, "hello", "worker", 1, []string{"/bin/echo", "ok"}, 3)
	if err != nil {
		cancelExec()
		t.Fatal(err)
	}
	var execOutput strings.Builder
	exited := false
	for execStream.Receive() {
		frame := execStream.Msg()
		execOutput.Write(frame.GetData())
		exited = exited || frame.GetExited()
	}
	cancelExec()
	if err := execStream.Err(); err != nil || execOutput.String() != "exec ok\n" || !exited {
		t.Fatalf("exec = %q exited=%v, %v", execOutput.String(), exited, err)
	}

	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	serverRunning = false
	if objects := firstRuntime.SnapshotObjects(); len(objects) != 1 || !objects[object.ID].Running {
		t.Fatalf("controller shutdown changed workload: %#v", objects)
	}
	server, err = Start(context.Background(), Options{DataDir: controllerDir, Listen: address})
	if err != nil {
		t.Fatal(err)
	}
	serverRunning = true
	remote.CloseIdleConnections()
	remote, err = controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	waitForNodeConnection(t, remote, orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_CONNECTED)
	waitForApplicationStatus(t, remote, "hello", orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_RUNNING)

	stopFirstAgent()
	if err := waitForAgentExit(firstAgentDone); err != nil {
		t.Fatalf("first agent exit = %v", err)
	}
	if objects := firstRuntime.SnapshotObjects(); len(objects) != 1 || !objects[object.ID].Running {
		t.Fatalf("agent shutdown changed workload: %#v", objects)
	}

	// Simulate a disconnect after durably caching the current assignment but
	// before its ACK reaches the controller. Reconnect must ACK the snapshot
	// before sending any cached report, or the controller rejects every stream.
	db, err := sql.Open("sqlite", filepath.Join(controllerDir, clusterctl.DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	_, resetErr := db.Exec(`UPDATE nodes SET acknowledged_generation = 0, acknowledged_digest = ''`)
	closeErr := db.Close()
	if resetErr != nil || closeErr != nil {
		t.Fatalf("simulate lost assignment ACK: %v, %v", resetErr, closeErr)
	}

	secondRuntime := orchruntime.NewFake()
	secondRuntime.InjectObject(object)
	secondAgentContext, stopSecondAgent := context.WithCancel(context.Background())
	secondAgentDone := make(chan error, 1)
	go func() {
		secondAgentDone <- agentctl.Serve(secondAgentContext, agentctl.ServeOptions{
			DataDir: agentDir, Snapshotter: "overlayfs", Runtime: secondRuntime, Discovery: discovery.NewFake(), Ingress: &ingress.Fake{},
			Now: time.Now, Random: rand.Reader,
		})
	}()
	defer func() {
		stopSecondAgent()
		_ = waitForAgentExit(secondAgentDone)
	}()
	waitForNodeConnection(t, remote, orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_CONNECTED)
	waitForApplicationStatus(t, remote, "hello", orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_RUNNING)
	deadline := time.Now().Add(10 * time.Second)
	for {
		nodes, err := remote.ListNodes(context.Background())
		if err == nil && len(nodes) == 1 && nodes[0].GetAcknowledgedGeneration() != 0 &&
			nodes[0].GetAcknowledgedGeneration() == nodes[0].GetAssignmentGeneration() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cached assignment was not acknowledged after reconnect: %v, %v", nodes, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, operation := range secondRuntime.SnapshotOperations() {
		if strings.HasPrefix(operation, "ensure:") {
			t.Fatalf("agent restart duplicated an adopted task: %v", secondRuntime.SnapshotOperations())
		}
	}
	removed, err := remote.RemoveApplication(context.Background(), "hello", "integration-remove-key-one")
	if err != nil || removed.GetExecutionStatus() != orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_STOPPING {
		t.Fatalf("RemoveApplication() = %#v, %v", removed, err)
	}
	waitForApplicationStatus(t, remote, "hello", orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_STOPPING)
	if objects := secondRuntime.SnapshotObjects(); len(objects) != 1 || !objects[object.ID].Running {
		t.Fatalf("runtime object did not remain alive for DNS drain: %#v", objects)
	}
	waitForApplicationStatus(t, remote, "hello", orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_REMOVED, statusWaitOptions{
		agentDone: secondAgentDone,
		diagnostics: func() string {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			nodes, err := remote.ListNodes(ctx)
			node := "missing"
			if len(nodes) == 1 {
				node = fmt.Sprintf("connection=%s assignment=%d acked=%d network=%s dns=%s ingress=%s drains=%d",
					nodes[0].GetConnectionStatus(), nodes[0].GetAssignmentGeneration(), nodes[0].GetAcknowledgedGeneration(),
					nodes[0].GetNetworkState(), nodes[0].GetDnsState(), nodes[0].GetIngressState(), nodes[0].GetDrainingObjects())
			}
			return fmt.Sprintf("operations=%v objects=%#v node={%s} nodes_error=%v",
				secondRuntime.SnapshotOperations(), secondRuntime.SnapshotObjects(), node, err)
		},
	})
	if objects := secondRuntime.SnapshotObjects(); len(objects) != 0 {
		t.Fatalf("runtime objects after remove = %#v", objects)
	}
}

func reserveLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

type statusWaitOptions struct {
	agentDone   <-chan error
	diagnostics func() string
}

func waitForApplicationStatus(t *testing.T, remote *controlclient.Client, name string, want orchv1alpha1.ExecutionStatus, options ...statusWaitOptions) {
	t.Helper()
	// Removal includes a mandatory five-second DNS TTL drain followed by the
	// two-second agent reconcile/report cadence. Leave enough scheduling margin
	// for race instrumentation without weakening any state assertion.
	deadline := time.Now().Add(20 * time.Second)
	var last orchv1alpha1.ExecutionStatus
	var lastErr error
	for time.Now().Before(deadline) {
		if len(options) != 0 && options[0].agentDone != nil {
			select {
			case err := <-options[0].agentDone:
				t.Fatalf("agent exited while waiting for %s: %v", want, err)
			default:
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		application, err := remote.GetApplication(ctx, name)
		cancel()
		if err == nil {
			last = application.GetExecutionStatus()
			if last == want {
				return
			}
		} else {
			lastErr = err
		}
		time.Sleep(25 * time.Millisecond)
	}
	diagnostics := ""
	if len(options) != 0 && options[0].diagnostics != nil {
		diagnostics = "; " + options[0].diagnostics()
	}
	t.Fatalf("application status = %s, want %s (last error %v)%s", last, want, lastErr, diagnostics)
}

func waitForNodeConnection(t *testing.T, remote *controlclient.Client, want orchv1alpha1.NodeConnectionStatus) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last orchv1alpha1.NodeConnectionStatus
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		nodes, err := remote.ListNodes(ctx)
		cancel()
		if err == nil && len(nodes) == 1 {
			last = nodes[0].GetConnectionStatus()
			if last == want {
				return
			}
		} else if err != nil {
			lastErr = err
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("node connection = %s, want %s (last error %v)", last, want, lastErr)
}

func waitForNetworkFrozen(t *testing.T, remote *controlclient.Client, cidr string, generation uint64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last *orchv1alpha1.Cluster
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		cluster, err := remote.GetCluster(ctx)
		cancel()
		if err == nil {
			last = cluster
			if cluster.GetWorkloadCidr() == cidr && cluster.GetNetworkGeneration() == generation && cluster.GetNetworkFrozen() {
				return
			}
		} else {
			lastErr = err
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("cluster network = %#v, want cidr=%q generation=%d frozen=true (last error %v)", last, cidr, generation, lastErr)
}

func waitForAgentExit(done <-chan error) error {
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		return errors.New("agent did not exit")
	}
}
