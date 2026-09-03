package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
)

var stateTestTime = time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)

func TestAssignmentCommitIsDurableMonotonicAndIdentityBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(privateTestDir(t), "agent.db")
	cache, err := Open(ctx, path, "cluster-one", "node-one")
	if err != nil {
		t.Fatal(err)
	}
	first := testAssignment(t, "node-one", 1, testAllocation(t, "sha256:"+strings.Repeat("a", 64)))
	changed, err := cache.CommitAssignment(ctx, first)
	if err != nil || !changed {
		t.Fatalf("CommitAssignment(first) = %v, %v", changed, err)
	}
	changed, err = cache.CommitAssignment(ctx, first)
	if err != nil || changed {
		t.Fatalf("CommitAssignment(duplicate) = %v, %v", changed, err)
	}

	conflict := testAssignment(t, "node-one", 1, testAllocation(t, "sha256:"+strings.Repeat("b", 64)))
	if _, err := cache.CommitAssignment(ctx, conflict); !errors.Is(err, ErrAssignmentConflict) {
		t.Fatalf("same-generation conflict = %v", err)
	}
	second := conflict
	second.Generation = 2
	second.CreatedAt = stateTestTime.Add(time.Minute)
	if changed, err := cache.CommitAssignment(ctx, second); err != nil || !changed {
		t.Fatalf("CommitAssignment(second) = %v, %v", changed, err)
	}
	if _, err := cache.CommitAssignment(ctx, first); !errors.Is(err, ErrStaleAssignment) {
		t.Fatalf("stale assignment = %v", err)
	}
	foreign := second
	foreign.NodeID = "node-other"
	if _, err := cache.CommitAssignment(ctx, foreign); !errors.Is(err, ErrAgentIdentityMismatch) {
		t.Fatalf("foreign assignment = %v", err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, path, "cluster-one", "node-one")
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Assignment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Allocations, second.Allocations) || got.Generation != second.Generation || got.Digest != second.Digest {
		t.Fatalf("reopened assignment = %#v, want %#v", got, second)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, path, "cluster-one", "node-other"); !errors.Is(err, ErrAgentIdentityMismatch) {
		t.Fatalf("identity-mismatched reopen = %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("database info = %#v, %v", info, err)
	}
}

func TestAssignmentRejectsCorruptDigestsWithoutPublishing(t *testing.T) {
	t.Parallel()
	cache := openTestCache(t)
	allocation := testAllocation(t, "sha256:"+strings.Repeat("a", 64))
	allocation.SpecDigest = "sha256:" + strings.Repeat("0", 64)
	assignment := testAssignment(t, "node-one", 1, allocation)
	if _, err := cache.CommitAssignment(context.Background(), assignment); err == nil || !strings.Contains(err.Error(), "spec digest") {
		t.Fatalf("corrupt allocation digest = %v", err)
	}
	if _, err := cache.Assignment(context.Background()); !errors.Is(err, ErrNoAssignment) {
		t.Fatalf("Assignment() after rejection = %v", err)
	}
	assignment = testAssignment(t, "node-one", 1, testAllocation(t, "sha256:"+strings.Repeat("a", 64)))
	assignment.Digest = "sha256:" + strings.Repeat("f", 64)
	if _, err := cache.CommitAssignment(context.Background(), assignment); err == nil || !strings.Contains(err.Error(), "assignment digest") {
		t.Fatalf("corrupt assignment digest = %v", err)
	}
}

func TestAssignmentRejectsNonCanonicalAllocationOrder(t *testing.T) {
	t.Parallel()
	cache := openTestCache(t)
	first := testAllocation(t, "sha256:"+strings.Repeat("a", 64))
	first.ID = "alloc-z"
	second := first
	second.ID = "alloc-a"
	assignment := testAssignment(t, "node-one", 1, first, second)
	if _, err := cache.CommitAssignment(context.Background(), assignment); err == nil || !strings.Contains(err.Error(), "canonical order") {
		t.Fatalf("CommitAssignment() = %v", err)
	}
	if _, err := cache.Assignment(context.Background()); !errors.Is(err, ErrNoAssignment) {
		t.Fatalf("Assignment() after rejection = %v", err)
	}
}

func TestAllocationRestartStateSurvivesReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(privateTestDir(t), "agent.db")
	cache, err := Open(ctx, path, "cluster-one", "node-one")
	if err != nil {
		t.Fatal(err)
	}
	next := stateTestTime.Add(8 * time.Second)
	exit := int32(17)
	want := AllocationState{
		ID: "alloc-one", SpecDigest: "sha256:" + strings.Repeat("a", 64), Phase: model.AllocationBackoff,
		RestartCount: 3, NextAttemptAt: &next, ExitCode: &exit, Reason: "restart_backoff",
		Message: "waiting", UpdatedAt: stateTestTime,
	}
	if err := cache.SaveAllocationState(ctx, want); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	cache, err = Open(ctx, path, "cluster-one", "node-one")
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	states, err := cache.AllocationStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || !reflect.DeepEqual(states[0], want) {
		t.Fatalf("allocation states = %#v, want %#v", states, want)
	}
	if err := cache.DeleteAllocationState(ctx, want.ID); err != nil {
		t.Fatal(err)
	}
	states, err = cache.AllocationStates(ctx)
	if err != nil || len(states) != 0 {
		t.Fatalf("states after delete = %#v, %v", states, err)
	}
}

func TestOpenRejectsUnsafeAndNewerAgentState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("insecure parent", func(t *testing.T) {
		directory := privateTestDir(t)
		if err := os.Chmod(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(ctx, filepath.Join(directory, "agent.db"), "cluster-one", "node-one"); err == nil || !strings.Contains(err.Error(), "private") {
			t.Fatalf("Open() = %v", err)
		}
	})
	t.Run("insecure file", func(t *testing.T) {
		path := filepath.Join(privateTestDir(t), "agent.db")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(ctx, path, "cluster-one", "node-one"); err == nil || !strings.Contains(err.Error(), "private regular") {
			t.Fatalf("Open() = %v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		directory := privateTestDir(t)
		target := filepath.Join(directory, "target")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "agent.db")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(ctx, path, "cluster-one", "node-one"); err == nil {
			t.Fatal("Open() accepted a symlink database")
		}
	})
	t.Run("insecure WAL sidecar", func(t *testing.T) {
		path := filepath.Join(privateTestDir(t), "agent.db")
		if err := os.WriteFile(path+"-wal", nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(ctx, path, "cluster-one", "node-one"); err == nil || !strings.Contains(err.Error(), "sidecar") {
			t.Fatalf("Open() = %v", err)
		}
	})
	t.Run("symlink SHM sidecar", func(t *testing.T) {
		directory := privateTestDir(t)
		path := filepath.Join(directory, "agent.db")
		target := filepath.Join(directory, "target")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path+"-shm"); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(ctx, path, "cluster-one", "node-one"); err == nil || !strings.Contains(err.Error(), "sidecar") {
			t.Fatalf("Open() = %v", err)
		}
	})
	t.Run("newer migration", func(t *testing.T) {
		path := filepath.Join(privateTestDir(t), "agent.db")
		cache, err := Open(ctx, path, "cluster-one", "node-one")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cache.db.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, checksum) VALUES (2, 'future.sql', 'future')`); err != nil {
			t.Fatal(err)
		}
		if err := cache.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(ctx, path, "cluster-one", "node-one"); err == nil || !strings.Contains(err.Error(), "newer") {
			t.Fatalf("Open() = %v", err)
		}
	})
}

func openTestCache(t *testing.T) *Store {
	t.Helper()
	cache, err := Open(context.Background(), filepath.Join(privateTestDir(t), "agent.db"), "cluster-one", "node-one")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	return cache
}

func privateTestDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func testAllocation(t *testing.T, image string) model.AllocationDesired {
	t.Helper()
	allocation := model.AllocationDesired{
		ID: "alloc-one", Application: "hello", Service: "worker", Replica: 1,
		ApplicationGeneration: 1, RevisionID: "revision-one", Image: image,
		Command: []string{"/bin/echo"}, Args: []string{"hello"}, Environment: map[string]string{"MODE": "test"},
		RestartPolicy: "always", CPUMillis: 100, MemoryBytes: 128 << 20,
		StartDeadlineSeconds: 30, AutoRollback: true, StartBeforeStop: true,
	}
	var err error
	allocation.SpecDigest, err = model.AllocationSpecDigest(allocation)
	if err != nil {
		t.Fatal(err)
	}
	return allocation
}

func testAssignment(t *testing.T, nodeID string, generation uint64, allocations ...model.AllocationDesired) model.Assignment {
	t.Helper()
	payload, err := json.Marshal(allocations)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	return model.Assignment{
		NodeID: nodeID, Generation: generation, Digest: "sha256:" + hex.EncodeToString(digest[:]),
		Payload: payload, Allocations: allocations, CreatedAt: stateTestTime,
	}
}
