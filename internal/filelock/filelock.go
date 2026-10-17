// Package filelock takes advisory, non-blocking exclusive locks on open files.
package filelock

import "errors"

// ErrLocked reports that another process already holds the lock.
var ErrLocked = errors.New("file is locked by another process")
