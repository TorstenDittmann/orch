package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/discovery"
	"github.com/TorstenDittmann/orch/internal/health"
	"github.com/TorstenDittmann/orch/internal/ingress"
	"github.com/TorstenDittmann/orch/internal/model"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

func TestSupervisorColdUpdateHonorsStartupDeadline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := supervisorTestTime
	runtime := orchruntime.NewFake()
	runtime.Now = func() time.Time { return now }
	probe := &health.Fake{}
	proxy := &ingress.Fake{}
	supervisor := NewSupervisorWithServices(supervisorCache(t), runtime, func() time.Time { return now }, SupervisorServices{
		Prober: probe, Discovery: discovery.NewFake(), Ingress: proxy,
	})
	network, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	old := supervisorHTTPAllocation(t, "alloc-web", "sha256:"+strings.Repeat("a", 64), 1)
	old.StartDeadlineSeconds = 300
	old.MaxSurge, old.MaxUnavailable, old.StartBeforeStop = 0, 1, false
	old.SpecDigest = mustAllocationDigest(t, old)
	assignment := model.Assignment{Network: network, Allocations: []model.AllocationDesired{old}}
	for range health.ReadySuccesses {
		if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
			t.Fatal(err)
		}
		now = now.Add(health.ProbeInterval)
	}
	replacement := old
	replacement.Image = "sha256:" + strings.Repeat("b", 64)
	replacement.ApplicationGeneration, replacement.RevisionID = 2, "revision-two"
	replacement.Rollback = rollbackFromAllocation(old)
	replacement.SpecDigest = mustAllocationDigest(t, replacement)
	assignment.Allocations = []model.AllocationDesired{replacement}
	probe.Err = errors.New("connection refused while the server warms up")
	for range 12 {
		statuses, err := supervisor.Reconcile(ctx, assignment)
		if err != nil {
			t.Fatal(err)
		}
		status := statuses[0]
		if rollbackStateReason(status.Reason) || status.Phase == model.AllocationFailed {
			t.Fatalf("startup aborted before its 5m deadline: %#v", status)
		}
		now = now.Add(health.ProbeInterval)
	}
	if len(proxy.Routes) != 0 {
		t.Fatalf("warming process was published: %#v", proxy.Routes)
	}
	// The service can still pass its normal readiness gate after more than
	// three unsuccessful startup probes, without being restarted or rolled back.
	probe.Err = nil
	for range health.ReadySuccesses {
		statuses, err := supervisor.Reconcile(ctx, assignment)
		if err != nil {
			t.Fatal(err)
		}
		if statuses[0].Phase == model.AllocationFailed || rollbackStateReason(statuses[0].Reason) {
			t.Fatalf("warming process did not recover: %#v", statuses[0])
		}
		now = now.Add(health.ProbeInterval)
	}
	if len(proxy.Routes) != 1 {
		t.Fatalf("ready process was not published: %#v", proxy.Routes)
	}
}

func TestSupervisorColdRollbackGetsItsOwnStartupDeadline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := supervisorTestTime
	runtime := orchruntime.NewFake()
	runtime.Now = func() time.Time { return now }
	probe := &health.Fake{Err: errors.New("connection refused during rollback startup")}
	supervisor := NewSupervisorWithServices(supervisorCache(t), runtime, func() time.Time { return now }, SupervisorServices{
		Prober: probe, Discovery: discovery.NewFake(), Ingress: &ingress.Fake{},
	})
	network, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.PrepareNetwork(ctx, network); err != nil {
		t.Fatal(err)
	}
	old := supervisorHTTPAllocation(t, "alloc-web", "sha256:"+strings.Repeat("a", 64), 1)
	old.StartDeadlineSeconds = 30
	old.SpecDigest = mustAllocationDigest(t, old)
	replacement := old
	replacement.Image = "sha256:" + strings.Repeat("b", 64)
	replacement.ApplicationGeneration, replacement.RevisionID = 2, "revision-two"
	replacement.StartDeadlineSeconds = 10
	replacement.Rollback = rollbackFromAllocation(old)
	replacement.SpecDigest = mustAllocationDigest(t, replacement)
	object, err := runtime.Ensure(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	status := model.AllocationStatus{ID: replacement.ID, Application: replacement.Application, Service: replacement.Service,
		Replica: 1, ApplicationGeneration: 2, SpecDigest: replacement.SpecDigest, HealthStatus: model.HealthChecking, UpdatedAt: now}
	started := now
	for range health.UnhealthyFailures + 3 {
		status, err = supervisor.rollbackStatus(ctx, replacement, object, status)
		if err != nil || status.Phase != model.AllocationRunning || status.HealthStatus != model.HealthChecking {
			t.Fatalf("cold fallback failed before its own deadline: %#v, %v", status, err)
		}
		if len(supervisor.serving) != 0 {
			t.Fatal("cold fallback received traffic before passing readiness")
		}
		now = now.Add(health.ProbeInterval)
	}
	// The fallback has the old revision's 30s budget, not the candidate's 10s.
	now = started.Add(30*time.Second - health.ProbeInterval)
	status, err = supervisor.rollbackStatus(ctx, replacement, object, status)
	if err != nil || status.Phase == model.AllocationFailed {
		t.Fatalf("fallback lost its startup grace: %#v, %v", status, err)
	}
	now = started.Add(30 * time.Second)
	status, err = supervisor.rollbackStatus(ctx, replacement, object, status)
	if err != nil || status.Phase != model.AllocationFailed || status.Reason != "rollback_health_deadline_exceeded" {
		t.Fatalf("fallback never failed after its deadline: %#v, %v", status, err)
	}
	if !strings.Contains(status.Message, "connection refused during rollback startup") {
		t.Fatalf("rollback failure hid the probe error: %q", status.Message)
	}
}

func TestSupervisorColdRollbackReconcilesThroughStartup(t *testing.T) {
	t.Parallel()
	for _, recovery := range []bool{false, true} {
		name := "deadline"
		if recovery {
			name = "recovery"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			now := supervisorTestTime
			runtime := orchruntime.NewFake()
			runtime.Now = func() time.Time { return now }
			probe := &health.Fake{Err: errors.New("connection refused while starting")}
			proxy := &ingress.Fake{}
			supervisor := NewSupervisorWithServices(supervisorCache(t), runtime, func() time.Time { return now }, SupervisorServices{
				Prober: probe, Discovery: discovery.NewFake(), Ingress: proxy,
			})
			network, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
			if err != nil {
				t.Fatal(err)
			}
			old := supervisorHTTPAllocation(t, "alloc-web", "sha256:"+strings.Repeat("a", 64), 1)
			old.StartDeadlineSeconds = 30
			old.SpecDigest = mustAllocationDigest(t, old)
			candidate := old
			candidate.Image = "sha256:" + strings.Repeat("b", 64)
			candidate.ApplicationGeneration, candidate.RevisionID = 2, "revision-two"
			candidate.StartDeadlineSeconds = 10
			candidate.MaxSurge, candidate.MaxUnavailable, candidate.StartBeforeStop = 0, 1, false
			candidate.Rollback = rollbackFromAllocation(old)
			candidate.SpecDigest = mustAllocationDigest(t, candidate)
			assignment := model.Assignment{Network: network, Allocations: []model.AllocationDesired{candidate}}
			if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
				t.Fatal(err)
			}
			now = now.Add(10 * time.Second)
			statuses, err := supervisor.Reconcile(ctx, assignment)
			if err != nil || statuses[0].ActiveSpecDigest != old.SpecDigest || statuses[0].HealthStatus != model.HealthChecking {
				t.Fatalf("candidate did not start a cold rollback at its deadline: %#v, %v", statuses, err)
			}
			started := now
			for range health.UnhealthyFailures + 3 {
				now = now.Add(health.ProbeInterval)
				statuses, err = supervisor.Reconcile(ctx, assignment)
				if err != nil || statuses[0].Phase != model.AllocationRunning || statuses[0].HealthStatus != model.HealthChecking {
					t.Fatalf("cold rollback lost its startup grace across reconciliation: %#v, %v", statuses, err)
				}
				if len(proxy.Routes) != 0 {
					t.Fatalf("cold rollback received traffic while starting: %#v", proxy.Routes)
				}
			}
			if recovery {
				probe.Err = nil
				for range health.ReadySuccesses {
					now = now.Add(health.ProbeInterval)
					statuses, err = supervisor.Reconcile(ctx, assignment)
					if err != nil {
						t.Fatal(err)
					}
				}
				if statuses[0].HealthStatus != model.HealthReady || statuses[0].ActiveSpecDigest != old.SpecDigest || len(proxy.Routes) != 1 {
					t.Fatalf("cold rollback failed to recover: %#v, routes=%#v", statuses, proxy.Routes)
				}
			} else {
				now = started.Add(30 * time.Second)
				statuses, err = supervisor.Reconcile(ctx, assignment)
				if err != nil || statuses[0].Phase != model.AllocationFailed || statuses[0].Reason != "rollback_health_deadline_exceeded" {
					t.Fatalf("cold rollback did not stop waiting at its deadline: %#v, %v", statuses, err)
				}
			}
		})
	}
}

func TestSupervisorReadyRevisionStillRollsBackAfterThreeFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := supervisorTestTime
	runtime := orchruntime.NewFake()
	runtime.Now = func() time.Time { return now }
	probe := &addressFailingProber{}
	supervisor := NewSupervisorWithServices(supervisorCache(t), runtime, func() time.Time { return now }, SupervisorServices{
		Prober: probe, Discovery: discovery.NewFake(), Ingress: &ingress.Fake{},
	})
	network, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	old := supervisorHTTPAllocation(t, "alloc-web", "sha256:"+strings.Repeat("a", 64), 1)
	old.StartDeadlineSeconds = 300
	old.SpecDigest = mustAllocationDigest(t, old)
	assignment := model.Assignment{Network: network, Allocations: []model.AllocationDesired{old}}
	for range health.ReadySuccesses {
		if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
			t.Fatal(err)
		}
		now = now.Add(health.ProbeInterval)
	}
	replacement := old
	replacement.Image = "sha256:" + strings.Repeat("b", 64)
	replacement.ApplicationGeneration, replacement.RevisionID = 2, "revision-two"
	replacement.Rollback = rollbackFromAllocation(old)
	replacement.SpecDigest = mustAllocationDigest(t, replacement)
	assignment.Allocations = []model.AllocationDesired{replacement}
	for range health.ReadySuccesses {
		if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
			t.Fatal(err)
		}
		now = now.Add(health.ProbeInterval)
	}
	object := runtime.SnapshotObjects()[orchruntime.ObjectID(replacement.ID, replacement.SpecDigest)]
	probe.FailAddress = object.Address
	var statuses []model.AllocationStatus
	for range health.UnhealthyFailures {
		statuses, err = supervisor.Reconcile(ctx, assignment)
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(health.ProbeInterval)
	}
	if !now.Before(object.StartedAt.Add(300*time.Second)) || !rollbackStateReason(statuses[0].Reason) ||
		statuses[0].ActiveGeneration != old.ApplicationGeneration {
		t.Fatalf("startup grace hid steady-state failure: %#v", statuses[0])
	}
}
