package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/network"
	"change.codes/torsten/better-swarm/internal/security"
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

func TestAssignmentSecretMaterialsAreEncryptedAndRecoverable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	directory := privateTestDir(t)
	path := filepath.Join(directory, "agent.db")
	cache, err := Open(ctx, path, "cluster-one", "node-one")
	if err != nil {
		t.Fatal(err)
	}
	box, err := security.NewSecretBox(bytes.Repeat([]byte{5}, security.SecretKeySize), bytes.NewReader(bytes.Repeat([]byte{6}, 1024)))
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.ConfigureSecretBox(ctx, box); err != nil {
		t.Fatal(err)
	}
	allocation := testAllocation(t, "docker.io/library/busybox@sha256:"+strings.Repeat("a", 64))
	allocation.SecretEnvironment = map[string]model.SecretReference{"TOKEN": {Name: "token", VersionID: "secv_one"}}
	allocation.SpecDigest, err = model.AllocationSpecDigest(allocation)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("agent-only-sensitive-value")
	assignment := testAssignment(t, "node-one", 1, allocation)
	assignment.Materials = []model.SecretMaterial{{Kind: model.SecretMaterialApplication, Application: "hello", Name: "token", VersionID: "secv_one", Value: plaintext}}
	if changed, err := cache.CommitAssignment(ctx, assignment); err != nil || !changed {
		t.Fatalf("CommitAssignment() = %t, %v", changed, err)
	}
	conflicting := assignment
	conflicting.Materials = []model.SecretMaterial{{Kind: model.SecretMaterialApplication, Application: "hello", Name: "token", VersionID: "secv_one", Value: []byte("substituted")}}
	if _, err := cache.CommitAssignment(ctx, conflicting); !errors.Is(err, ErrAssignmentConflict) {
		t.Fatalf("same-version material substitution = %v", err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		contents, readErr := os.ReadFile(path + suffix)
		if readErr == nil && bytes.Contains(contents, plaintext) {
			t.Fatalf("plaintext secret appears in agent database%s", suffix)
		}
	}
	reopened, err := Open(ctx, path, "cluster-one", "node-one")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.ConfigureSecretBox(ctx, box); err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Assignment(ctx)
	if err != nil || len(got.Materials) != 1 || !bytes.Equal(got.Materials[0].Value, plaintext) {
		t.Fatalf("recovered assignment material = %#v, %v", got.Materials, err)
	}
	wrong, _ := security.NewSecretBox(bytes.Repeat([]byte{8}, security.SecretKeySize), nil)
	if err := reopened.ConfigureSecretBox(ctx, wrong); err == nil {
		t.Fatal("ConfigureSecretBox() accepted a mismatched key")
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

func TestAssignmentCommitPreservesPlan4AllocationDuringNetworkPreflight(t *testing.T) {
	t.Parallel()
	cache := openTestCache(t)
	allocation := testAllocation(t, "sha256:"+strings.Repeat("a", 64))
	var err error
	allocation.SpecDigest, err = model.Plan4AllocationSpecDigest(allocation)
	if err != nil {
		t.Fatal(err)
	}
	network, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	payload, digest, err := model.AssignmentPayload(network, []model.AllocationDesired{allocation})
	if err != nil {
		t.Fatal(err)
	}
	assignment := model.Assignment{NodeID: "node-one", Generation: 2, Digest: digest, Payload: payload,
		Allocations: []model.AllocationDesired{allocation}, Network: network, CreatedAt: stateTestTime}
	if changed, err := cache.CommitAssignment(context.Background(), assignment); err != nil || !changed {
		t.Fatalf("CommitAssignment(legacy preflight) = %t, %v", changed, err)
	}
	got, err := cache.Assignment(context.Background())
	if err != nil || len(got.Allocations) != 1 || got.Allocations[0].SpecDigest != allocation.SpecDigest || !model.NetworkConfigEqual(got.Network, network) {
		t.Fatalf("Assignment(legacy preflight) = %#v, %v", got, err)
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
		Message: "waiting", UpdatedAt: stateTestTime, HealthStatus: model.HealthNotConfigured,
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
		if _, err := cache.db.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, checksum) VALUES (5, 'future.sql', 'future')`); err != nil {
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

func TestVersionOneAgentStateMigratesWithoutIdentityOrRuntimeLoss(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(privateTestDir(t), "agent.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	contents, err := fs.ReadFile(migrationFiles, "migrations/001_agent_core.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(contents)); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, checksum)
		VALUES (1, '001_agent_core.sql', ?)`, hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	allocation := testAllocation(t, "sha256:"+strings.Repeat("a", 64))
	assignment := testAssignment(t, "node-one", 1, allocation)
	encodedAllocation, err := json.Marshal(allocation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO agent_identity(singleton, cluster_id, node_id)
		VALUES (1, 'cluster-one', 'node-one')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO assignment(singleton, generation, digest, payload, created_at)
		VALUES (1, 1, ?, ?, ?)`, assignment.Digest, string(assignment.Payload), stateTestTime.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO desired_allocations(id, spec_digest, spec)
		VALUES (?, ?, ?)`, allocation.ID, allocation.SpecDigest, string(encodedAllocation)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO allocation_state
		(id, spec_digest, phase, restart_count, reason, message, updated_at)
		VALUES (?, ?, 'RUNNING', 2, 'process_running', 'running', ?)`, allocation.ID,
		allocation.SpecDigest, stateTestTime.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	cache, err := Open(ctx, path, "cluster-one", "node-one")
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	gotAssignment, err := cache.Assignment(ctx)
	if err != nil || gotAssignment.Generation != 1 || len(gotAssignment.Allocations) != 1 ||
		gotAssignment.Allocations[0].SpecDigest != allocation.SpecDigest {
		t.Fatalf("migrated assignment = %#v, %v", gotAssignment, err)
	}
	states, err := cache.AllocationStates(ctx)
	if err != nil || len(states) != 1 {
		t.Fatalf("migrated allocation states = %#v, %v", states, err)
	}
	if states[0].Phase != model.AllocationRunning || states[0].RestartCount != 2 ||
		states[0].HealthStatus != model.HealthNotConfigured || states[0].SpecDigest != allocation.SpecDigest {
		t.Fatalf("migrated allocation state = %#v", states[0])
	}
	var migrations int
	if err := cache.db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if migrations != 4 {
		t.Fatalf("migration count = %d, want 4", migrations)
	}
}

func TestNetworkAttachmentsAreDurableAndOwnershipBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cache := openTestCache(t)
	attachment := network.Attachment{
		AllocationID: "alloc-one", ObjectID: "object-one", SpecDigest: "sha256:" + strings.Repeat("a", 64),
		NetworkGeneration: 1, Address: netip.MustParseAddr("10.88.0.2"), NetNS: "/proc/123/ns/net", CreatedAt: stateTestTime,
	}
	if err := cache.SaveAttachment(ctx, attachment); err != nil {
		t.Fatal(err)
	}
	if err := cache.SaveAttachment(ctx, attachment); err != nil {
		t.Fatalf("idempotent SaveAttachment() = %v", err)
	}
	mismatch := attachment
	mismatch.SpecDigest = "sha256:" + strings.Repeat("b", 64)
	if err := cache.SaveAttachment(ctx, mismatch); !errors.Is(err, network.ErrOwnershipMismatch) {
		t.Fatalf("mismatched SaveAttachment() = %v", err)
	}
	stored, err := cache.Attachment(ctx, attachment.ObjectID)
	if err != nil || stored.AllocationID != attachment.AllocationID || stored.Address != attachment.Address {
		t.Fatalf("Attachment() = %#v, %v", stored, err)
	}
	all, err := cache.Attachments(ctx)
	if err != nil || len(all) != 1 || all[0].ObjectID != attachment.ObjectID {
		t.Fatalf("Attachments() = %#v, %v", all, err)
	}
	if err := cache.DeleteAttachment(ctx, attachment.ObjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Attachment(ctx, attachment.ObjectID); !errors.Is(err, network.ErrNotFound) {
		t.Fatalf("Attachment() after delete = %v", err)
	}
}

func TestServingSnapshotAndDrainAreDurableAndAtomic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(privateTestDir(t), "agent.db")
	cache, err := Open(ctx, path, "cluster-one", "node-one")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := ServingEndpoint{
		AllocationID: "alloc-one", Application: "hello", Service: "web", Address: "10.88.0.2",
		SpecDigest: "sha256:" + strings.Repeat("a", 64), UpdatedAt: stateTestTime,
	}
	if err := cache.ReplaceServingEndpoints(ctx, []ServingEndpoint{endpoint}); err != nil {
		t.Fatal(err)
	}
	replacement := endpoint
	replacement.Address = "10.88.0.3"
	replacement.SpecDigest = "sha256:" + strings.Repeat("b", 64)
	replacement.UpdatedAt = stateTestTime.Add(time.Second)
	if err := cache.StageServingEndpoints(ctx, []ServingEndpoint{replacement}); err != nil {
		t.Fatal(err)
	}
	if endpoints, err := cache.ServingEndpoints(ctx); err != nil || !reflect.DeepEqual(endpoints, []ServingEndpoint{endpoint}) {
		t.Fatalf("accepted snapshot changed while staged = %#v, %v", endpoints, err)
	}
	if err := cache.CommitServingEndpoints(ctx); err != nil {
		t.Fatal(err)
	}
	until := stateTestTime.Add(5 * time.Second)
	drain := Drain{ObjectID: "object-one", AllocationID: endpoint.AllocationID, SpecDigest: endpoint.SpecDigest, Until: until}
	gotUntil, err := cache.BeginDrain(ctx, drain)
	if err != nil || !gotUntil.Equal(until) {
		t.Fatalf("BeginDrain() = %v, %v", gotUntil, err)
	}
	extended := drain
	extended.Until = until.Add(time.Hour)
	gotUntil, err = cache.BeginDrain(ctx, extended)
	if err != nil || !gotUntil.Equal(until) {
		t.Fatalf("idempotent BeginDrain() = %v, %v", gotUntil, err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	cache, err = Open(ctx, path, "cluster-one", "node-one")
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	endpoints, err := cache.ServingEndpoints(ctx)
	if err != nil || !reflect.DeepEqual(endpoints, []ServingEndpoint{replacement}) {
		t.Fatalf("ServingEndpoints() = %#v, %v", endpoints, err)
	}
	drains, err := cache.Drains(ctx)
	if err != nil || !reflect.DeepEqual(drains, []Drain{drain}) {
		t.Fatalf("Drains() = %#v, %v", drains, err)
	}
	if err := cache.ReplaceServingEndpoints(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if endpoints, err := cache.ServingEndpoints(ctx); err != nil || len(endpoints) != 0 {
		t.Fatalf("cleared ServingEndpoints() = %#v, %v", endpoints, err)
	}
	if err := cache.DeleteDrain(ctx, drain.ObjectID); err != nil {
		t.Fatal(err)
	}
	if drains, err := cache.Drains(ctx); err != nil || len(drains) != 0 {
		t.Fatalf("cleared Drains() = %#v, %v", drains, err)
	}
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
