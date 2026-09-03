package control

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	agentctl "change.codes/torsten/better-swarm/internal/agent"
	controlclient "change.codes/torsten/better-swarm/internal/client"
	clusterctl "change.codes/torsten/better-swarm/internal/cluster"
	orchruntime "change.codes/torsten/better-swarm/internal/runtime"
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
	agentDir := filepath.Join(root, "agent")
	if result, err := agentctl.Enroll(context.Background(), agentctl.EnrollOptions{
		Name: "node-1", ContextPath: contextPath, DataDir: agentDir, Now: time.Now, Random: rand.Reader,
	}); err != nil || !result.Created {
		t.Fatalf("Enroll() = %#v, %v", result, err)
	}

	manifest := []byte(`schema = "v1alpha1"
name = "hello"

[services.worker]
image = "` + executableImage + `"
command = ["/bin/sleep"]
args = ["3600"]
restart = "always"
`)
	if _, err := remote.ApplyApplication(context.Background(), manifest, "integration-deploy-key-one"); err != nil {
		t.Fatal(err)
	}

	firstRuntime := orchruntime.NewFake()
	firstAgentContext, stopFirstAgent := context.WithCancel(context.Background())
	firstAgentDone := make(chan error, 1)
	go func() {
		firstAgentDone <- agentctl.Serve(firstAgentContext, agentctl.ServeOptions{
			DataDir: agentDir, Snapshotter: "overlayfs", Runtime: firstRuntime, Now: time.Now, Random: rand.Reader,
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

	secondRuntime := orchruntime.NewFake()
	secondRuntime.InjectObject(object)
	secondAgentContext, stopSecondAgent := context.WithCancel(context.Background())
	secondAgentDone := make(chan error, 1)
	go func() {
		secondAgentDone <- agentctl.Serve(secondAgentContext, agentctl.ServeOptions{
			DataDir: agentDir, Snapshotter: "overlayfs", Runtime: secondRuntime, Now: time.Now, Random: rand.Reader,
		})
	}()
	defer func() {
		stopSecondAgent()
		_ = waitForAgentExit(secondAgentDone)
	}()
	waitForNodeConnection(t, remote, orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_CONNECTED)
	waitForApplicationStatus(t, remote, "hello", orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_RUNNING)
	for _, operation := range secondRuntime.SnapshotOperations() {
		if strings.HasPrefix(operation, "ensure:") {
			t.Fatalf("agent restart duplicated an adopted task: %v", secondRuntime.SnapshotOperations())
		}
	}
	if _, err := remote.RemoveApplication(context.Background(), "hello", "integration-remove-key-one"); err != nil {
		t.Fatal(err)
	}
	waitForApplicationStatus(t, remote, "hello", orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_REMOVED)
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

func waitForApplicationStatus(t *testing.T, remote *controlclient.Client, name string, want orchv1alpha1.ExecutionStatus) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last orchv1alpha1.ExecutionStatus
	var lastErr error
	for time.Now().Before(deadline) {
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
	t.Fatalf("application status = %s, want %s (last error %v)", last, want, lastErr)
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

func waitForAgentExit(done <-chan error) error {
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		return errors.New("agent did not exit")
	}
}
