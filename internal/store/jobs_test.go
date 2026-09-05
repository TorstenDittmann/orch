package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"change.codes/torsten/better-swarm/internal/model"
)

func TestJobRequestsAreDurableAtomicAndImmutable(t *testing.T) {
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	if _, err := state.EnrollAgent(ctx, testEnrollAgent("node-one", "node-1", strings.Repeat("a", 64), "enrollment-key-one", strings.Repeat("1", 64))); err != nil {
		t.Fatal(err)
	}
	input := applicationInput(`{"name":"hello","services":{"web":{"image":"pinned"}}}`, "revision-one", "application-key-one", testCreatedAt)
	if _, err := state.ApplyApplication(ctx, input); err != nil {
		t.Fatal(err)
	}
	at := testCreatedAt
	template := model.CronJobDesired{Application: "hello", Job: "run-one", RunID: "job_one", RunAt: &at, ApplicationGeneration: 1, RevisionID: "revision-one", Image: "example/web@sha256:" + strings.Repeat("a", 64), CPUMillis: 100, MemoryBytes: 1024, SpecDigest: "digest-one"}
	job := model.Job{ID: template.RunID, NodeID: "node-one", Service: "web", Kind: "manual", Template: template, Status: model.JobRunStatus{ID: template.RunID, Application: "hello", Job: template.Job, SpecDigest: template.SpecDigest, ScheduledAt: at, State: model.JobRunWaiting, UpdatedAt: at}}
	var wg sync.WaitGroup
	results := make([]model.Job, 2)
	for i := range results {
		wg.Go(func() {
			value, err := state.CreateJob(ctx, job, "job-request-one", strings.Repeat("1", 64), 100, 1024)
			if err != nil {
				t.Error(err)
			}
			results[i] = value
		})
	}
	wg.Wait()
	if results[0].ID != "job_one" || results[0].ID != results[1].ID {
		t.Fatal("concurrent replay created different jobs")
	}
	changed := job
	changed.ID = "job_two"
	changed.Template.RunID = changed.ID
	if _, err := state.CreateJob(ctx, changed, "job-request-two", strings.Repeat("2", 64), 100, 1024); !errors.Is(err, ErrJobLimit) {
		t.Fatalf("capacity overcommit: %v", err)
	}
	if _, _, err := state.ReplayJob(ctx, "job-request-one", "other-hash"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("request conflict: %v", err)
	}
	if _, err := state.db.ExecContext(ctx, `UPDATE one_off_jobs SET template='{}' WHERE id='job_one'`); err == nil {
		t.Fatal("job template was mutable")
	}
	if _, err := state.CancelJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	path := state.path
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replay, found, err := reopened.ReplayJob(ctx, "job-request-one", strings.Repeat("1", 64))
	if err != nil || !found || !replay.CancelRequested || replay.Template.Image != template.Image || replay.Status.FinishedAt != nil {
		t.Fatalf("durable cancellation/replay: %+v %v", replay, err)
	}
	tx, err := reopened.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	run := job.Status
	run.ID = "job_forged"
	if err := observeOneOffTx(ctx, tx, "node-one", template, run); err == nil {
		t.Fatal("forged run accepted")
	}
	tx.Rollback()
}
