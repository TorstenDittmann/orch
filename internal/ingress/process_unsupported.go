//go:build !linux

package ingress

import (
	"fmt"
	"os"
	"os/exec"
)

func validateProcessOptions(CaddyOptions) (processIdentity, error) {
	return processIdentity{}, ErrUnsupported
}

func prepareProcessCommand(*exec.Cmd, processIdentity) error                { return ErrUnsupported }
func protectProcessFile(string, processIdentity, os.FileMode) error         { return ErrUnsupported }
func publishProcessFile(string, []byte, processIdentity, os.FileMode) error { return ErrUnsupported }
func openProcessLog(string, processIdentity) (*os.File, error) {
	return nil, fmt.Errorf("open Caddy log: %w", ErrUnsupported)
}
func inspectRunningProcess(CaddyOptions, processIdentity) (runningProcessIdentity, error) {
	return runningProcessIdentity{}, ErrUnsupported
}
func inspectProcessPID(int, CaddyOptions, processIdentity) (runningProcessIdentity, error) {
	return runningProcessIdentity{}, ErrUnsupported
}
func removeOwnedStaleSocket(CaddyOptions, processIdentity, runningProcessIdentity) error {
	return ErrUnsupported
}
