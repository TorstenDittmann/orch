// Package host implements local, explicitly owned node installation.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	nodebundle "change.codes/torsten/better-swarm/packaging/node"
)

const Prefix = "/opt/orch"
const StateDir = "/var/lib/orch-install"
const AgentDir = "/var/lib/orch-agent"
const VethLinkPath = "/etc/systemd/network/10-orch-veth.link"
const ManifestPath = StateDir + "/manifest.json"

type Artifact struct {
	Name    string            `json:"name"`
	Version string            `json:"version"`
	URL     string            `json:"url"`
	SHA256  string            `json:"sha256"`
	Archive string            `json:"archive"`
	Files   map[string]string `json:"files"`
	License string            `json:"license"`
	Source  string            `json:"source"`
}
type Check struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	Detail      string `json:"detail"`
	Remedy      string `json:"remedy,omitempty"`
	Installable bool   `json:"installable,omitempty"`
}
type Report struct {
	OS           string  `json:"os"`
	Version      string  `json:"version"`
	Architecture string  `json:"architecture"`
	Checks       []Check `json:"checks"`
	Ready        bool    `json:"ready"`
	Installable  bool    `json:"installable"`
}
type Plan struct {
	Report    Report     `json:"report"`
	Address   string     `json:"advertise_address"`
	Packages  []string   `json:"packages"`
	Artifacts []Artifact `json:"artifacts"`
	Files     []string   `json:"files"`
	Actions   []string   `json:"actions"`
}
type Command func(context.Context, string, ...string) (string, error)

func Artifacts() ([]Artifact, error) {
	data, err := nodebundle.Files.ReadFile("artifacts.json")
	if err != nil {
		return nil, err
	}
	var result []Artifact
	err = json.Unmarshal(data, &result)
	return result, err
}
func address(raw string) (netip.Addr, error) {
	ip, err := netip.ParseAddr(raw)
	if err != nil || !ip.Is4() || !ip.IsPrivate() || ip.IsLoopback() {
		return netip.Addr{}, errors.New("advertise address must be a private IPv4 address on this host")
	}
	return ip, nil
}
func Distro(data string) (string, string) {
	return releaseValue(data, "ID"), releaseValue(data, "VERSION_ID")
}
func releaseValue(data, name string) string {
	for _, line := range strings.Split(data, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && key == name {
			return strings.Trim(value, "\"'")
		}
	}
	return ""
}

func packages(distro string) []string {
	if distro == "fedora" {
		return []string{"ca-certificates", "nftables", "iproute", "libcap", "kmod", "shadow-utils", "container-selinux", "policycoreutils-python-utils", "systemd-udev"}
	}
	return []string{"ca-certificates", "nftables", "iproute2", "libcap2-bin", "kmod", "passwd"}
}
func BuildPlan(ctx context.Context, raw string) (Plan, error) {
	ip, err := address(raw)
	if err != nil {
		return Plan{}, err
	}
	report := Doctor(ctx, ip.String())
	artifacts, err := Artifacts()
	if err != nil {
		return Plan{}, err
	}
	files := []string{Prefix + "/bin/orch", Prefix + "/containerd.toml", "/etc/systemd/system/orch-containerd.service", "/etc/systemd/system/orch-agent.service", "/etc/sysctl.d/90-orch.conf", "/etc/modules-load.d/orch.conf", ManifestPath, AgentDir, "/var/lib/orch-containerd", "/var/lib/orch-ingress"}
	for _, a := range artifacts {
		for _, f := range a.Files {
			files = append(files, Prefix+"/"+f)
		}
	}
	if report.OS == "fedora" {
		files = append(files, VethLinkPath)
	}
	sort.Strings(files)
	actions := []string{"Install listed OS utilities from signed distribution repositories (no host upgrade)", "Download and verify pinned artifacts; copy this orch executable", "Create owned service/configuration files and private state directories", "Create non-login orch-ingress account and set Caddy low-port capability", "Enable IPv4 forwarding; load overlay and WireGuard modules", "Enroll using the supplied join artifact; enable and start owned systemd services", "Wait for the authenticated agent assignment and network readiness; retain state on timeout", "Leave host/provider firewall rules unchanged; allow peer UDP 51820 and ingress TCP 80/443 as needed"}
	if report.OS == "fedora" {
		actions = append(actions, "Install an owned veth link policy preserving CNI MAC addresses; reload udev rules without reconfiguring existing interfaces")
		actions = append(actions, "Install distribution container-selinux policy; label owned runtime binaries container_runtime_exec_t, runtime state container_var_lib_t/container_var_run_t and configuration container_config_t; restore labels without disabling SELinux or firewalld")
	}
	return Plan{Report: report, Address: ip.String(), Packages: packages(report.OS), Artifacts: artifacts, Files: files, Actions: actions}, nil
}
func privateRegular(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return nil, fmt.Errorf("%s must be a regular file without group/world write permission", path)
	}
	return info, nil
}
func noSymlinks(path string) error {
	path = filepath.Clean(path)
	for {
		info, err := os.Lstat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in owned path: %s", path)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}
func timeoutCommand(run Command, ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return run(ctx, name, args...)
}
func supported(osName, version, arch string) bool {
	return arch == "amd64" && (osName == "ubuntu" && version == "24.04" || osName == "fedora" && version == "44")
}
func platform() string { return runtime.GOOS }
