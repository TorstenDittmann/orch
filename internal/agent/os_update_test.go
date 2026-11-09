package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
)

func TestNewerKernelInstalled(t *testing.T) {
	t.Parallel()
	output := "kernel-core-6.11.5-300.fc41.x86_64  Tue 05 Nov 2026 10:00:00 AM UTC\nkernel-core-6.11.4-301.fc41.x86_64  Mon 21 Oct 2026\n"
	for running, want := range map[string]bool{
		"6.11.5-300.fc41.x86_64": false,
		"6.11.4-301.fc41.x86_64": true,
		"":                       false,
	} {
		if got := newerKernelInstalled(output, running); got != want {
			t.Fatalf("running %q: got %t, want %t", running, got, want)
		}
	}
	if newerKernelInstalled("package kernel-core is not installed", "6.11.4") {
		t.Fatal("missing package read as a newer kernel")
	}
}

type stubOSUpdater struct {
	err      error
	required bool
	reboots  int
}

func (s *stubOSUpdater) Available() bool { return true }
func (s *stubOSUpdater) Upgrade(context.Context) ([]byte, error) {
	return []byte("  done\n"), s.err
}
func (s *stubOSUpdater) RebootRequired(context.Context) bool { return s.required }
func (s *stubOSUpdater) Reboot(context.Context) error {
	s.reboots++
	return nil
}

func TestRunOSUpdateAnswersAndRebootsOnlyWhenNeeded(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)
	run := func(updater *stubOSUpdater, available bool) *orchv1alpha1.AgentOperationResult {
		t.Helper()
		runner := &sessionRunner{osUpdates: available, reboot: &rebootCheck{updater: updater}, now: func() time.Time { return now }}
		outbound := make(chan *orchv1alpha1.ConnectRequest, 1)
		runner.runOSUpdate(context.Background(), &orchv1alpha1.OSUpdateCommand{RequestId: "request"}, outbound)
		return (<-outbound).GetOperationResult()
	}
	plain := &stubOSUpdater{}
	if result := run(plain, true).GetOsUpdate(); result.GetRebooting() || result.GetOutput() != "done" || plain.reboots != 0 {
		t.Fatalf("result = %v reboots = %d", result, plain.reboots)
	}
	kernel := &stubOSUpdater{required: true}
	if result := run(kernel, true).GetOsUpdate(); !result.GetRebooting() || kernel.reboots != 1 {
		t.Fatalf("result = %v reboots = %d", result, kernel.reboots)
	}
	broken := &stubOSUpdater{err: errors.New("exit status 100"), required: true}
	if failure := run(broken, true).GetError(); failure.GetCode() != "failed" || failure.GetMessage() != "exit status 100: done" || broken.reboots != 0 {
		t.Fatalf("failure = %v reboots = %d", failure, broken.reboots)
	}
	if failure := run(&stubOSUpdater{}, false).GetError(); failure.GetCode() != "unsupported" {
		t.Fatalf("unsupported = %v", failure)
	}
}

// Serve builds the runner without a clock, so heartbeats and OS updates must
// fall back to the wall clock instead of calling a nil now.
func TestRebootCheckWithoutClock(t *testing.T) {
	t.Parallel()
	updater := &stubOSUpdater{required: true}
	runner := &sessionRunner{cache: supervisorCache(t), osUpdates: true, reboot: &rebootCheck{updater: updater}}
	if heartbeat := runner.heartbeatRequest(context.Background()).GetHeartbeat(); !heartbeat.GetRebootRequired() {
		t.Fatalf("heartbeat = %v", heartbeat)
	}
	outbound := make(chan *orchv1alpha1.ConnectRequest, 1)
	runner.runOSUpdate(context.Background(), &orchv1alpha1.OSUpdateCommand{RequestId: "request"}, outbound)
	if result := (<-outbound).GetOperationResult().GetOsUpdate(); !result.GetRebooting() || updater.reboots != 1 {
		t.Fatalf("result = %v reboots = %d", result, updater.reboots)
	}
}
