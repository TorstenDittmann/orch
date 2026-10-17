//go:build !windows

package filelock

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// TryLock takes an exclusive lock on file without waiting.
func TryLock(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrLocked
	}
	return err
}

// Unlock releases a lock taken with TryLock.
func Unlock(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
