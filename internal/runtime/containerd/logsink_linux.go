//go:build linux

package containerd

import (
	"context"

	"github.com/TorstenDittmann/orch/internal/logsink"
)

const (
	maxLogFileBytes = logsink.MaxFileBytes
)

var (
	newRotatingLog             = logsink.NewRotatingLog
	openPrivateLog             = logsink.OpenPrivate
	requirePrivateLogDirectory = logsink.RequirePrivateDirectory
)

// RunLogSink is the binary-v2 logger entrypoint behind "orch
// runtime-log-sink", used when a build carries no embedded orch-log-sink.
func RunLogSink(ctx context.Context, prefix string) error {
	return logsink.Run(ctx, prefix)
}
