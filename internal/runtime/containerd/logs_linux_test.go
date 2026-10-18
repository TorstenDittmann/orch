//go:build linux

package containerd

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

func TestRotatingLogBoundsFilesAndTailKeepsStreamsSeparate(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	prefix, err := ensureLogFiles(directory, "object-one")
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := newRotatingLog(prefix + ".stdout.log")
	if err != nil {
		t.Fatal(err)
	}
	contents := bytes.Repeat([]byte("line\n"), maxLogFileBytes/5+1)
	if _, err := stdout.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := stdout.Close(); err != nil {
		t.Fatal(err)
	}
	stderr, err := newRotatingLog(prefix + ".stderr.log")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stderr.Write([]byte("warning\n")); err != nil {
		t.Fatal(err)
	}
	if err := stderr.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".stdout.log", ".stdout.log.1", ".stderr.log"} {
		info, err := os.Lstat(prefix + suffix)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("log %s = %#v, %v", suffix, info, err)
		}
		if info.Size() > maxLogFileBytes {
			t.Fatalf("log %s size = %d", suffix, info.Size())
		}
	}
	frames, err := tailObjectLogs(directory, "object-one", 2)
	if err != nil || len(frames) != 2 {
		t.Fatalf("tailObjectLogs() = %#v, %v", frames, err)
	}
	if frames[0].Stream != "stdout" || frames[1].Stream != "stderr" || string(frames[1].Data) != "warning\n" || frames[0].Offset == 0 {
		t.Fatalf("tail frames = %#v", frames)
	}
}

func TestReadObjectLogsContinuesWithoutDuplicatesAndResetsAfterShrink(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	prefix, err := ensureLogFiles(directory, "object-follow")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prefix+".stdout.log", []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	initial, err := readObjectLogs(directory, "object-follow", 1, "")
	if err != nil || len(initial.Frames) != 1 || string(initial.Frames[0].Data) != "two\n" || initial.NextCursor == "" {
		t.Fatalf("initial read = %#v, %v", initial, err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(initial.NextCursor)
	if err != nil || bytes.Contains(decoded, []byte("object-follow")) {
		t.Fatalf("cursor exposes runtime object identity: %q, %v", decoded, err)
	}
	file, err := os.OpenFile(prefix+".stdout.log", os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString("three\n")
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("append = %v, close = %v", writeErr, closeErr)
	}
	next, err := readObjectLogs(directory, "object-follow", 1, initial.NextCursor)
	if err != nil || next.CursorReset || len(next.Frames) != 1 || string(next.Frames[0].Data) != "three\n" {
		t.Fatalf("continuation = %#v, %v", next, err)
	}
	if err := os.WriteFile(prefix+".stdout.log", []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reset, err := readObjectLogs(directory, "object-follow", 10, next.NextCursor)
	if err != nil || !reset.CursorReset || len(reset.Frames) != 1 || string(reset.Frames[0].Data) != "new\n" {
		t.Fatalf("reset read = %#v, %v", reset, err)
	}
	malformed, err := readObjectLogs(directory, "object-follow", 10, "not-a-cursor")
	if err != nil || !malformed.CursorReset {
		t.Fatalf("malformed cursor = %#v, %v", malformed, err)
	}
}

func TestLogFilesRejectSymlinks(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "object.stdout.log")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := openPrivateLog(path); err == nil {
		t.Fatal("openPrivateLog accepted a symlink")
	}
	if _, err := readPrivateLog(path); err == nil {
		t.Fatal("readPrivateLog accepted a symlink")
	}
}

func TestRemoveObjectLogsDeletesOnlyPrivateRegularFiles(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	prefix, err := ensureLogFiles(directory, "object-one")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prefix+".stdout.log.1", []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeObjectLogs(directory, "object-one"); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".stdout.log", ".stdout.log.1", ".stderr.log", ".stderr.log.1"} {
		if _, err := os.Lstat(prefix + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("log %s remains after removal: %v", suffix, err)
		}
	}
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, prefix+".stdout.log"); err != nil {
		t.Fatal(err)
	}
	if err := removeObjectLogs(directory, "object-one"); err == nil {
		t.Fatal("removeObjectLogs accepted a symlink")
	}
	if err := removeObjectLogs(directory, "../object-one"); err == nil {
		t.Fatal("removeObjectLogs accepted a path-like object id")
	}
}

func TestTaskLogCreatorUsesOrchBinaryV2Sink(t *testing.T) {
	t.Parallel()
	creator := taskLogCreator("/usr/local/bin/orch", "/var/lib/orch-agent/logs/object")
	stream, err := creator("ignored")
	if err != nil {
		t.Fatal(err)
	}
	configuration := stream.Config()
	if configuration.Stdout != configuration.Stderr {
		t.Fatalf("logger URIs differ: %#v", configuration)
	}
	parsed, err := url.Parse(configuration.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "binary-v2" || parsed.Path != "/usr/local/bin/orch" || parsed.Query().Get("runtime-log-sink") != "/var/lib/orch-agent/logs/object" {
		t.Fatalf("logger URI = %s", configuration.Stdout)
	}
}

func TestRetiredLogsServePreviousInstanceAndExpire(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeObject := func(id, line string) {
		t.Helper()
		prefix, err := ensureLogFiles(directory, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(prefix+".stdout.log", []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	allocation := "alloc_" + strings.Repeat("a", 32)
	writeObject(allocation+"-111111111111", "first")
	writeObject(allocation+"-222222222222", "second")
	writeObject("alloc_"+strings.Repeat("b", 32)+"-333333333333", "other")
	if _, err := readPreviousLogs(directory, allocation, 10, "", now); !errors.Is(err, orchruntime.ErrNotFound) {
		t.Fatalf("previous logs before any removal = %v", err)
	}
	if err := retireObjectLogs(directory, allocation+"-111111111111", now); err != nil {
		t.Fatal(err)
	}
	if err := retireObjectLogs(directory, allocation+"-222222222222", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(directory, allocation+"-222222222222.stdout.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired log is still in the live directory: %v", err)
	}
	batch, err := readPreviousLogs(directory, allocation, 10, "", now.Add(2*time.Minute))
	if err != nil || len(batch.Frames) != 1 || string(batch.Frames[0].Data) != "second\n" || !batch.Exited {
		t.Fatalf("previous logs = %#v, %v", batch, err)
	}
	if _, err := readPreviousLogs(directory, "alloc_"+strings.Repeat("b", 32), 10, "", now); !errors.Is(err, orchruntime.ErrNotFound) {
		t.Fatalf("another allocation's previous logs = %v", err)
	}
	if _, err := readPreviousLogs(directory, allocation, 10, "", now.Add(retiredLogRetention+2*time.Minute)); !errors.Is(err, orchruntime.ErrNotFound) {
		t.Fatalf("previous logs after the retention = %v", err)
	}
	if entries, err := os.ReadDir(filepath.Join(directory, retiredLogsDirectory)); err != nil || len(entries) != 0 {
		t.Fatalf("expired retired logs = %v, %v", entries, err)
	}
}

func TestRetiredLogsStayWithinBudget(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	large := bytes.Repeat([]byte("x"), retiredLogBudget/2+1)
	for index, id := range []string{"alloc_one-111111111111", "alloc_two-222222222222", "alloc_three-333333333333"} {
		prefix, err := ensureLogFiles(directory, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(prefix+".stdout.log", large, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := retireObjectLogs(directory, id, now.Add(time.Duration(index)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	objects, err := retiredObjects(filepath.Join(directory, retiredLogsDirectory))
	if err != nil || len(objects) != 1 || objects[0].id != "alloc_three-333333333333" {
		t.Fatalf("retired objects within budget = %#v, %v", objects, err)
	}
}
