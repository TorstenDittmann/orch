package client

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"google.golang.org/protobuf/proto"
)

func readyApplication() *orchv1alpha1.Application {
	return &orchv1alpha1.Application{Name: "hello", Generation: 2, RevisionId: "rev-two", ObservedGeneration: 2, ActiveGeneration: 2, ActiveRevisionId: "rev-two", ExecutionStatus: orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_HEALTHY}
}

func TestDeploymentReadinessRequiresExactReadyGeneration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		edit        func(*orchv1alpha1.Application)
		ready, fail bool
	}{
		{"healthy", func(*orchv1alpha1.Application) {}, true, false},
		{"process without gates", func(a *orchv1alpha1.Application) {
			a.ExecutionStatus = orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_RUNNING
		}, true, false},
		{"old observation", func(a *orchv1alpha1.Application) { a.ObservedGeneration = 1 }, false, false},
		{"fallback", func(a *orchv1alpha1.Application) { a.ActiveGeneration = 1; a.ActiveRevisionId = "rev-one" }, false, false},
		{"blocked", func(a *orchv1alpha1.Application) {
			a.ExecutionStatus = orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_BLOCKED
		}, false, false},
		{"pending", func(a *orchv1alpha1.Application) {
			a.ExecutionStatus = orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_PENDING
		}, false, false},
		{"failed", func(a *orchv1alpha1.Application) {
			a.ExecutionStatus = orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_FAILED
		}, false, true},
		{"automatic rollback", func(a *orchv1alpha1.Application) {
			a.ExecutionStatus = orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_ROLLED_BACK
		}, false, true},
		{"superseded", func(a *orchv1alpha1.Application) { a.Generation = 3 }, false, true},
		{"removed", func(a *orchv1alpha1.Application) { a.Deleted = true }, false, true},
		{"wrong revision", func(a *orchv1alpha1.Application) { a.RevisionId = "different" }, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := readyApplication()
			tc.edit(app)
			ready, err := DeploymentReady(app, 2, "rev-two")
			if ready != tc.ready || (err != nil) != tc.fail {
				t.Fatalf("ready=%t err=%v", ready, err)
			}
		})
	}
}

type sequenceReader struct {
	calls  int
	values []*orchv1alpha1.Application
	err    error
}

func (r *sequenceReader) GetApplication(ctx context.Context, _ string) (*orchv1alpha1.Application, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return proto.Clone(r.values[min(r.calls-1, len(r.values)-1)]).(*orchv1alpha1.Application), nil
}

func TestWaitDeploymentProgressTimeoutCancellationAndDisconnect(t *testing.T) {
	t.Parallel()
	pending := readyApplication()
	pending.ObservedGeneration = 0
	reader := &sequenceReader{values: []*orchv1alpha1.Application{pending, readyApplication()}}
	seen := 0
	got, err := WaitDeployment(context.Background(), reader, "hello", 2, "rev-two", time.Millisecond, func(*orchv1alpha1.Application) { seen++ })
	if err != nil || got.GetGeneration() != 2 || seen != 2 {
		t.Fatalf("wait: %+v %v observed=%d", got, err, seen)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	reader = &sequenceReader{values: []*orchv1alpha1.Application{pending}}
	if _, err := WaitDeployment(ctx, reader, "hello", 2, "rev-two", time.Second, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	calls := reader.calls
	if _, err := WaitDeployment(ctx, reader, "hello", 2, "rev-two", time.Second, nil); !errors.Is(err, context.Canceled) || reader.calls != calls {
		t.Fatalf("cancellation: %v", err)
	}
	reader = &sequenceReader{err: errors.New("disconnected")}
	if _, err := WaitDeployment(context.Background(), reader, "hello", 2, "rev-two", time.Second, nil); err == nil || !strings.Contains(err.Error(), "disconnected") {
		t.Fatalf("connection: %v", err)
	}
}
