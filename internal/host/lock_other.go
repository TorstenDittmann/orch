//go:build !linux

package host

import "errors"

func installationLock() (func(), error)           { return nil, errors.New("Linux required") }
func controllerInstallationLock() (func(), error) { return nil, errors.New("Linux required") }
func renameExclusive(string, string) error        { return errors.New("Linux required") }
