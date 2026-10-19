package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	agentctl "github.com/TorstenDittmann/orch/internal/agent"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/control"
	"github.com/TorstenDittmann/orch/internal/discovery"
	"github.com/TorstenDittmann/orch/internal/ingress"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
	"github.com/TorstenDittmann/orch/internal/security"
	"github.com/TorstenDittmann/orch/internal/store"
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
	file := writeManifest(t, `amends "https://raw.githubusercontent.com/TorstenDittmann/orch/main/pkl/v1alpha1/Application.pkl"
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

	// Recover onto a fresh controller directory while retaining the agent's
	// cache and administrator context. The original controller stays stopped.
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	remote.CloseIdleConnections()
	state, err := store.OpenInitialized(ctx, filepath.Join(dir, clusterctl.DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	box, err := security.LoadSecretBox(filepath.Join(dir, clusterctl.SecretKeyFilename), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.SetApplicationSecret(ctx, box, "hello", "recovery", "secv_recovery", []byte("recovered-secret"), "recovery-secret-request", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	backup, key := filepath.Join(root, "backup", "cluster.db"), filepath.Join(root, "saved-key")
	local := func(args ...string) []byte {
		t.Helper()
		var out, errOut bytes.Buffer
		if code := RunContext(ctx, args, &out, &errOut); code != ExitOK {
			t.Fatalf("recovery CLI: %d %s", code, errOut.String())
		}
		return out.Bytes()
	}
	local("cluster", "backup", "--data-dir", dir, "--output", backup, "--secret-key-output", key)
	savedPKI := filepath.Join(root, "saved-pki")
	if err := os.Mkdir(savedPKI, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ca.pem", "ca-key.pem", "controller.pem", "controller-key.pem"} {
		contents, err := os.ReadFile(filepath.Join(dir, "pki", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(savedPKI, name), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(dir, filepath.Join(root, "failed-controller")); err != nil {
		t.Fatal(err)
	}
	var inspection clusterctl.RecoveryResult
	if err := json.Unmarshal(local("cluster", "backup", "inspect", "--backup", backup, "--pki-dir", savedPKI, "--secret-key", key, "--json"), &inspection); err != nil || !inspection.Ready || inspection.Jobs != 2 {
		t.Fatalf("inspect recovery: %+v %v", inspection, err)
	}
	recovered := filepath.Join(root, "recovered")
	local("cluster", "restore", "--backup", backup, "--pki-dir", savedPKI, "--secret-key", key, "--data-dir", recovered)
	recoveredState, err := store.OpenInitialized(ctx, filepath.Join(recovered, clusterctl.DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	recoveredBox, err := security.LoadSecretBox(filepath.Join(recovered, clusterctl.SecretKeyFilename), nil)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := recoveredState.ResolveApplicationSecretVersion(ctx, recoveredBox, "hello", "recovery", "secv_recovery")
	if err != nil || string(secret.Value) != "recovered-secret" {
		t.Fatal("recovered secret unavailable")
	}
	if err := recoveredState.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := control.Start(ctx, control.Options{DataDir: recovered, Listen: address})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Shutdown(context.Background())
	// A successful routed log request proves the original agent authenticated
	// and reconnected to the recovered controller with its existing identity.
	wait(func() bool {
		var out, errOut bytes.Buffer
		return RunContext(ctx, []string{"job", "logs", first.Id, "--context", path}, &out, &errOut) == ExitOK && bytes.Contains(out.Bytes(), []byte("maintenance output"))
	})
	var replay orchv1alpha1.OneOffJob
	if err := json.Unmarshal(invoke(ExitOK, "job", "run", "hello", "web", "--json", "--idempotency-key", "cli-job-request-one", "--", "/bin/maintenance", "--context", "process-argument"), &replay); err != nil || replay.Id != first.Id || replay.GetStatus().GetFinishedAt() == nil || replay.GetStatus().GetExitCode() != 37 {
		t.Fatalf("recovery lost terminal replay: %v", err)
	}
	app, err := remote.GetApplication(ctx, "hello")
	if err != nil || app == nil {
		t.Fatalf("recovered deployment unavailable: %v", err)
	}
	for _, object := range runtime.SnapshotObjects() {
		if object.AllocationID == first.Id && object.Running {
			t.Fatal("recovery restarted completed job")
		}
	}
}
