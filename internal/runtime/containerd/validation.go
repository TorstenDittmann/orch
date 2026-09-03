package containerd

import (
	"change.codes/torsten/better-swarm/internal/model"
	orchruntime "change.codes/torsten/better-swarm/internal/runtime"
)

func validateAllocation(allocation model.AllocationDesired) error {
	return orchruntime.ValidateAllocation(allocation)
}
