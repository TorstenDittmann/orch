package host

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// commandLinkPath puts orch on PATH for root and sudo. It is a variable so
// tests can point it into a temporary directory.
var commandLinkPath = "/usr/local/bin/orch"

// linkCommand points commandLinkPath at an installed orch binary. A link to
// another orch install is replaced only when preferred is set or its target is
// gone, so the controller's binary wins on a host that runs both. A copy of
// the target is replaced too. Anything else is left alone.
func linkCommand(target string, preferred bool) (bool, error) {
	current, err := os.Readlink(commandLinkPath)
	switch {
	case err == nil:
		if current == target {
			return false, nil
		}
		if !ownedCommandTarget(current) {
			return false, nil
		}
		if _, statErr := os.Stat(current); statErr == nil && !preferred {
			return false, nil
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		info, lstatErr := os.Lstat(commandLinkPath)
		if lstatErr != nil {
			return false, err
		}
		// A copy of the installed binary, such as the one install.sh puts on
		// PATH, gives way to the link. Any other file belongs to the operator.
		if !info.Mode().IsRegular() || !sameContents(commandLinkPath, target) {
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(commandLinkPath), 0755); err != nil {
		return false, err
	}
	staged := commandLinkPath + ".orch-new"
	_ = os.Remove(staged)
	if err := os.Symlink(target, staged); err != nil {
		return false, err
	}
	if err := os.Rename(staged, commandLinkPath); err != nil {
		_ = os.Remove(staged)
		return false, err
	}
	return true, nil
}

func sameContents(a, b string) bool {
	left, err := os.ReadFile(a)
	if err != nil {
		return false
	}
	right, err := os.ReadFile(b)
	return err == nil && bytes.Equal(left, right)
}

func ownedCommandTarget(target string) bool {
	return strings.HasPrefix(target, Prefix+"/") || strings.HasPrefix(target, ControllerPrefix+"/")
}

// ensureCommandLink links orch onto PATH and reports, but never fails on, a
// problem: a missing link costs convenience, not a working host.
func ensureCommandLink(target string, preferred bool, progress io.Writer) {
	changed, err := linkCommand(target, preferred)
	if progress == nil {
		return
	}
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(progress, "Warning: could not link %s to %s: %v\n", commandLinkPath, target, err)
	case changed:
		_, _ = fmt.Fprintf(progress, "Linked %s to %s\n", commandLinkPath, target)
	}
}
