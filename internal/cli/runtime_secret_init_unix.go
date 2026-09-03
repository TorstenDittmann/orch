//go:build !windows

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func runRuntimeSecretInit(args []string, stderr io.Writer) int {
	separator := -1
	for index, value := range args {
		if value == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator == len(args)-1 {
		fmt.Fprintln(stderr, "runtime secret initializer received an invalid process command")
		return ExitRuntime
	}
	for _, binding := range args[:separator] {
		name, path, ok := strings.Cut(binding, "=")
		if !ok || name == "" || path == "" || strings.ContainsAny(name, "=\x00") || !strings.HasPrefix(path, "/run/orch/secrets/") {
			fmt.Fprintln(stderr, "runtime secret initializer received an invalid binding")
			return ExitRuntime
		}
		value, err := os.ReadFile(path)
		if err != nil || len(value) == 0 || len(value) > maxSecretInput || strings.IndexByte(string(value), 0) >= 0 {
			wipeCLISecret(value)
			fmt.Fprintln(stderr, "runtime secret initializer could not read a valid secret")
			return ExitRuntime
		}
		err = os.Setenv(name, string(value))
		wipeCLISecret(value)
		if err != nil {
			fmt.Fprintln(stderr, "runtime secret initializer could not set a secret environment variable")
			return ExitRuntime
		}
	}
	command := args[separator+1:]
	executable, err := exec.LookPath(command[0])
	if err != nil {
		fmt.Fprintln(stderr, "runtime secret initializer could not resolve the workload process")
		return ExitRuntime
	}
	if err := syscall.Exec(executable, command, os.Environ()); err != nil {
		fmt.Fprintln(stderr, errors.New("runtime secret initializer could not execute the workload process"))
		return ExitRuntime
	}
	return ExitOK
}

func wipeCLISecret(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
