//go:build !linux

package logsink

import (
	"context"
	"errors"
)

// Run is only available on Linux, where containerd runs tasks.
func Run(context.Context, string) error {
	return errors.New("the orch log sink runs only on Linux")
}
