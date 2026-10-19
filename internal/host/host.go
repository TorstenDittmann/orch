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

	"github.com/TorstenDittmann/orch/internal/model"
	nodebundle "github.com/TorstenDittmann/orch/packaging/node"
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
	// arch is the bundle the entry came from; extraction checks executables
	// against it. It stays out of JSON so the recorded bundle hash is unchanged.
	arch string
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

// Artifacts returns the pinned dependency bundle for a Linux architecture. An
// unsupported architecture has no bundle; the doctor report already blocks it.
func Artifacts(arch string) ([]Artifact, error) {
	data, err := nodebundle.Files.ReadFile("artifacts.json")
	if err != nil {
		return nil, err
	}
	var bundles map[string][]Artifact
	if err := json.Unmarshal(data, &bundles); err != nil {
		return nil, err
	}
	result := bundles[arch]
	for i := range result {
		result[i].arch = arch
	}
	return result, nil
}
func address(raw string) (netip.Addr, error) {
	ip, err := netip.ParseAddr(raw)
	if err != nil || !model.IsMeshEndpointAddress(ip) {
		return netip.Addr{}, errors.New("advertise address must be a unicast IPv4 address on this host")
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
	return []string{"ca-certificates", "nftables", "iproute2", "libcap2-bin", "kmod", "passwd", "udev"}
}
func BuildPlan(ctx context.Context, raw string) (Plan, error) {
	ip, err := address(raw)
	if err != nil {
		return Plan{}, err
	}
	report := Doctor(ctx, ip.String())
	artifacts, err := Artifacts(report.Architecture)
	if err != nil {
		return Plan{}, err
	}
	return buildPlan(report, ip.String(), artifacts), nil
}

func buildPlan(report Report, address string, artifacts []Artifact) Plan {
	files := []string{Prefix + "/bin/orch", Prefix + "/containerd.toml", "/etc/systemd/system/orch-containerd.service", "/etc/systemd/system/orch-agent.service", "/etc/sysctl.d/90-orch.conf", "/etc/modules-load.d/orch.conf", ManifestPath, AgentDir, "/var/lib/orch-containerd", "/var/lib/orch-ingress"}
	for _, a := range artifacts {
		for _, f := range a.Files {
			files = append(files, Prefix+"/"+f)
		}
	}
	files = append(files, VethLinkPath)
	sort.Strings(files)
	actions := []string{"Install listed OS utilities from signed distribution repositories (no host upgrade)", "Download and verify pinned artifacts; copy this orch executable", "Create owned service/configuration files and private state directories", "Create non-login orch-ingress account and set Caddy low-port capability", "Enable IPv4 forwarding; load overlay and WireGuard modules", "Enroll using the supplied join artifact; enable and start owned systemd services", "Wait for the authenticated agent assignment and network readiness; retain state on timeout", "Leave host/provider firewall rules unchanged; allow peer UDP 51820 and ingress TCP 80/443 as needed"}
	actions = append(actions, "Install an owned veth link policy preserving CNI MAC addresses; reload udev rules without reconfiguring existing interfaces")
	if report.OS == "fedora" {
		actions = append(actions, "Install distribution container-selinux policy; label owned runtime binaries container_runtime_exec_t, runtime state container_var_lib_t/container_var_run_t and configuration container_config_t; restore labels without disabling SELinux or firewalld")
	}
	return Plan{Report: report, Address: address, Packages: packages(report.OS), Artifacts: artifacts, Files: files, Actions: actions}
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
	return (arch == "amd64" || arch == "arm64") && (osName == "ubuntu" && version == "24.04" || osName == "fedora" && version == "44")
}
func platform() string { return runtime.GOOS }

// blockersError lists the checks that stop installation, with their remedies.
func blockersError(kind string, report Report) error {
	var lines []string
	for _, check := range report.Checks {
		if (check.State == "fail" || check.State == "unknown") && !check.Installable {
			line := fmt.Sprintf("  %s: %s", check.ID, check.Detail)
			if check.Remedy != "" {
				line += " (" + check.Remedy + ")"
			}
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return fmt.Errorf("host has %s blockers", kind)
	}
	return fmt.Errorf("host has %s blockers:\n%s", kind, strings.Join(lines, "\n"))
}

// distributionDetail explains a failed distribution check. Any Fedora 44
// edition works except ostree-based ones, whose /usr is read-only.
func distributionDetail(os, version, arch string, ostree bool) string {
	detail := os + " " + version + " " + arch
	if os == "fedora" && ostree {
		detail += " (ostree-based Fedora is not supported)"
	}
	return detail
}

// selinuxMode reports enforcing, permissive or disabled from selinuxfs.
func selinuxMode(read func(string) ([]byte, error)) string {
	value, err := read("/sys/fs/selinux/enforce")
	switch {
	case err != nil:
		return "disabled"
	case strings.TrimSpace(string(value)) == "1":
		return "enforcing"
	default:
		return "permissive"
	}
}

// selinuxCheck turns the SELinux mode into a doctor check. orch labels its
// files whenever SELinux is enabled, and never changes the mode itself.
func selinuxCheck(mode string) Check {
	switch mode {
	case "enforcing":
		return Check{ID: "selinux", State: "pass", Detail: "SELinux enforcing; orch installs its file labels"}
	case "permissive":
		return Check{ID: "selinux", State: "warn", Detail: "SELinux permissive; orch installs its file labels so enforcing mode works later"}
	default:
		return Check{ID: "selinux", State: "warn", Detail: "SELinux disabled; orch skips file labels", Remedy: "Rerun the install after enabling SELinux to apply labels"}
	}
}
