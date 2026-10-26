//go:build !linux

package agent

import "errors"

func mountVolumeNoSymlinks(string, string) (func() error, error) {
	return nil, errors.New("volume backups need Linux")
}

func unmountVolumeView(string) error { return nil }

func canMountVolumes() bool { return false }

const archiveOpenFlags = 0

func isSymlinkLoop(error) bool { return false }
