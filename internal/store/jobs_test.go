package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
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

func TestJobCapacityCountsRollbackOnlyWhileItCanCoexist(t *testing.T) {
	// A running allocation of 400 whose previous revision, kept for rollback,
	// reserved 600. On a node of 1000 a job of 500 fits only when the rollback
	// process cannot run next to the current one.
	for _, test := range []struct {
		name            string
		startBeforeStop bool
		activeRevision  string
		activeDigest    string
		fits            bool
	}{
		{name: "start-before-stop rollout in progress", startBeforeStop: true, activeRevision: "revision-zero", activeDigest: "sha256:old", fits: false},
		{name: "start-before-stop rollout not yet observed", startBeforeStop: true, fits: false},
		{name: "start-before-stop rollout settled", startBeforeStop: true, activeRevision: "revision-one", activeDigest: "sha256:" + strings.Repeat("b", 64), fits: true},
		{name: "stop-first", startBeforeStop: false, activeRevision: "revision-zero", activeDigest: "sha256:old", fits: true},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			digest := "sha256:" + strings.Repeat("b", 64)
			spec := fmt.Sprintf(`{"id":"alloc-web","revision_id":"revision-one","spec_digest":%q,"cpu_millis":400,"memory_bytes":400,"start_before_stop":%t,"rollback":{"revision_id":"revision-zero","spec_digest":"sha256:old","cpu_millis":600,"memory_bytes":600}}`, digest, test.startBeforeStop)
			if _, err := state.db.ExecContext(ctx, `INSERT INTO desired_allocations (id, node_id, application_name, service_name, replica, application_generation, revision_id, spec_digest, spec, assignment_generation)
				VALUES ('alloc-web', 'node-one', 'hello', 'web', 1, 1, 'revision-one', ?, ?, 1)`, digest, spec); err != nil {
				t.Fatal(err)
			}
			if test.activeRevision != "" {
				if _, err := state.db.ExecContext(ctx, `INSERT INTO allocation_observations (id, node_id, application_name, service_name, replica, application_generation, spec_digest, phase, reason, message, restart_count, updated_at, active_revision_id, active_spec_digest)
					VALUES ('alloc-web', 'node-one', 'hello', 'web', 1, 1, ?, 'RUNNING', '', '', 0, ?, ?, ?)`, digest, testCreatedAt.Format(time.RFC3339Nano), test.activeRevision, test.activeDigest); err != nil {
					t.Fatal(err)
				}
			}
			at := testCreatedAt
			template := model.CronJobDesired{Application: "hello", Job: "run-one", RunID: "job_one", RunAt: &at, ApplicationGeneration: 1, RevisionID: "revision-one", Image: "example/web@sha256:" + strings.Repeat("a", 64), CPUMillis: 500, MemoryBytes: 500, SpecDigest: "digest-one"}
			job := model.Job{ID: template.RunID, NodeID: "node-one", Service: "web", Kind: "manual", Template: template, Status: model.JobRunStatus{ID: template.RunID, Application: "hello", Job: template.Job, SpecDigest: template.SpecDigest, ScheduledAt: at, State: model.JobRunWaiting, UpdatedAt: at}}
			_, err := state.CreateJob(ctx, job, "job-request-one", strings.Repeat("3", 64), 1000, 1000)
			if test.fits && err != nil {
				t.Fatalf("job did not fit: %v", err)
			}
			if !test.fits && !errors.Is(err, ErrJobLimit) {
				t.Fatalf("rollback capacity was not reserved: %v", err)
			}

			cpu, memory, err := state.NodeAssignedResources(ctx, "node-one")
			if err != nil {
				t.Fatal(err)
			}
			want := uint64(400)
			if !test.fits {
				want = 1000
			}
			if cpu != want || memory != want {
				t.Fatalf("NodeAssignedResources() = %d, %d; want %d", cpu, memory, want)
			}
		})
	}
}
