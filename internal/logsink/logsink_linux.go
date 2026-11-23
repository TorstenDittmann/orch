//go:build linux

// Package logsink is the per-task log sink containerd's shim starts for every
// container. It imports only the standard library so cmd/orch-log-sink stays a
// small binary: one sink runs for each container on a node.
package logsink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// MaxFileBytes bounds each stream's log file; RotatedBytes is how much of it
// is kept in the .1 file when it rotates.
const (
	MaxFileBytes = 8 << 20
	RotatedBytes = 4 << 20
)

// Run is the binary-v2 logger entrypoint used by the runc v2
// shim. File descriptors 3 and 4 carry stdout and stderr; descriptor 5 is the
// binary-v2 readiness pipe.
func Run(ctx context.Context, prefix string) error {
	if !filepath.IsAbs(prefix) || filepath.Base(prefix) == "." || filepath.Base(prefix) == string(filepath.Separator) {
		return fmt.Errorf("log sink prefix must be an absolute object path")
	}
	if err := RequirePrivateDirectory(filepath.Dir(prefix)); err != nil {
		return err
	}
	stdout, err := NewRotatingLog(prefix + ".stdout.log")
	if err != nil {
		return err
	}
	defer stdout.Close()
	stderr, err := NewRotatingLog(prefix + ".stderr.log")
	if err != nil {
		return err
	}
	defer stderr.Close()

	stdoutPipe := os.NewFile(3, "container-stdout")
	stderrPipe := os.NewFile(4, "container-stderr")
	readyPipe := os.NewFile(5, "container-wait")
	if stdoutPipe == nil || stderrPipe == nil || readyPipe == nil {
		return fmt.Errorf("containerd log descriptors are unavailable")
	}
	defer stdoutPipe.Close()
	defer stderrPipe.Close()
	if _, err := readyPipe.Write([]byte{1}); err != nil {
		_ = readyPipe.Close()
		return fmt.Errorf("signal log sink readiness: %w", err)
	}
	if err := readyPipe.Close(); err != nil {
		return fmt.Errorf("close log sink readiness pipe: %w", err)
	}

	type copyResult struct{ err error }
	results := make(chan copyResult, 2)
	copyStream := func(destination io.Writer, source *os.File) {
		_, copyErr := io.Copy(destination, source)
		results <- copyResult{err: copyErr}
	}
	go copyStream(stdout, stdoutPipe)
	go copyStream(stderr, stderrPipe)

	var copyErrors []error
	for completed := 0; completed < 2; {
		select {
		case <-ctx.Done():
			_ = stdoutPipe.Close()
			_ = stderrPipe.Close()
			ctx = context.Background()
		case result := <-results:
			completed++
			if result.err != nil && !errors.Is(result.err, os.ErrClosed) {
				copyErrors = append(copyErrors, result.err)
				_ = stdoutPipe.Close()
				_ = stderrPipe.Close()
			}
		}
	}
	return errors.Join(copyErrors...)
}

// RotatingLog appends to one stream's log file and keeps it under
// MaxFileBytes, moving the newest RotatedBytes to a .1 file when it fills.
type RotatingLog struct {
	mu   sync.Mutex
	path string
	file *os.File
	size int64
}

// NewRotatingLog opens path as a private log file for appending.
func NewRotatingLog(path string) (*RotatingLog, error) {
	file, err := OpenPrivate(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	result := &RotatingLog{path: path, file: file, size: info.Size()}
	if result.size > MaxFileBytes {
		if err := result.rotate(); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return result, nil
}

func (w *RotatingLog) Write(contents []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written := 0
	for len(contents) != 0 {
		if w.size >= MaxFileBytes {
			if err := w.rotate(); err != nil {
				return written, err
			}
		}
		available := int64(MaxFileBytes) - w.size
		chunkSize := min(int64(len(contents)), available)
		amount, err := w.file.Write(contents[:chunkSize])
		written += amount
		w.size += int64(amount)
		contents = contents[amount:]
		if err != nil {
			return written, err
		}
		if amount == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func (w *RotatingLog) rotate() error {
	retain := min(w.size, int64(RotatedBytes))
	contents := make([]byte, retain)
	if retain != 0 {
		if _, err := w.file.ReadAt(contents, w.size-retain); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	}
	if err := writePrivateLogAtomic(w.path+".1", contents); err != nil {
		return err
	}
	if err := w.file.Truncate(0); err != nil {
		return err
	}
	w.size = 0
	return nil
}

func (w *RotatingLog) Close() error {
	if w == nil || w.file == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return errors.Join(w.file.Sync(), w.file.Close())
}

// OpenPrivate opens or creates path as a 0600 regular file, refusing
// symlinks and files other users can read.
func OpenPrivate(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("log path %s must be a private regular file", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	descriptor, err := syscall.Open(path, syscall.O_CREAT|syscall.O_APPEND|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(descriptor), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		_ = file.Close()
		return nil, fmt.Errorf("log path %s must be a private regular file", path)
	}
	return file, nil
}

func writePrivateLogAtomic(path string, contents []byte) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("rotated log path %s must be a private regular file", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".orch-log-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

// RequirePrivateDirectory checks that path is a directory only its owner
// can use, and not a symlink.
func RequirePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("log directory %s must be a private non-symlink directory", path)
	}
	return nil
}
