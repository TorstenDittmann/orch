package control

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

const controllerLockFilename = "controller.lock"

var (
	ErrControllerRunning = errors.New("another controller already owns this data directory")
	processLocks         = struct {
		sync.Mutex
		paths map[string]struct{}
	}{paths: make(map[string]struct{})}
)

type dataLock struct {
	path string
	file *os.File
	once sync.Once
	err  error
}

func acquireDataLock(dataDir string) (*dataLock, error) {
	path := filepath.Join(dataDir, controllerLockFilename)
	processLocks.Lock()
	if _, exists := processLocks.paths[path]; exists {
		processLocks.Unlock()
		return nil, ErrControllerRunning
	}
	processLocks.paths[path] = struct{}{}
	processLocks.Unlock()
	releasePath := true
	defer func() {
		if releasePath {
			forgetProcessLock(path)
		}
	}()

	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("controller lock is not a regular file: %s", path)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("controller lock permissions %04o allow group or world access", info.Mode().Perm())
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect controller lock: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open controller lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrControllerRunning
		}
		return nil, fmt.Errorf("acquire controller lock: %w", err)
	}
	if err := file.Truncate(0); err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
		return nil, fmt.Errorf("truncate controller lock: %w", err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
		return nil, fmt.Errorf("seek controller lock: %w", err)
	}
	if _, err := file.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
		return nil, fmt.Errorf("write controller lock: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
		return nil, fmt.Errorf("sync controller lock: %w", err)
	}
	releasePath = false
	return &dataLock{path: path, file: file}, nil
}

func (lock *dataLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	lock.once.Do(func() {
		unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
		closeErr := lock.file.Close()
		forgetProcessLock(lock.path)
		lock.err = errors.Join(unlockErr, closeErr)
	})
	return lock.err
}

func forgetProcessLock(path string) {
	processLocks.Lock()
	delete(processLocks.paths, path)
	processLocks.Unlock()
}
