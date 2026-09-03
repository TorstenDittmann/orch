//go:build linux

package containerd

import (
	"bytes"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
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
