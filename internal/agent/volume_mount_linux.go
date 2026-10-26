//go:build linux

package agent

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// archiveOpenFlags keep a backup's opens from blocking on a FIFO or
// following a final symlink.
const archiveOpenFlags = unix.O_NONBLOCK | unix.O_NOFOLLOW

func isSymlinkLoop(err error) bool { return errors.Is(err, unix.ELOOP) }

// mountVolumeNoSymlinks bind-mounts a volume at an empty mountpoint where the
// kernel refuses to follow symlinks. The workload can change its volume while
// a backup reads it as root; through this mount a file it swaps for a symlink
// to a host path fails to open instead of being archived. The mount is
// private so it never propagates into other mount namespaces.
func mountVolumeNoSymlinks(source, mountpoint string) (unmount func() error, err error) {
	if err := unix.Mount(source, mountpoint, "", unix.MS_BIND, ""); err != nil {
		return nil, fmt.Errorf("bind volume for backup: %w", err)
	}
	unmount = func() error { return unmountVolumeView(mountpoint) }
	if err := unix.Mount("", mountpoint, "", unix.MS_PRIVATE, ""); err != nil {
		return nil, errors.Join(fmt.Errorf("make volume backup view private: %w", err), unmount())
	}
	flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_NOSYMFOLLOW | unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
	if err := unix.Mount("", mountpoint, "", flags, ""); err != nil {
		return nil, errors.Join(fmt.Errorf("refuse symlinks in volume backup view: %w", err), unmount())
	}
	return unmount, nil
}

// unmountVolumeView detaches a backup view. EINVAL means it is not mounted.
func unmountVolumeView(mountpoint string) error {
	if err := unix.Unmount(mountpoint, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("unmount volume backup view: %w", err)
	}
	return nil
}

// canMountVolumes reports whether this process may create the backup view.
// Without root a backup reads only what its own user can, so a planted
// symlink gains nothing.
func canMountVolumes() bool { return os.Geteuid() == 0 }
