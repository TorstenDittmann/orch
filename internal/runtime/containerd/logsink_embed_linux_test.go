//go:build linux

package containerd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNativeExecutableAcceptsOnlyThisArchitecture(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if !nativeExecutable(contents) {
		t.Fatal("nativeExecutable rejected the running test binary")
	}
	if nativeExecutable([]byte("#!/bin/sh\n")) {
		t.Fatal("nativeExecutable accepted a script")
	}
}

func TestInstallLogSinkWritesPrivateExecutable(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "log-sink")
	path, err := installLogSink(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, embedErr := embeddedLogSink.ReadFile("logsinkbin/orch-log-sink"); embedErr != nil {
		if path != "" {
			t.Fatalf("installLogSink() = %q without an embedded sink", path)
		}
		t.Skip("this build embeds no log sink")
	}
	if path == "" {
		t.Skip("the embedded log sink is for another architecture")
	}
	stale := filepath.Join(directory, logSinkPrefix+"stale")
	if err := os.WriteFile(stale, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	again, err := installLogSink(directory)
	if err != nil || again != path {
		t.Fatalf("second installLogSink() = %q, %v; want %q", again, err, path)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o700 {
		t.Fatalf("installed sink = %#v, %v", info, err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale sink was kept: %v", err)
	}
}
