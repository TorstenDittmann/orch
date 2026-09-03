//go:build !linux

package containerd

import (
	"fmt"

	orchruntime "change.codes/torsten/better-swarm/internal/runtime"
)

func New(_ string, _ orchruntime.Options) (orchruntime.Runtime, error) {
	return nil, fmt.Errorf("containerd agent runtime requires Linux: %w", orchruntime.ErrUnsupported)
}
