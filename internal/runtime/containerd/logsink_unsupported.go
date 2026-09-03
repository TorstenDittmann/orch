//go:build !linux

package containerd

import (
	"context"

	orchruntime "change.codes/torsten/better-swarm/internal/runtime"
)

func RunLogSink(context.Context, string) error {
	return orchruntime.ErrUnsupported
}
