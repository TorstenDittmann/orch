package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
)

const (
	osUpdateUnit    = "orch-os-update"
	osRebootUnit    = "orch-os-reboot"
	osUpdateTimeout = 45 * time.Minute
	// osUpdateOutputBytes is how much of the package manager's output the
	// agent returns: enough to see what failed.
	osUpdateOutputBytes = 8 << 10
	// rebootCheckEvery spaces out the reboot check, which runs rpm on
	// Fedora, across heartbeats.
	rebootCheckEvery = 10 * time.Minute
	// controllerUnit is installed by orch init and orch cluster install.
	controllerUnit = "/etc/systemd/system/orch-controller.service"
)

// OSUpdater upgrades the host's packages and reboots it for orch node update.
type OSUpdater interface {
	// Available reports whether the host can be updated at all.
	Available() bool
	// Upgrade upgrades the host's packages and returns the package
	// manager's output.
	Upgrade(ctx context.Context) ([]byte, error)
	// RebootRequired reports whether installed updates wait for a reboot.
	RebootRequired(ctx context.Context) bool
	// Reboot reboots the host shortly, after the agent has answered.
	Reboot(ctx context.Context) error
}

// HostOSUpdater updates the host with apt-get or dnf through systemd.
type HostOSUpdater struct{}

// script returns the shell script that upgrades the host's packages, or ""
// when the host has no supported package manager.
func (HostOSUpdater) script() string {
	if _, err := exec.LookPath("apt-get"); err == nil {
		// needrestart would restart services, the agent among them, in the
		// middle of the operation; a reboot follows when one is needed.
		return `export DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=l NEEDRESTART_SUSPEND=1
apt-get -q -o DPkg::Lock::Timeout=600 update &&
apt-get -q -y -o DPkg::Lock::Timeout=600 -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold upgrade --with-new-pkgs`
	}
	if _, err := exec.LookPath("dnf"); err == nil {
		return `dnf -y upgrade --refresh`
	}
	return ""
}

func (u HostOSUpdater) Available() bool {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 || u.script() == "" {
		return false
	}
	_, err := exec.LookPath("systemd-run")
	return err == nil
}

// Upgrade runs the package manager in a transient systemd unit, outside the
// agent's own cgroup, so a package that restarts services cannot kill it
// halfway.
func (u HostOSUpdater) Upgrade(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "systemd-run", "--unit", osUpdateUnit, "--collect", "--quiet", "--wait", "--pipe",
		"--description", "orch OS update", "/bin/sh", "-c", u.script()).CombinedOutput()
}

// RebootRequired reads the marker file Debian and Ubuntu leave, and on
// Fedora compares the newest installed kernel with the running one.
func (HostOSUpdater) RebootRequired(ctx context.Context) bool {
	for _, marker := range []string{"/run/reboot-required", "/var/run/reboot-required"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	if _, err := exec.LookPath("rpm"); err != nil {
		return false
	}
	commandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(commandCtx, "rpm", "-q", "--last", "kernel-core").Output()
	if err != nil {
		return false
	}
	running, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return false
	}
	return newerKernelInstalled(string(output), strings.TrimSpace(string(running)))
}

// Reboot schedules the reboot in its own systemd unit, a few seconds out, so
// the agent's answer reaches the controller first.
func (HostOSUpdater) Reboot(ctx context.Context) error {
	output, err := exec.CommandContext(ctx, "systemd-run", "--unit", osRebootUnit, "--collect", "--quiet",
		"--on-active=5", "--description", "orch reboot after OS update", "/bin/systemctl", "reboot").CombinedOutput()
	if err != nil {
		return fmt.Errorf("schedule reboot: %v: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// newerKernelInstalled reads the output of rpm -q --last kernel-core, newest
// install first, such as "kernel-core-6.11.4-301.fc41.x86_64  Tue 05 Nov".
func newerKernelInstalled(rpmOutput, running string) bool {
	line, _, _ := strings.Cut(strings.TrimSpace(rpmOutput), "\n")
	fields := strings.Fields(line)
	if len(fields) == 0 || running == "" {
		return false
	}
	newest, ok := strings.CutPrefix(fields[0], "kernel-core-")
	return ok && newest != running
}

func controllerOnHost() bool {
	_, err := os.Stat(controllerUnit)
	return err == nil
}

// rebootCheck caches the updater's reboot check between heartbeats.
type rebootCheck struct {
	updater OSUpdater
	mu      sync.Mutex
	checked time.Time
	value   bool
}

func (c *rebootCheck) required(ctx context.Context, now time.Time) bool {
	if c == nil || c.updater == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.checked.IsZero() || now.Sub(c.checked) >= rebootCheckEvery {
		c.value, c.checked = c.updater.RebootRequired(ctx), now
	}
	return c.value
}

func (c *rebootCheck) reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.checked = time.Time{}
	c.mu.Unlock()
}

func (r *sessionRunner) runOSUpdate(ctx context.Context, command *orchv1alpha1.OSUpdateCommand, outbound chan<- *orchv1alpha1.ConnectRequest) {
	if !r.osUpdates || r.reboot == nil || r.reboot.updater == nil {
		enqueueOperationError(ctx, outbound, command.GetRequestId(), "unsupported", "this host has no supported package manager or systemd-run")
		return
	}
	if !r.osUpdating.CompareAndSwap(false, true) {
		enqueueOperationError(ctx, outbound, command.GetRequestId(), "busy", "an OS update is already running on this node")
		return
	}
	defer r.osUpdating.Store(false)
	updater := r.reboot.updater
	updateContext, cancel := context.WithTimeout(ctx, osUpdateTimeout)
	defer cancel()
	output, err := updater.Upgrade(updateContext)
	output = tail(output, osUpdateOutputBytes)
	r.reboot.reset()
	if err != nil {
		message := err.Error()
		if len(output) > 0 {
			message += ": " + string(output)
		}
		enqueueOperationError(ctx, outbound, command.GetRequestId(), "failed", strings.ToValidUTF8(string(tail([]byte(message), osUpdateOutputBytes)), ""))
		return
	}
	rebooting := false
	if r.reboot.required(ctx, r.now()) {
		if err := updater.Reboot(ctx); err != nil {
			enqueueOperationError(ctx, outbound, command.GetRequestId(), "failed", boundedMessage(err.Error()))
			return
		}
		rebooting = true
	}
	select {
	case <-ctx.Done():
	case outbound <- operationResult(&orchv1alpha1.AgentOperationResult_OsUpdate{OsUpdate: &orchv1alpha1.OSUpdateResponse{
		RequestId: command.GetRequestId(), Rebooting: rebooting, Output: strings.ToValidUTF8(string(output), ""),
	}}):
	}
}

func tail(data []byte, limit int) []byte {
	data = bytes.TrimSpace(data)
	if len(data) <= limit {
		return data
	}
	return data[len(data)-limit:]
}
