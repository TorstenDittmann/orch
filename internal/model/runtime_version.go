package model

import (
	"strconv"
	"strings"
)

// MinimumContainerdPatch is the oldest containerd 2.3 patch release orch runs
// workloads on. Nodes installed before the bundle moved to a newer patch keep
// working until their next upgrade refreshes the daemon.
const MinimumContainerdPatch = 4

// ContainerdRequirement describes the supported runtime for error messages.
const ContainerdRequirement = "containerd 2.3.4 or a newer 2.3 release"

// SupportedRuntimeVersion reports whether a node's runtime version, as the
// agent reports it ("containerd 2.3.7"), is one this release supports.
func SupportedRuntimeVersion(runtimeVersion string) bool {
	version, ok := strings.CutPrefix(strings.TrimSpace(runtimeVersion), "containerd ")
	if !ok {
		return false
	}
	return SupportedContainerdVersion(version)
}

// SupportedContainerdVersion reports whether a bare containerd version
// ("2.3.7" or "v2.3.7") is supported: 2.3.x from MinimumContainerdPatch on.
func SupportedContainerdVersion(version string) bool {
	patch, ok := strings.CutPrefix(strings.TrimPrefix(strings.TrimSpace(version), "v"), "2.3.")
	if !ok || patch == "" {
		return false
	}
	number, err := strconv.Atoi(patch)
	return err == nil && number >= MinimumContainerdPatch && strconv.Itoa(number) == patch
}
