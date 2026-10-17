package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/discovery"
	"github.com/TorstenDittmann/orch/internal/model"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

func TestSupervisorStopsWithTheAllocationStopGrace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runtime := orchruntime.NewFake()
	supervisor := NewSupervisorWithServices(supervisorCache(t), runtime, func() time.Time { return supervisorTestTime },
		SupervisorServices{Discovery: discovery.NewFake()})
	slow := supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))
	slow.StopGraceSeconds = 120
	slow.SpecDigest = mustAllocationDigest(t, slow)
	if _, err := supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{slow}}); err != nil {
		t.Fatal(err)
	}
	// Removing the allocation must still use the grace period it started with.
	if _, err := supervisor.Reconcile(ctx, model.Assignment{}); err != nil {
		t.Fatal(err)
	}
	if grace, ok := runtime.LastStopGrace(orchruntime.ObjectID(slow.ID, slow.SpecDigest)); !ok || grace != 2*time.Minute {
		t.Fatalf("stop grace = %s, %t; want 2m", grace, ok)
	}

	plain := supervisorAllocation(t, "sha256:"+strings.Repeat("b", 64))
	if _, err := supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{plain}}); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Reconcile(ctx, model.Assignment{}); err != nil {
		t.Fatal(err)
	}
	if grace, _ := runtime.LastStopGrace(orchruntime.ObjectID(plain.ID, plain.SpecDigest)); grace != orchruntime.DefaultStopGrace {
		t.Fatalf("default stop grace = %s", grace)
	}
}

func TestStopGraceIsPartOfTheRuntimeSpecDigest(t *testing.T) {
	allocation := supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))
	before := mustAllocationDigest(t, allocation)
	allocation.StopGraceSeconds = 30
	if after := mustAllocationDigest(t, allocation); after == before {
		t.Fatal("stop grace is not part of the runtime spec digest")
	}
}

func TestSessionSendsHeartbeatsWhileAReplicaStops(t *testing.T) {
	previous := heartbeatInterval
	heartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = previous })
	ctx := context.Background()
	runtime := orchruntime.NewFake()
	cache := supervisorCache(t)
	supervisor := NewSupervisorWithServices(cache, runtime, func() time.Time { return supervisorTestTime },
		SupervisorServices{Discovery: discovery.NewFake()})
	allocation := supervisorAllocation(t, "sha256:"+strings.Repeat("a", 64))
	if _, err := supervisor.Reconcile(ctx, model.Assignment{Allocations: []model.AllocationDesired{allocation}}); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	runtime.StopHook = func(string) { <-release }
	heartbeats := make(chan struct{}, 16)
	runner := &sessionRunner{cache: cache, supervisor: supervisor, keepalive: func(context.Context) error {
		select {
		case heartbeats <- struct{}{}:
		default:
		}
		return nil
	}}
	done := make(chan error, 1)
	go func() {
		_, err := runner.reconcile(ctx, model.Assignment{})
		done <- err
	}()
	for range 2 {
		select {
		case <-heartbeats:
		case <-time.After(5 * time.Second):
			t.Fatal("no heartbeat while the replica was stopping")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
