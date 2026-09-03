//go:build windows

package cli

import (
	"fmt"
	"io"
)

func runRuntimeSecretInit(_ []string, stderr io.Writer) int {
	fmt.Fprintln(stderr, "runtime secret initialization requires a Unix host")
	return ExitRuntime
}
