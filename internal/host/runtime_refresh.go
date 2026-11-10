package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// runtimeRefresh replaces a node's managed containerd with the one bundled in
// this orch release when the installed daemon is older. orch-containerd runs
// with KillMode=process, so restarting it leaves the shims, and with them every
// running container, in place.
type runtimeRefresh struct {
	prefix   string
	stateDir string
	arch     string
	run      Command
	fetch    func(context.Context, Artifact, string) error
}

func newRuntimeRefresh(run Command) runtimeRefresh {
	return runtimeRefresh{prefix: Prefix, stateDir: StateDir, arch: runtime.GOARCH, run: run, fetch: download}
}

var containerdVersionPattern = regexp.MustCompile(`(?:^|\s)v?(\d+\.\d+\.\d+)(?:\s|$)`)

// installedContainerdVersion parses `containerd --version` output such as
// "containerd github.com/containerd/containerd/v2 v2.3.4 1a2b3c".
func installedContainerdVersion(output string) string {
	match := containerdVersionPattern.FindStringSubmatch(output)
	if match == nil {
		return ""
	}
	return match[1]
}

// runtimeUndo puts back the containerd that a refresh replaced, for when the
// rest of the upgrade fails and the previous orch, which may accept only the
// older daemon, comes back.
type runtimeUndo struct {
	prefix   string
	stage    string
	targets  []string
	previous map[string]string
	run      Command
}

// revert restores the previous daemon and shim, restarts containerd and puts
// the previous digests back into files.
func (u *runtimeUndo) revert(ctx context.Context, files map[string]string) error {
	defer u.discard()
	for _, target := range u.targets {
		if err := replaceRegular(filepath.Join(u.stage, "previous", target), filepath.Join(u.prefix, target), 0755); err != nil {
			return fmt.Errorf("restore previous containerd: %w", err)
		}
	}
	if _, err := u.run(ctx, "systemctl", "restart", "orch-containerd.service"); err != nil {
		return fmt.Errorf("restart previous containerd: %w", err)
	}
	for path, digest := range u.previous {
		files[path] = digest
	}
	return nil
}

// discard drops the backup once the upgrade no longer needs it.
func (u *runtimeUndo) discard() { os.RemoveAll(u.stage) }

// apply refreshes containerd when needed. files is the installation
// manifest's owned-file map; it is updated with the new digests on success.
// It returns an undo when the daemon was replaced and nil when it was current.
func (r runtimeRefresh) apply(ctx context.Context, files map[string]string) (*runtimeUndo, error) {
	artifacts, err := Artifacts(r.arch)
	if err != nil {
		return nil, err
	}
	var bundled *Artifact
	for index := range artifacts {
		if strings.HasPrefix(artifacts[index].Name, "containerd-static-") {
			bundled = &artifacts[index]
		}
	}
	if bundled == nil {
		return nil, errors.New("the node bundle has no containerd")
	}
	daemon := filepath.Join(r.prefix, "bin", "containerd")
	output, err := r.run(ctx, daemon, "--version")
	if err != nil {
		return nil, fmt.Errorf("read installed containerd version: %w", err)
	}
	installed := installedContainerdVersion(output)
	if installed == "" {
		return nil, fmt.Errorf("unrecognized containerd version output %q", strings.TrimSpace(output))
	}
	if installed == strings.TrimPrefix(bundled.Version, "v") {
		return nil, nil
	}
	targets := make([]string, 0, len(bundled.Files))
	for _, target := range bundled.Files {
		targets = append(targets, target)
	}
	// The shim goes first: a new shim under the old daemon is fine within 2.3,
	// and the daemon only changes once its restart is the last step.
	sort.Slice(targets, func(i, j int) bool {
		return strings.Contains(targets[i], "shim") && !strings.Contains(targets[j], "shim")
	})
	for _, target := range targets {
		path := filepath.Join(r.prefix, target)
		recorded, owned := files[path]
		if !owned {
			return nil, fmt.Errorf("refusing unowned file: %s", path)
		}
		if err := verifyHash(path, recorded); err != nil {
			return nil, fmt.Errorf("owned content changed: %s", path)
		}
	}
	stage, err := os.MkdirTemp(r.stateDir, "runtime-refresh-*")
	if err != nil {
		return nil, err
	}
	undo := &runtimeUndo{prefix: r.prefix, stage: stage, targets: targets, previous: map[string]string{}, run: r.run}
	keep := false
	defer func() {
		if !keep {
			undo.discard()
		}
	}()
	downloaded := filepath.Join(stage, bundled.Name)
	if err := r.fetch(ctx, *bundled, downloaded); err != nil {
		return nil, err
	}
	unpacked := filepath.Join(stage, "unpacked")
	if err := extract(*bundled, downloaded, unpacked); err != nil {
		return nil, err
	}
	backup := filepath.Join(stage, "previous")
	for _, target := range targets {
		if err := copyRegular(filepath.Join(r.prefix, target), filepath.Join(backup, target), 0755); err != nil {
			return nil, err
		}
	}
	for _, target := range targets {
		path := filepath.Join(r.prefix, target)
		undo.previous[path] = files[path]
	}
	restore := func(cause error) (*runtimeUndo, error) {
		if err := undo.revert(ctx, files); err != nil {
			return nil, fmt.Errorf("%v; %w", cause, err)
		}
		return nil, cause
	}
	for _, target := range targets {
		if err := replaceRegular(filepath.Join(unpacked, target), filepath.Join(r.prefix, target), 0755); err != nil {
			return restore(err)
		}
	}
	if _, err := r.run(ctx, "systemctl", "restart", "orch-containerd.service"); err != nil {
		return restore(fmt.Errorf("restart containerd %s: %w", bundled.Version, err))
	}
	for _, target := range targets {
		digest, err := hashFile(filepath.Join(r.prefix, target))
		if err != nil {
			return restore(err)
		}
		files[filepath.Join(r.prefix, target)] = digest
	}
	keep = true
	return undo, nil
}
