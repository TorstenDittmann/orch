package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	clusterctl "change.codes/torsten/better-swarm/internal/cluster"
	"change.codes/torsten/better-swarm/internal/control"
)

type cliApplicationReader struct{ app *orchv1alpha1.Application }

func (r cliApplicationReader) GetApplication(context.Context, string) (*orchv1alpha1.Application, error) {
	return r.app, nil
}

func TestWaitOutputJSONAndExitStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		status  orchv1alpha1.ExecutionStatus
		timeout time.Duration
		code    int
		result  string
	}{
		{"ready", orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_HEALTHY, time.Second, ExitOK, "READY"},
		{"rolled back", orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_ROLLED_BACK, time.Second, ExitRuntime, "FAILED"},
		{"timeout", orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_PENDING, time.Millisecond, ExitRuntime, "TIMED_OUT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := &orchv1alpha1.Application{Name: "hello", Generation: 2, RevisionId: "rev-two", ObservedGeneration: 2, ActiveGeneration: 2, ActiveRevisionId: "rev-two", ExecutionStatus: tc.status}
			var out, errOut bytes.Buffer
			code := waitForDeployment(context.Background(), cliApplicationReader{app}, "hello", 2, "rev-two", "idempotency-one", tc.timeout, true, &out, &errOut)
			var result map[string]any
			if code != tc.code || json.Unmarshal(out.Bytes(), &result) != nil || result["status"] != tc.result || result["idempotency_key"] != "idempotency-one" {
				t.Fatalf("code %d output %s stderr %s", code, out.String(), errOut.String())
			}
		})
	}
}

func TestDeploymentCLIHistoryDiffRollbackAndWait(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "state")
	path := filepath.Join(root, "config", "context.json")
	if _, err := clusterctl.Bootstrap(context.Background(), clusterctl.BootstrapOptions{Name: "test", Server: "https://127.0.0.1", DataDir: dir, ContextPath: path, Now: time.Now, Random: rand.Reader}); err != nil {
		t.Fatal(err)
	}
	server, err := control.Start(context.Background(), control.Options{DataDir: dir, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	rewriteCLIContextServer(t, path, "https://"+server.Addr().String())
	first := writeManifest(t, cliManifest)
	second := writeManifest(t, strings.Replace(cliManifest, "example/web@sha256:1111111111111111111111111111111111111111111111111111111111111111", "example/web@sha256:2222222222222222222222222222222222222222222222222222222222222222", 1))
	invoke := func(want int, args ...string) []byte {
		t.Helper()
		var out, errOut bytes.Buffer
		args = append(args, "--context", path, "--json")
		if code := Run(args, &out, &errOut); code != want {
			t.Fatalf("%v code=%d want=%d out=%s err=%s", args, code, want, out.String(), errOut.String())
		}
		return out.Bytes()
	}
	var accepted struct {
		RevisionID string `json:"revision_id"`
	}
	if err := json.Unmarshal(invoke(ExitOK, "deploy", "-f", first), &accepted); err != nil {
		t.Fatal(err)
	}
	invoke(ExitOK, "deploy", "-f", second)
	history := invoke(ExitOK, "app", "history", "hello", "--limit", "1")
	if !bytes.Contains(history, []byte("next_before_generation")) {
		t.Fatalf("no page cursor: %s", history)
	}
	diff := invoke(ExitOK, "app", "diff", "hello", accepted.RevisionID)
	if !bytes.Contains(diff, []byte("services.web.image")) {
		t.Fatalf("missing comparison: %s", diff)
	}
	rollback := invoke(ExitOK, "rollback", "hello", accepted.RevisionID, "--expected-generation", "2", "--idempotency-key", "idempotency-rollback")
	replay := invoke(ExitOK, "rollback", "hello", accepted.RevisionID, "--expected-generation", "2", "--idempotency-key", "idempotency-rollback")
	if !bytes.Equal(rollback, replay) {
		t.Fatalf("rollback replay differs: %s vs %s", rollback, replay)
	}
	invoke(ExitRuntime, "rollback", "hello", accepted.RevisionID, "--expected-generation", "2", "--idempotency-key", "idempotency-stale")
	waited := invoke(ExitRuntime, "deploy", "-f", first, "--wait", "--timeout", "10ms")
	if !bytes.Contains(waited, []byte("TIMED_OUT")) {
		t.Fatalf("wait claimed readiness without agent: %s", waited)
	}
	invoke(ExitRuntime, "app", "watch", "hello", "--revision", accepted.RevisionID, "--timeout", "1s")
	invoke(ExitInvalid, "rollback", "hello", accepted.RevisionID, "--idempotency-key", "idempotency-no-generation")
}

type cliImageResolver struct{}

func (cliImageResolver) Resolve(_ context.Context, image, _ string, _ []byte) (string, error) {
	return "docker.io/example/web@sha256:" + strings.Repeat("a", 64), nil
}

func TestTagDeployCLIReceiptAndHistory(t *testing.T) {
	root := t.TempDir()
	dir, path := filepath.Join(root, "state"), filepath.Join(root, "context.json")
	if _, err := clusterctl.Bootstrap(context.Background(), clusterctl.BootstrapOptions{Name: "test", Server: "https://127.0.0.1", DataDir: dir, ContextPath: path, Now: time.Now, Random: rand.Reader}); err != nil {
		t.Fatal(err)
	}
	server, err := control.Start(context.Background(), control.Options{DataDir: dir, Listen: "127.0.0.1:0", ImageResolver: cliImageResolver{}})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown(context.Background())
	rewriteCLIContextServer(t, path, "https://"+server.Addr().String())
	file := writeManifest(t, strings.Replace(cliManifest, "example/web@sha256:"+strings.Repeat("1", 64), "example/web:latest", 1))
	var out, errOut bytes.Buffer
	if code := Run([]string{"deploy", "-f", file, "--context", path}, &out, &errOut); code != ExitOK {
		t.Fatalf("tag receipt rejected: %d %s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"app", "history", "hello", "--context", path}, &out, &errOut); code != ExitOK {
		t.Fatalf("history: %d %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "example/web:latest -> docker.io/example/web@sha256:"+strings.Repeat("a", 64)) {
		t.Fatalf("missing tag resolution: %s", out.String())
	}
}
