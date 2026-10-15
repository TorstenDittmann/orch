package containerd

import (
	"strings"

	"github.com/TorstenDittmann/orch/internal/model"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

func validateAllocation(allocation model.AllocationDesired) error {
	return orchruntime.ValidateAllocation(allocation)
}

func containerdRuntimeVersion(version string) string {
	return "containerd " + strings.TrimPrefix(version, "v")
}
