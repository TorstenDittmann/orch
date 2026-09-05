//go:build !linux

package host

import "errors"

func rootOwned(string, bool) error { return errors.New("root ownership inspection requires Linux") }
