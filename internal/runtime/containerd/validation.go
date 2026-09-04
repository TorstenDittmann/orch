package containerd

import (
	"strings"

	"change.codes/torsten/better-swarm/internal/model"
	orchruntime "change.codes/torsten/better-swarm/internal/runtime"
)

func validateAllocation(allocation model.AllocationDesired) error {
	return orchruntime.ValidateAllocation(allocation)
}

func containerdRuntimeVersion(version string) string {
	return "containerd " + strings.TrimPrefix(version, "v")
}
