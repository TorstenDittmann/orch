//go:build linux

package host

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"strings"
	"syscall"
)

func rootOwned(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 || info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) {
		return fmt.Errorf("%s must be root owned, without group/world write permission and with the expected file type", path)
	}
	return noSymlinks(path)
}

func securityLabel(path string) (string, error) {
	data := make([]byte, 4096)
	n, err := unix.Getxattr(path, "security.selinux", data)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data[:n]), "\x00"), nil
}
