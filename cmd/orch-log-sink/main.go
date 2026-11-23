// Command orch-log-sink is the small binary-v2 logger that containerd's shim
// starts for each task. It does what "orch runtime-log-sink" does without
// loading the whole orch binary, so each running container costs well under
// a megabyte for its logs instead of several. Release builds embed it in orch,
// and the agent installs it on start.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/TorstenDittmann/orch/internal/logsink"
)

func main() {
	// The shim passes the logger URI's query as arguments, the same ones
	// orch itself receives: runtime-log-sink <object path prefix>.
	if len(os.Args) != 3 || os.Args[1] != "runtime-log-sink" {
		fmt.Fprintln(os.Stderr, "usage: orch-log-sink runtime-log-sink <prefix>")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := logsink.Run(ctx, os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
