//go:build !linux

package containerd

import (
	"context"

	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

func RunLogSink(context.Context, string) error {
	return orchruntime.ErrUnsupported
}
