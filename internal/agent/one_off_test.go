package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"change.codes/torsten/better-swarm/internal/agent/state"
	"change.codes/torsten/better-swarm/internal/discovery"
	"change.codes/torsten/better-swarm/internal/ingress"
	"change.codes/torsten/better-swarm/internal/model"
	orchruntime "change.codes/torsten/better-swarm/internal/runtime"
)

func TestOneOffRunCompletionCancellationAndRestart(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "cancel", "timeout", "uncertain"} {
		t.Run(outcome, func(t *testing.T) {
			ctx := context.Background()
			cache := supervisorCache(t)
			runtime := orchruntime.NewFake()
			now := supervisorTestTime
			newSupervisor := func() *Supervisor {
				return NewSupervisorWithServices(cache, runtime, func() time.Time { return now }, SupervisorServices{Discovery: discovery.NewFake(), Ingress: &ingress.Fake{}})
			}
			supervisor := newSupervisor()
			network, _ := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
			job := model.CronJobDesired{Application: "hello", Job: "run-test", RunID: "job_test", RunAt: &now, ApplicationGeneration: 1, RevisionID: "revision-one", Image: supervisorExecutableImage, Command: []string{"/bin/true"}, CPUMillis: 100, MemoryBytes: 64 << 20, StartDeadlineSeconds: 60, TimeoutSeconds: 10, NetworkGeneration: 1}
			job.SpecDigest, _ = model.CronJobSpecDigest(job)
			if outcome == "uncertain" {
				if err := cache.SaveCronRunState(ctx, state.CronRunState{JobRunStatus: model.JobRunStatus{ID: job.RunID, Application: job.Application, Job: job.Job, SpecDigest: job.SpecDigest, ScheduledAt: now, State: model.JobRunRunning, StartedAt: &now, UpdatedAt: now}}); err != nil {
					t.Fatal(err)
				}
			}
			assignment := model.Assignment{Network: network, CronJobs: []model.CronJobDesired{job}}
			if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
				t.Fatal(err)
			}
			_, runs := supervisor.CronObservations()
			if len(runs) != 1 {
				t.Fatalf("runs: %+v", runs)
			}
			objectID := ""
			for id, object := range runtime.SnapshotObjects() {
				if object.AllocationID == job.RunID {
					objectID = id
				}
			}
			if outcome != "uncertain" && objectID == "" {
				t.Fatal("job did not start")
			}
			switch outcome {
			case "success":
				if err := runtime.Exit(objectID, 0); err != nil {
					t.Fatal(err)
				}
			case "failure":
				if err := runtime.Exit(objectID, 42); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				assignment.CronJobs[0].CancelRequested = true
			case "timeout":
				now = now.Add(11 * time.Second)
			}
			// Recover a durable start without starting it again, including an already exited process.
			supervisor = newSupervisor()
			if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
				t.Fatal(err)
			}
			_, runs = supervisor.CronObservations()
			run := runs[0]
			if run.FinishedAt == nil {
				t.Fatalf("run did not finish: %+v", run)
			}
			if outcome == "success" && (run.State != model.JobRunSucceeded || run.ExitCode == nil || *run.ExitCode != 0) {
				t.Fatalf("success: %+v", run)
			}
			if outcome == "failure" && (run.ExitCode == nil || *run.ExitCode != 42) {
				t.Fatalf("exit code: %+v", run)
			}
			if outcome == "cancel" && run.Reason != "cancelled" {
				t.Fatalf("cancel: %+v", run)
			}
			if outcome == "timeout" && run.State != model.JobRunTimedOut {
				t.Fatalf("timeout: %+v", run)
			}
			if outcome == "uncertain" && run.Reason != "start_outcome_unknown" {
				t.Fatalf("uncertain: %+v", run)
			}
			for range 3 {
				if _, err := supervisor.Reconcile(ctx, assignment); err != nil {
					t.Fatal(err)
				}
			}
			starts := 0
			for _, op := range runtime.SnapshotOperations() {
				if strings.HasPrefix(op, "ensure:") {
					starts++
				}
			}
			want := 1
			if outcome == "uncertain" {
				want = 0
			}
			if starts != want {
				t.Fatalf("process starts=%d want=%d", starts, want)
			}
		})
	}
}
