//go:build linux

package containerd

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"embed"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// embeddedLogSink holds cmd/orch-log-sink when the build put it in
// logsinkbin (make build and make release do). Plain go build leaves only
// .gitkeep, and tasks then log through the orch binary itself.
//
//go:embed all:logsinkbin
var embeddedLogSink embed.FS

const logSinkPrefix = "orch-log-sink-"

// installLogSink writes the embedded log sink into directory under a name
// derived from its content and returns its path, or "" when this build carries
// no sink for this machine. Older copies are removed: sinks still running
// from them keep their executable until their task exits.
func installLogSink(directory string) (string, error) {
	contents, err := embeddedLogSink.ReadFile("logsinkbin/orch-log-sink")
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !nativeExecutable(contents) {
		return "", nil
	}
	if err := ensurePrivateDirectory(directory); err != nil {
		return "", err
	}
	digest := sha256.Sum256(contents)
	name := logSinkPrefix + hex.EncodeToString(digest[:8])
	path := filepath.Join(directory, name)
	if existing, err := os.ReadFile(path); err != nil || !bytes.Equal(existing, contents) {
		if err := writeExecutable(path, contents); err != nil {
			return "", err
		}
	}
	if entries, err := os.ReadDir(directory); err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), logSinkPrefix) && entry.Name() != name {
				_ = os.Remove(filepath.Join(directory, entry.Name()))
			}
		}
	}
	return path, nil
}

// nativeExecutable reports whether contents is a Linux executable for this
// machine's architecture, so a mismatched build never breaks task logging.
func nativeExecutable(contents []byte) bool {
	file, err := elf.NewFile(bytes.NewReader(contents))
	if err != nil {
		return false
	}
	machines := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}
	machine, known := machines[runtime.GOARCH]
	return known && file.Machine == machine
}

func writeExecutable(path string, contents []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".orch-log-sink-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	_, err = temporary.Write(contents)
	if err == nil {
		err = temporary.Chmod(0o700)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}
