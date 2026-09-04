package containerd

import (
	"fmt"
	"strings"
	"testing"

	"change.codes/torsten/better-swarm/internal/model"
)

func TestValidateAllocationFailsClosedAtRuntimeBoundary(t *testing.T) {
	t.Parallel()
	valid := runtimeTestAllocation(t)
	if err := validateAllocation(valid); err != nil {
		t.Fatalf("valid allocation: %v", err)
	}
	tests := []struct {
		name string
		want string
		edit func(*model.AllocationDesired)
	}{
		{name: "application path", want: "incomplete", edit: func(value *model.AllocationDesired) { value.Application = "../escape" }},
		{name: "opaque id path", want: "incomplete", edit: func(value *model.AllocationDesired) { value.ID = "../escape" }},
		{name: "service label", want: "incomplete", edit: func(value *model.AllocationDesired) { value.Service = "Worker" }},
		{name: "replica", want: "incomplete", edit: func(value *model.AllocationDesired) { value.Replica = 0 }},
		{name: "cpu overflow", want: "incomplete", edit: func(value *model.AllocationDesired) { value.CPUMillis = uint64(1<<63-1)/100 + 1 }},
		{name: "memory overflow", want: "incomplete", edit: func(value *model.AllocationDesired) { value.MemoryBytes = uint64(1 << 63) }},
		{name: "image tag", want: "digest-qualified", edit: func(value *model.AllocationDesired) { value.Image = "docker.io/library/busybox:latest" }},
		{name: "restart policy", want: "restart policy", edit: func(value *model.AllocationDesired) { value.RestartPolicy = "sometimes" }},
		{name: "empty command", want: "command", edit: func(value *model.AllocationDesired) { value.Command = []string{""} }},
		{name: "oversized argument", want: "bounded", edit: func(value *model.AllocationDesired) {
			value.Args = []string{strings.Repeat("x", (4<<10)+1)}
		}},
		{name: "too many environment entries", want: "too many", edit: func(value *model.AllocationDesired) {
			value.Environment = make(map[string]string, 257)
			for index := 0; index <= 256; index++ {
				value.Environment[fmt.Sprintf("KEY_%d", index)] = "value"
			}
		}},
		{name: "environment", want: "environment", edit: func(value *model.AllocationDesired) { value.Environment = map[string]string{"BAD=KEY": "value"} }},
		{name: "environment syntax", want: "environment", edit: func(value *model.AllocationDesired) { value.Environment = map[string]string{"BAD-NAME": "value"} }},
		{name: "volume name", want: "volume mount", edit: func(value *model.AllocationDesired) {
			value.Mounts = []model.VolumeMount{{Name: "../data", Target: "/data"}}
		}},
		{name: "protected mount", want: "volume mount", edit: func(value *model.AllocationDesired) {
			value.Mounts = []model.VolumeMount{{Name: "data", Target: "/proc/data"}}
		}},
		{name: "resolver mount", want: "volume mount", edit: func(value *model.AllocationDesired) {
			value.Mounts = []model.VolumeMount{{Name: "data", Target: "/etc/resolv.conf"}}
		}},
		{name: "duplicate mount", want: "duplicated", edit: func(value *model.AllocationDesired) {
			value.Mounts = []model.VolumeMount{{Name: "data", Target: "/data"}, {Name: "cache", Target: "/data"}}
		}},
		{name: "too many domains", want: "too many domains", edit: func(value *model.AllocationDesired) {
			value.NetworkGeneration = 1
			value.HTTP = &model.HTTPDesired{Port: 8080, HealthPath: "/healthz", Domains: make([]string, 101)}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.edit(&candidate)
			candidate.SpecDigest = runtimeTestDigest(t, candidate)
			if err := validateAllocation(candidate); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateAllocation() = %v, want error containing %q", err, test.want)
			}
		})
	}

	mismatch := valid
	mismatch.Args = []string{"changed"}
	if err := validateAllocation(mismatch); err == nil || !strings.Contains(err.Error(), "spec digest") {
		t.Fatalf("spec mismatch = %v", err)
	}
}

func TestContainerdRuntimeVersionIsCanonical(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"2.3.4", "v2.3.4"} {
		if got := containerdRuntimeVersion(version); got != "containerd 2.3.4" {
			t.Fatalf("containerdRuntimeVersion(%q) = %q", version, got)
		}
	}
}

func TestValidateAllocationAcceptsDigestPinnedPrivateRegistry(t *testing.T) {
	t.Parallel()
	allocation := runtimeTestAllocation(t)
	allocation.Image = "registry.example:5443/team/app@sha256:" + strings.Repeat("b", 64)
	allocation.RegistryCredential = &model.RegistryCredentialReference{Host: "registry.example:5443", VersionID: "regv_one", Username: "robot"}
	allocation.SpecDigest = runtimeTestDigest(t, allocation)
	if err := validateAllocation(allocation); err != nil {
		t.Fatalf("validate private registry allocation: %v", err)
	}
	allocation.RegistryCredential.Host = "other.example:5443"
	allocation.SpecDigest = runtimeTestDigest(t, allocation)
	if err := validateAllocation(allocation); err == nil || !strings.Contains(err.Error(), "registry credential") {
		t.Fatalf("validate cross-host registry credential = %v", err)
	}
}

func TestValidateAllocationAcceptsOnlyExactPlan4DigestDuringUpgrade(t *testing.T) {
	t.Parallel()
	legacy := runtimeTestAllocation(t)
	var err error
	legacy.SpecDigest, err = model.Plan4AllocationSpecDigest(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateAllocation(legacy); err != nil {
		t.Fatalf("validate legacy allocation: %v", err)
	}
	legacy.MaxSurge = 1
	if err := validateAllocation(legacy); err == nil || !strings.Contains(err.Error(), "spec digest") {
		t.Fatalf("extended allocation with legacy digest = %v", err)
	}
}

func TestValidateAllocationAcceptsMetadataOnlyApplicationRollback(t *testing.T) {
	t.Parallel()
	previous := runtimeTestAllocation(t)
	updated := previous
	updated.ApplicationGeneration = 2
	updated.RevisionID = "revision-two"
	updated.Rollback = &model.RollbackSpec{
		ApplicationGeneration: previous.ApplicationGeneration, RevisionID: previous.RevisionID,
		SpecDigest: previous.SpecDigest, Image: previous.Image,
		Command: append([]string(nil), previous.Command...), Args: append([]string(nil), previous.Args...),
		Environment: previous.Environment, RestartPolicy: previous.RestartPolicy,
		CPUMillis: previous.CPUMillis, MemoryBytes: previous.MemoryBytes,
		StartDeadlineSeconds: previous.StartDeadlineSeconds,
		AutoRollback:         previous.AutoRollback, StartBeforeStop: previous.StartBeforeStop,
	}
	if updated.SpecDigest != updated.Rollback.SpecDigest {
		t.Fatal("test setup does not use one immutable runtime spec")
	}
	if err := validateAllocation(updated); err != nil {
		t.Fatalf("metadata-only rollback: %v", err)
	}
}

func runtimeTestAllocation(t *testing.T) model.AllocationDesired {
	t.Helper()
	value := model.AllocationDesired{
		ID: "alloc-one", Application: "hello", Service: "worker", Replica: 1,
		ApplicationGeneration: 1, RevisionID: "revision-one",
		Image:   "docker.io/library/busybox@sha256:" + strings.Repeat("a", 64),
		Command: []string{"/bin/sleep"}, Args: []string{"3600"}, Environment: map[string]string{"APP_ENV": "test"},
		RestartPolicy: "always", CPUMillis: 100, MemoryBytes: 128 << 20,
		StartDeadlineSeconds: 30, AutoRollback: true, StartBeforeStop: true,
	}
	value.SpecDigest = runtimeTestDigest(t, value)
	return value
}

func runtimeTestDigest(t *testing.T, value model.AllocationDesired) string {
	t.Helper()
	digest, err := model.AllocationSpecDigest(value)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}
