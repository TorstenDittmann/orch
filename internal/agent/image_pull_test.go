package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/discovery"
	"github.com/TorstenDittmann/orch/internal/model"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

func TestSupervisorReportsImagePullErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runtime := orchruntime.NewFake()
	supervisor := NewSupervisorWithServices(supervisorCache(t), runtime, func() time.Time { return supervisorTestTime },
		SupervisorServices{Discovery: discovery.NewFake()})
	allocation := supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))
	runtime.EnsureErrors = map[string]error{allocation.ID: &orchruntime.ImagePullError{
		Image: "ghcr.io/acme/game@sha256:" + strings.Repeat("c", 64),
		Err:   errors.New("failed to resolve reference:\n401 Unauthorized"),
	}}
	statuses, err := supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{allocation}})
	if err != nil && !errors.Is(err, ErrAllocationReconcile) {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Phase != model.AllocationBackoff || statuses[0].Reason != "image_pull_failed" {
		t.Fatalf("statuses = %#v", statuses)
	}
	message := statuses[0].Message
	if !strings.Contains(message, "401 Unauthorized") || strings.Contains(message, "\n") || !strings.HasSuffix(message, "waiting before retry") {
		t.Fatalf("message = %q", message)
	}
}

func TestImagePullMessageFitsTheStatusLimit(t *testing.T) {
	message := imagePullMessage(&orchruntime.ImagePullError{Image: strings.Repeat("i", 1024), Err: errors.New(strings.Repeat("é", 800))}, "waiting before retry")
	if len(message) > 1024 || !strings.HasSuffix(message, "...; waiting before retry") || !strings.Contains(message, "pulling ") {
		t.Fatalf("message (%d bytes) = %q", len(message), message)
	}
}
