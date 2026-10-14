//go:build !windows

package cluster

import (
	"fmt"
	"os"
	"syscall"
)

// matchOwner gives path the owner of reference when they differ, so files
// written by root into a service account's directory stay readable by it.
func matchOwner(path, reference string) error {
	want, err := os.Lstat(reference)
	if err != nil {
		return err
	}
	have, err := os.Lstat(path)
	if err != nil {
		return err
	}
	wantStat, wantOK := want.Sys().(*syscall.Stat_t)
	haveStat, haveOK := have.Sys().(*syscall.Stat_t)
	if !wantOK || !haveOK {
		return fmt.Errorf("file ownership is unavailable for %s", path)
	}
	if wantStat.Uid == haveStat.Uid && wantStat.Gid == haveStat.Gid {
		return nil
	}
	return os.Lchown(path, int(wantStat.Uid), int(wantStat.Gid))
}
