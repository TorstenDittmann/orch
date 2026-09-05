//go:build linux

package host

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

func installationLock() (func(), error) {
	return namedInstallationLock("/run/lock/orch-install.lock", "node")
}
func controllerInstallationLock() (func(), error) {
	return namedInstallationLock("/run/lock/orch-controller-install.lock", "controller")
}
func namedInstallationLock(path, kind string) (func(), error) {
	if err := noSymlinks(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := rootOwned(path, false); err != nil {
		f.Close()
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another %s install is running: %w", kind, err)
	}
	return func() { unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, nil
}
func renameExclusive(from, to string) error {
	return unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
}
