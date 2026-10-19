package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

func TestLogReadsDoNotWaitForReconciliationLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runtime := orchruntime.NewFake()
	supervisor := NewSupervisor(supervisorCache(t), runtime, nil)
	allocation := supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))
	if _, err := supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{allocation}}); err != nil {
		t.Fatal(err)
	}
	runtime.SetLogs(orchruntime.ObjectID(allocation.ID, allocation.SpecDigest), []orchruntime.LogFrame{{Stream: "stdout", Data: []byte("alive\n")}})
	// Ingress reconciliation holds this mutex while applying Caddy config.
	// Log reads must finish without waiting for that operation to complete.
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	current, previous := make(chan error, 1), make(chan error, 1)
	go func() {
		batch, err := supervisor.ReadLogs(ctx, allocation, 10, "")
		if err == nil && (len(batch.Frames) != 1 || string(batch.Frames[0].Data) != "alive\n") {
			err = errors.New("missing current log data")
		}
		current <- err
	}()
	go func() {
		_, err := supervisor.ReadPreviousLogs(ctx, allocation, 10, "")
		if errors.Is(err, orchruntime.ErrNotFound) {
			err = nil // No retired instance is valid, but must return promptly.
		}
		previous <- err
	}()
	for _, result := range []<-chan error{current, previous} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("log read blocked behind reconciliation")
		}
	}
}
