package containerd

import (
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
		{name: "local registry", want: "private registry", edit: func(value *model.AllocationDesired) {
			value.Image = "localhost:5000/test/app@sha256:" + strings.Repeat("b", 64)
		}},
		{name: "restart policy", want: "restart policy", edit: func(value *model.AllocationDesired) { value.RestartPolicy = "sometimes" }},
		{name: "empty command", want: "command", edit: func(value *model.AllocationDesired) { value.Command = []string{""} }},
		{name: "environment", want: "environment", edit: func(value *model.AllocationDesired) { value.Environment = map[string]string{"BAD=KEY": "value"} }},
		{name: "environment syntax", want: "environment", edit: func(value *model.AllocationDesired) { value.Environment = map[string]string{"BAD-NAME": "value"} }},
		{name: "volume name", want: "volume mount", edit: func(value *model.AllocationDesired) {
			value.Mounts = []model.VolumeMount{{Name: "../data", Target: "/data"}}
		}},
		{name: "protected mount", want: "volume mount", edit: func(value *model.AllocationDesired) {
			value.Mounts = []model.VolumeMount{{Name: "data", Target: "/proc/data"}}
		}},
		{name: "duplicate mount", want: "duplicated", edit: func(value *model.AllocationDesired) {
			value.Mounts = []model.VolumeMount{{Name: "data", Target: "/data"}, {Name: "cache", Target: "/data"}}
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
