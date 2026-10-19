//go:build !linux

package agent

import "errors"

func readHost(string) (hostReading, error) {
	return hostReading{}, errors.New("host usage is only read on Linux")
}
