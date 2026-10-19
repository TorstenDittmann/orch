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
	"sort"
	"strings"
	"time"

	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
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
	for _, suffix := range logSuffixes {
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

const (
	retiredLogsDirectory = "retired"
	// A removed service instance's logs are kept this long, within
	// retiredLogBudget per node, for orch logs --previous.
	retiredLogRetention = 24 * time.Hour
	retiredLogBudget    = 256 << 20
)

var logSuffixes = []string{".stdout.log", ".stdout.log.1", ".stderr.log", ".stderr.log.1"}

// retireObjectLogs moves a removed object's logs into the retired directory,
// replacing any logs retired earlier under the same object ID, and stamps
// them with the removal time.
func retireObjectLogs(directory, id string, now time.Time) error {
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id {
		return fmt.Errorf("refusing to retire logs for invalid object id %q", id)
	}
	retired := filepath.Join(directory, retiredLogsDirectory)
	if err := os.Mkdir(retired, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := requirePrivateLogDirectory(retired); err != nil {
		return err
	}
	if err := removeObjectLogs(retired, id); err != nil {
		return err
	}
	for _, suffix := range logSuffixes {
		source := filepath.Join(directory, id+suffix)
		info, err := os.Lstat(source)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("refusing to retire non-private log path %s", source)
		}
		target := filepath.Join(retired, id+suffix)
		if err := os.Rename(source, target); err != nil {
			return err
		}
		if err := os.Chtimes(target, now, now); err != nil {
			return err
		}
	}
	return pruneRetiredLogs(retired, now)
}

type retiredObject struct {
	id      string
	retired time.Time
	size    int64
}

// retiredObjects lists the retired directory by object, newest first. A
// missing directory has no objects.
func retiredObjects(retired string) ([]retiredObject, error) {
	entries, err := os.ReadDir(retired)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	byID := map[string]*retiredObject{}
	for _, entry := range entries {
		var id string
		for _, suffix := range logSuffixes {
			if trimmed, ok := strings.CutSuffix(entry.Name(), suffix); ok && trimmed != "" {
				id = trimmed
				break
			}
		}
		if id == "" || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		object := byID[id]
		if object == nil {
			object = &retiredObject{id: id}
			byID[id] = object
		}
		object.size += info.Size()
		if info.ModTime().After(object.retired) {
			object.retired = info.ModTime()
		}
	}
	objects := make([]retiredObject, 0, len(byID))
	for _, object := range byID {
		objects = append(objects, *object)
	}
	sort.Slice(objects, func(i, j int) bool {
		if !objects[i].retired.Equal(objects[j].retired) {
			return objects[i].retired.After(objects[j].retired)
		}
		return objects[i].id < objects[j].id
	})
	return objects, nil
}

// pruneRetiredLogs drops retired logs past retiredLogRetention, then the
// oldest ones until the rest fit in retiredLogBudget.
func pruneRetiredLogs(retired string, now time.Time) error {
	objects, err := retiredObjects(retired)
	if err != nil {
		return err
	}
	var kept int64
	for _, object := range objects {
		if now.Sub(object.retired) <= retiredLogRetention && kept+object.size <= retiredLogBudget {
			kept += object.size
			continue
		}
		if err := removeObjectLogs(retired, object.id); err != nil {
			return err
		}
	}
	return nil
}

// readPreviousLogs reads the most recently retired logs of an allocation.
func readPreviousLogs(directory, allocationID string, lines uint32, cursor string, now time.Time) (orchruntime.LogBatch, error) {
	if allocationID == "" || filepath.Base(allocationID) != allocationID {
		return orchruntime.LogBatch{}, orchruntime.ErrNotFound
	}
	retired := filepath.Join(directory, retiredLogsDirectory)
	if err := pruneRetiredLogs(retired, now); err != nil {
		return orchruntime.LogBatch{}, err
	}
	objects, err := retiredObjects(retired)
	if err != nil {
		return orchruntime.LogBatch{}, err
	}
	for _, object := range objects {
		if strings.HasPrefix(object.id, allocationID+"-") {
			batch, err := readObjectLogs(retired, object.id, lines, cursor)
			if err != nil {
				return orchruntime.LogBatch{}, err
			}
			batch.Exited = true
			return batch, nil
		}
	}
	return orchruntime.LogBatch{}, orchruntime.ErrNotFound
}
