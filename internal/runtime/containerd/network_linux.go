//go:build linux

package containerd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func applicationResolverPath(path, application string) string {
	extension := filepath.Ext(path)
	base := strings.TrimSuffix(filepath.Base(path), extension)
	return filepath.Join(filepath.Dir(path), base+"-"+application+extension)
}

func writeResolverFile(path string, contents []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create resolver directory: %w", err)
	}
	directory, err := os.Lstat(filepath.Dir(path))
	if err != nil || !directory.IsDir() || directory.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("resolver directory must be private")
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".resolv-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	// The parent directory remains private to the root agent. The file itself
	// must be readable by arbitrary non-root workload identities after it is
	// bind-mounted into a container.
	if err := temporary.Chmod(0o644); err != nil {
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
	directoryHandle, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directoryHandle.Close()
	return directoryHandle.Sync()
}
