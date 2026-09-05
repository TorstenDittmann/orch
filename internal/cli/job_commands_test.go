package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	agentctl "change.codes/torsten/better-swarm/internal/agent"
	controlclient "change.codes/torsten/better-swarm/internal/client"
	clusterctl "change.codes/torsten/better-swarm/internal/cluster"
	"change.codes/torsten/better-swarm/internal/control"
	"change.codes/torsten/better-swarm/internal/discovery"
	"change.codes/torsten/better-swarm/internal/ingress"
	orchruntime "change.codes/torsten/better-swarm/internal/runtime"
)

func TestJobCLIThroughControllerAndAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback controller/agent")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir, path := filepath.Join(root, "controller"), filepath.Join(root, "context.json")
	if _, err := clusterctl.Bootstrap(ctx, clusterctl.BootstrapOptions{Name: "test", Server: "https://" + address, DataDir: dir, ContextPath: path, Now: time.Now, Random: rand.Reader}); err != nil {
		t.Fatal(err)
	}
	server, err := control.Start(ctx, control.Options{DataDir: dir, Listen: address})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown(context.Background())
	rewriteCLIContextServer(t, path, "https://"+server.Addr().String())
	agentDir := filepath.Join(root, "agent")
	if _, err := agentctl.Enroll(ctx, agentctl.EnrollOptions{Name: "node-one", ContextPath: path, DataDir: agentDir, Now: time.Now, Random: rand.Reader}); err != nil {
		t.Fatal(err)
	}
	runtime := orchruntime.NewFake()
	runtime.Capabilities.TotalCPUMillis = 4000
	runtime.Capabilities.TotalMemoryBytes = 8 << 30
	agentCtx, stopAgent := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- agentctl.Serve(agentCtx, agentctl.ServeOptions{DataDir: agentDir, Snapshotter: "overlayfs", Runtime: runtime, Discovery: discovery.NewFake(), Ingress: &ingress.Fake{}, Now: time.Now, Random: rand.Reader})
	}()
	defer func() {
		stopAgent()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("agent did not stop")
		}
	}()
	remote, err := controlclient.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.CloseIdleConnections()
	invoke := func(want int, args ...string) []byte {
		t.Helper()
		var out, errOut bytes.Buffer
		// Insert context before the process-command delimiter.
		position := len(args)
		for i, arg := range args {
			if arg == "--" {
				position = i
				break
			}
		}
		prepared := append([]string(nil), args[:position]...)
		prepared = append(prepared, "--context", path)
		prepared = append(prepared, args[position:]...)
		if code := RunContext(ctx, prepared, &out, &errOut); code != want {
			t.Fatalf("%v: code=%d want=%d out=%s err=%s", args, code, want, out.String(), errOut.String())
		}
		return out.Bytes()
	}
	file := writeManifest(t, `amends "orch:/v1alpha1/Application.pkl"
name = "hello"
services { ["web"] { image = "example/web@sha256:`+strings.Repeat("a", 64)+`" } }
`)
	invoke(ExitOK, "deploy", "-f", file)
	wait := func(test func() bool) {
		t.Helper()
		for {
			if test() {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("timed out waiting for job lifecycle")
			case <-time.After(30 * time.Millisecond):
			}
		}
	}
	wait(func() bool {
		app, err := remote.GetApplication(ctx, "hello")
		return err == nil && app.GetExecutionStatus() == orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_RUNNING
	})
	var first orchv1alpha1.OneOffJob
	if err := json.Unmarshal(invoke(ExitOK, "job", "run", "hello", "web", "--json", "--idempotency-key", "cli-job-request-one", "--", "/bin/maintenance", "--context", "process-argument"), &first); err != nil {
		t.Fatal(err)
	}
	if first.Id == "" || first.Image == "" {
		t.Fatal("missing accepted job identity")
	}
	wait(func() bool {
		job, err := remote.GetJob(ctx, first.Id)
		return err == nil && job.GetStatus().GetState() == orchv1alpha1.JobRunState_JOB_RUN_STATE_RUNNING
	})
	objectID := ""
	for id, object := range runtime.SnapshotObjects() {
		if object.AllocationID == first.Id {
			objectID = id
		}
	}
	if objectID == "" {
		t.Fatal("accepted job did not reach runtime")
	}
	runtime.SetLogs(objectID, []orchruntime.LogFrame{{Stream: "stdout", Data: []byte("maintenance output\n")}})
	if output := invoke(ExitOK, "job", "logs", first.Id); !bytes.Contains(output, []byte("maintenance output")) {
		t.Fatalf("job logs: %s", output)
	}
	if err := runtime.Exit(objectID, 37); err != nil {
		t.Fatal(err)
	}
	output := invoke(ExitRuntime, "job", "wait", first.Id, "--timeout", "10s", "--json")
	var finished orchv1alpha1.OneOffJob
	if err := json.Unmarshal(output, &finished); err != nil || finished.GetStatus().GetExitCode() != 37 || !finished.GetStatus().GetHasExitCode() {
		t.Fatalf("exit code missing: %s %v", output, err)
	}
	var retried orchv1alpha1.OneOffJob
	if err := json.Unmarshal(invoke(ExitOK, "job", "retry", first.Id, "--idempotency-key", "cli-job-retry-one", "--json"), &retried); err != nil {
		t.Fatal(err)
	}
	if retried.Id == first.Id || retried.RetryOf != first.Id {
		t.Fatal("retry did not create a new run")
	}
	invoke(ExitOK, "job", "cancel", retried.Id)
	wait(func() bool {
		job, err := remote.GetJob(ctx, retried.Id)
		return err == nil && job.GetStatus().GetFinishedAt() != nil
	})
	output = invoke(ExitOK, "job", "list", "hello", "--json")
	if !bytes.Contains(output, []byte(first.Id)) || !bytes.Contains(output, []byte(retried.Id)) {
		t.Fatalf("history lost runs: %s", output)
	}
}
