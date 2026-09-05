package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
)

func TestApplyApplicationCreatesUpdatesReplaysAndNoOps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}

	firstInput := applicationInput(`{"name":"hello","image":"example/web:1"}`, "revision-one", "idempotency-key-one", testCreatedAt)
	first, err := state.ApplyApplication(ctx, firstInput)
	if err != nil {
		t.Fatal(err)
	}
	if first.Generation != 1 || !first.Changed || first.EventSequence != 2 || first.RevisionID != "revision-one" {
		t.Fatalf("first apply = %#v", first)
	}
	stored, err := state.Application(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Generation != 1 || stored.RevisionID != "revision-one" || stored.Digest != firstInput.Digest || string(stored.CanonicalManifest) != string(firstInput.CanonicalManifest) {
		t.Fatalf("stored application = %#v", stored)
	}

	replayed, err := state.ApplyApplication(ctx, firstInput)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed, first) {
		t.Fatalf("replay = %#v, want %#v", replayed, first)
	}

	conflict := applicationInput(`{"name":"hello","image":"example/web:2"}`, "unused", firstInput.IdempotencyKey, testCreatedAt.Add(time.Minute))
	if _, err := state.ApplyApplication(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting apply = %v, want ErrIdempotencyConflict", err)
	}

	noOpInput := firstInput
	noOpInput.RevisionID = "unused-no-op-revision"
	noOpInput.IdempotencyKey = "idempotency-key-no-op"
	noOpInput.CreatedAt = testCreatedAt.Add(2 * time.Minute)
	noOp, err := state.ApplyApplication(ctx, noOpInput)
	if err != nil {
		t.Fatal(err)
	}
	if noOp.Changed || noOp.Generation != 1 || noOp.RevisionID != first.RevisionID || noOp.EventSequence != 0 || noOp.UpdatedAt != first.UpdatedAt {
		t.Fatalf("no-op apply = %#v", noOp)
	}

	secondInput := conflict
	secondInput.IdempotencyKey = "idempotency-key-two"
	secondInput.RevisionID = "revision-two"
	secondInput.CreatedAt = testCreatedAt.Add(3 * time.Minute)
	second, err := state.ApplyApplication(ctx, secondInput)
	if err != nil {
		t.Fatal(err)
	}
	if second.Generation != 2 || !second.Changed || second.EventSequence != 3 || second.CreatedAt != first.CreatedAt || second.UpdatedAt != secondInput.CreatedAt {
		t.Fatalf("second apply = %#v", second)
	}

	events, err := state.EventsAfter(ctx, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != model.EventApplicationDesiredUpdated || events[1].Sequence != 3 {
		t.Fatalf("application events = %#v", events)
	}
	var revisions, replayRecords int
	if err := state.db.QueryRowContext(ctx, "SELECT count(*) FROM application_revisions").Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if err := state.db.QueryRowContext(ctx, "SELECT count(*) FROM idempotency_keys").Scan(&replayRecords); err != nil {
		t.Fatal(err)
	}
	if revisions != 2 || replayRecords != 3 {
		t.Fatalf("revision count = %d, replay count = %d", revisions, replayRecords)
	}
}

func TestApplyApplicationRollsBackEveryWriteBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		trigger string
		needle  string
	}{
		{name: "application", trigger: `CREATE TRIGGER reject_application BEFORE INSERT ON applications BEGIN SELECT RAISE(ABORT, 'application rejected'); END`, needle: "application rejected"},
		{name: "revision", trigger: `CREATE TRIGGER reject_revision BEFORE INSERT ON application_revisions BEGIN SELECT RAISE(ABORT, 'revision rejected'); END`, needle: "revision rejected"},
		{name: "event", trigger: `CREATE TRIGGER reject_application_event BEFORE INSERT ON events WHEN NEW.kind = 'application.desired.updated' BEGIN SELECT RAISE(ABORT, 'event rejected'); END`, needle: "event rejected"},
		{name: "idempotency", trigger: `CREATE TRIGGER reject_idempotency BEFORE INSERT ON idempotency_keys BEGIN SELECT RAISE(ABORT, 'idempotency rejected'); END`, needle: "idempotency rejected"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			state := openTestStore(t)
			if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
				t.Fatal(err)
			}
			if _, err := state.db.ExecContext(ctx, test.trigger); err != nil {
				t.Fatal(err)
			}
			_, err := state.ApplyApplication(ctx, applicationInput(`{"name":"hello"}`, "revision-one", "idempotency-key-one", testCreatedAt))
			if err == nil || !strings.Contains(err.Error(), test.needle) {
				t.Fatalf("ApplyApplication() = %v, want %q", err, test.needle)
			}
			if _, err := state.Application(ctx, "hello"); !errors.Is(err, ErrApplicationNotFound) {
				t.Fatalf("Application() = %v, want ErrApplicationNotFound", err)
			}
			for table, want := range map[string]int{"application_revisions": 0, "idempotency_keys": 0, "events": 1} {
				var got int
				if err := state.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Fatalf("%s rows = %d, want %d", table, got, want)
				}
			}
		})
	}
}

func TestApplyApplicationConcurrentReplayCreatesOneRevision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	input := applicationInput(`{"name":"hello"}`, "revision-one", "idempotency-key-one", testCreatedAt)
	const workers = 8
	results := make(chan model.ApplyApplicationResult, workers)
	errorsSeen := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := state.ApplyApplication(ctx, input)
			if err != nil {
				errorsSeen <- err
				return
			}
			results <- result
		}()
	}
	wait.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Errorf("ApplyApplication() = %v", err)
	}
	var first *model.ApplyApplicationResult
	for result := range results {
		if first == nil {
			copy := result
			first = &copy
		} else if !reflect.DeepEqual(result, *first) {
			t.Errorf("concurrent replay = %#v, want %#v", result, *first)
		}
	}
	var revisions int
	if err := state.db.QueryRowContext(ctx, "SELECT count(*) FROM application_revisions").Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if revisions != 1 {
		t.Fatalf("revision count = %d, want 1", revisions)
	}
}

func TestApplicationIntentSurvivesReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "control.db")
	state, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	input := applicationInput(`{"name":"hello"}`, "revision-one", "idempotency-key-one", testCreatedAt)
	want, err := state.ApplyApplication(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenInitialized(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.ApplyApplication(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened replay = %#v, want %#v", got, want)
	}
}

func TestApplicationRevisionsAreImmutable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	if _, err := state.ApplyApplication(ctx, applicationInput(`{"name":"hello"}`, "revision-one", "idempotency-key-one", testCreatedAt)); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"UPDATE application_revisions SET digest = digest WHERE id = 'revision-one'",
		"DELETE FROM application_revisions WHERE id = 'revision-one'",
	} {
		if _, err := state.db.ExecContext(ctx, statement); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("revision mutation %q = %v, want immutable error", statement, err)
		}
	}
}

func TestEventsAfterUsesExclusiveCursor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	if _, err := state.ApplyApplication(ctx, applicationInput(`{"name":"hello"}`, "revision-one", "idempotency-key-one", testCreatedAt)); err != nil {
		t.Fatal(err)
	}
	events, err := state.EventsAfter(ctx, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Sequence != 2 {
		t.Fatalf("EventsAfter() = %#v", events)
	}
	recent, err := state.RecentEvents(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].Sequence != 2 {
		t.Fatalf("RecentEvents(1) = %#v", recent)
	}
	recent, err = state.RecentEvents(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 || recent[0].Sequence != 1 || recent[1].Sequence != 2 {
		t.Fatalf("RecentEvents(2) = %#v", recent)
	}
	if _, err := state.RecentEvents(ctx, 0); err == nil {
		t.Fatal("RecentEvents() accepted a zero limit")
	}
	if _, err := state.EventsAfter(ctx, -1, 1); err == nil {
		t.Fatal("EventsAfter() accepted a negative cursor")
	}
}

func applicationInput(manifest, revisionID, key string, now time.Time) model.ApplyApplication {
	canonical := []byte(manifest)
	sum := sha256.Sum256(canonical)
	return model.ApplyApplication{
		Name: "hello", RevisionID: revisionID,
		Digest: "sha256:" + hex.EncodeToString(sum[:]), CanonicalManifest: canonical,
		IdempotencyKey: key, CreatedAt: now,
	}
}

func TestApplyApplicationValidatesInput(t *testing.T) {
	t.Parallel()
	state := openTestStore(t)
	input := applicationInput(`{"name":"hello"}`, "revision-one", "idempotency-key-one", testCreatedAt)
	input.Digest = fmt.Sprintf("sha256:%064d", 0)
	if _, err := state.ApplyApplication(context.Background(), input); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("ApplyApplication() = %v, want digest mismatch", err)
	}
}

func TestApplyApplicationRejectsOneHundredFirstActiveApplication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 100; index++ {
		name := fmt.Sprintf("app-%03d", index)
		input := applicationInput(fmt.Sprintf(`{"name":%q}`, name), fmt.Sprintf("revision-%03d", index),
			fmt.Sprintf("application-key-%03d", index), testCreatedAt)
		input.Name = name
		if _, err := state.ApplyApplication(ctx, input); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	overflow := applicationInput(`{"name":"overflow"}`, "revision-overflow", "application-key-overflow", testCreatedAt)
	overflow.Name = "overflow"
	if _, err := state.ApplyApplication(ctx, overflow); !errors.Is(err, ErrApplicationLimit) {
		t.Fatalf("one-hundred-first application = %v", err)
	}
}

func TestApplyApplicationRejectsMoreThanTwoHundredClusterAllocations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	firstManifest := `{"name":"first","services":{"web":{"replicas":100},"worker":{"replicas":100}}}`
	first := applicationInput(firstManifest, "allocation-limit-first", "allocation-limit-key-first", testCreatedAt)
	first.Name = "first"
	if _, err := state.ApplyApplication(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := applicationInput(`{"name":"second","services":{"web":{"replicas":1}}}`,
		"allocation-limit-second", "allocation-limit-key-second", testCreatedAt)
	second.Name = "second"
	if _, err := state.ApplyApplication(ctx, second); !errors.Is(err, ErrAllocationLimit) {
		t.Fatalf("two-hundred-first allocation = %v", err)
	}
}

func TestRequestedImagesPersistAcrossRestartAndNoOp(t *testing.T) {
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	requested := []byte(`{"name":"hello","image":"example/web:latest"}`)
	input := applicationInput(`{"name":"hello","image":"example/web@sha256:first"}`, "revision-one", "image-request-one", testCreatedAt)
	input.RequestedManifest = requested
	first, err := state.ApplyApplication(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(requested)
	if first.Digest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatal("receipt did not hash authored request")
	}
	path := state.path
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	replay, found, err := state.ReplayApplication(ctx, input.IdempotencyKey, requested)
	if err != nil || !found || !reflect.DeepEqual(replay, first) {
		t.Fatalf("replay after reopen: %v %v", found, err)
	}
	d, err := state.Deployment(ctx, "hello", first.RevisionID)
	if err != nil || string(d.RequestedManifest) != string(requested) || string(d.CanonicalManifest) != string(input.CanonicalManifest) || d.Digest != input.Digest {
		t.Fatalf("durable image metadata: %+v %v", d, err)
	}
	if _, err := state.db.ExecContext(ctx, `UPDATE application_revisions SET requested_manifest='{}' WHERE id=?`, first.RevisionID); err == nil {
		t.Fatal("requested metadata was mutable")
	}
	input.IdempotencyKey = "image-request-two"
	input.RevisionID = "revision-two"
	noop, err := state.ApplyApplication(ctx, input)
	if err != nil || noop.Changed || noop.Generation != 1 || noop.Digest != first.Digest {
		t.Fatalf("same tag/digest no-op: %+v %v", noop, err)
	}
	input.IdempotencyKey = "image-request-three"
	input.RevisionID = "revision-three"
	input.RequestedManifest = []byte(`{"name":"hello","image":"example/web:stable"}`)
	next, err := state.ApplyApplication(ctx, input)
	if err != nil || !next.Changed || next.Generation != 2 {
		t.Fatalf("requested reference change lost: %+v %v", next, err)
	}
}
