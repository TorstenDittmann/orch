package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/TorstenDittmann/orch/internal/filelock"
)

const lockFilename = "agent.lock"

var (
	ErrAgentRunning   = errors.New("another agent already owns this data directory")
	agentProcessLocks = struct {
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
	path := filepath.Join(dataDir, lockFilename)
	agentProcessLocks.Lock()
	if _, exists := agentProcessLocks.paths[path]; exists {
		agentProcessLocks.Unlock()
		return nil, ErrAgentRunning
	}
	agentProcessLocks.paths[path] = struct{}{}
	agentProcessLocks.Unlock()
	releasePath := true
	defer func() {
		if releasePath {
			forgetAgentLock(path)
		}
	}()
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("agent lock must be a private regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := filelock.TryLock(file); err != nil {
		_ = file.Close()
		if errors.Is(err, filelock.ErrLocked) {
			return nil, ErrAgentRunning
		}
		return nil, err
	}
	if err := file.Truncate(0); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := file.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, err
	}
	releasePath = false
	return &dataLock{path: path, file: file}, nil
}

func (lock *dataLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	lock.once.Do(func() {
		lock.err = errors.Join(filelock.Unlock(lock.file), lock.file.Close())
		forgetAgentLock(lock.path)
	})
	return lock.err
}

func forgetAgentLock(path string) {
	agentProcessLocks.Lock()
	delete(agentProcessLocks.paths, path)
	agentProcessLocks.Unlock()
}
