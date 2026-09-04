//go:build linux

package containerd

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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
	batch, err := readObjectLogs(directory, id, lines, "")
	return batch.Frames, err
}

type logCursor struct {
	Version int               `json:"v"`
	Object  []byte            `json:"o"`
	Offsets map[string]uint64 `json:"p"`
}

func readObjectLogs(directory, id string, lines uint32, encodedCursor string) (orchruntime.LogBatch, error) {
	if lines == 0 {
		lines = 100
	}
	identity := sha256.Sum256([]byte(id))
	cursor := logCursor{Version: 1, Object: identity[:], Offsets: map[string]uint64{}}
	reset := false
	continuation := encodedCursor != ""
	if continuation {
		decoded, err := base64.RawURLEncoding.DecodeString(encodedCursor)
		if err != nil || json.Unmarshal(decoded, &cursor) != nil || cursor.Version != 1 || !bytes.Equal(cursor.Object, identity[:]) || cursor.Offsets == nil {
			cursor = logCursor{Version: 1, Object: identity[:], Offsets: map[string]uint64{}}
			continuation, reset = false, true
		}
	}
	result := orchruntime.LogBatch{CursorReset: reset}
	for _, stream := range []string{"stdout", "stderr"} {
		combined, err := combinedLog(directory, id, stream)
		if err != nil {
			return orchruntime.LogBatch{}, err
		}
		start := cursor.Offsets[stream]
		streamReset := continuation && start > uint64(len(combined))
		if streamReset {
			start, result.CursorReset = 0, true
		}
		if continuation && !streamReset {
			if start < uint64(len(combined)) {
				result.Frames = append(result.Frames, orchruntime.LogFrame{Stream: stream, Data: append([]byte(nil), combined[start:]...), Offset: uint64(len(combined))})
			}
			cursor.Offsets[stream] = uint64(len(combined))
			continue
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
			cursor.Offsets[stream] = uint64(len(combined))
			continue
		}
		data := bytes.Join(parts, []byte{'\n'})
		data = append(data, '\n')
		result.Frames = append(result.Frames, orchruntime.LogFrame{Stream: stream, Data: data, Offset: uint64(len(combined)), Truncated: truncated || streamReset})
		cursor.Offsets[stream] = uint64(len(combined))
	}
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return orchruntime.LogBatch{}, err
	}
	result.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	return result, nil
}

func combinedLog(directory, id, stream string) ([]byte, error) {
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
	return combined, nil
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
