//go:build linux

package containerd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	orchruntime "change.codes/torsten/better-swarm/internal/runtime"
)

const (
	maxLogFileBytes = 8 << 20
	rotatedLogBytes = 4 << 20
)

func ensureLogFiles(directory, id string) (string, error) {
	prefix := filepath.Join(directory, id)
	stdout := prefix + ".stdout.log"
	stderr := prefix + ".stderr.log"
	for _, path := range []string{stdout, stderr} {
		file, err := openPrivateLog(path)
		if err != nil {
			return "", err
		}
		if err := file.Close(); err != nil {
			return "", err
		}
	}
	return prefix, nil
}

func tailObjectLogs(directory, id string, lines uint32) ([]orchruntime.LogFrame, error) {
	if lines == 0 {
		lines = 100
	}
	var result []orchruntime.LogFrame
	for _, stream := range []string{"stdout", "stderr"} {
		base := filepath.Join(directory, id+"."+stream+".log")
		var combined []byte
		for _, path := range []string{base + ".1", base} {
			contents, err := readPrivateLog(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			combined = append(combined, contents...)
		}
		parts := bytes.Split(combined, []byte{'\n'})
		if len(parts) != 0 && len(parts[len(parts)-1]) == 0 {
			parts = parts[:len(parts)-1]
		}
		truncated := len(parts) > int(lines)
		if truncated {
			parts = parts[len(parts)-int(lines):]
		}
		if len(parts) == 0 {
			continue
		}
		data := bytes.Join(parts, []byte{'\n'})
		data = append(data, '\n')
		result = append(result, orchruntime.LogFrame{Stream: stream, Data: data, Offset: uint64(len(combined)), Truncated: truncated})
	}
	return result, nil
}

func readPrivateLog(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("log path %s must be a private regular file", path)
	}
	return os.ReadFile(path)
}

func removeObjectLogs(directory, id string) error {
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id {
		return fmt.Errorf("refusing to remove logs for invalid object id %q", id)
	}
	for _, suffix := range []string{".stdout.log", ".stdout.log.1", ".stderr.log", ".stderr.log.1"} {
		path := filepath.Join(directory, id+suffix)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("refusing to remove non-private log path %s", path)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}
