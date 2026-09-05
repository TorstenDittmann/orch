//go:build linux || darwin

package host

import "golang.org/x/sys/unix"

func freeSpace(path string) uint64 {
	var s unix.Statfs_t
	if unix.Statfs(path, &s) != nil {
		return 0
	}
	return s.Bavail * uint64(s.Bsize)
}
