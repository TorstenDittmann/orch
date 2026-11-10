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

// apply refreshes containerd when needed. files is the installation
// manifest's owned-file map; it is updated with the new digests on success.
// It reports whether the daemon was replaced.
func (r runtimeRefresh) apply(ctx context.Context, files map[string]string) (bool, error) {
	artifacts, err := Artifacts(r.arch)
	if err != nil {
		return false, err
	}
	var bundled *Artifact
	for index := range artifacts {
		if strings.HasPrefix(artifacts[index].Name, "containerd-static-") {
			bundled = &artifacts[index]
		}
	}
	if bundled == nil {
		return false, errors.New("the node bundle has no containerd")
	}
	daemon := filepath.Join(r.prefix, "bin", "containerd")
	output, err := r.run(ctx, daemon, "--version")
	if err != nil {
		return false, fmt.Errorf("read installed containerd version: %w", err)
	}
	installed := installedContainerdVersion(output)
	if installed == "" {
		return false, fmt.Errorf("unrecognized containerd version output %q", strings.TrimSpace(output))
	}
	if installed == strings.TrimPrefix(bundled.Version, "v") {
		return false, nil
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
			return false, fmt.Errorf("refusing unowned file: %s", path)
		}
		if err := verifyHash(path, recorded); err != nil {
			return false, fmt.Errorf("owned content changed: %s", path)
		}
	}
	stage, err := os.MkdirTemp(r.stateDir, "runtime-refresh-*")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(stage)
	downloaded := filepath.Join(stage, bundled.Name)
	if err := r.fetch(ctx, *bundled, downloaded); err != nil {
		return false, err
	}
	unpacked := filepath.Join(stage, "unpacked")
	if err := extract(*bundled, downloaded, unpacked); err != nil {
		return false, err
	}
	backup := filepath.Join(stage, "previous")
	for _, target := range targets {
		if err := copyRegular(filepath.Join(r.prefix, target), filepath.Join(backup, target), 0755); err != nil {
			return false, err
		}
	}
	restore := func(cause error) (bool, error) {
		for _, target := range targets {
			if err := replaceRegular(filepath.Join(backup, target), filepath.Join(r.prefix, target), 0755); err != nil {
				return false, fmt.Errorf("%v; restore previous containerd: %w", cause, err)
			}
		}
		if _, err := r.run(ctx, "systemctl", "restart", "orch-containerd.service"); err != nil {
			return false, fmt.Errorf("%v; restart previous containerd: %w", cause, err)
		}
		return false, cause
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
			return true, err
		}
		files[filepath.Join(r.prefix, target)] = digest
	}
	return true, nil
}
