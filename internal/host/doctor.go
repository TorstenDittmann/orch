package host

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	agentctl "change.codes/torsten/better-swarm/internal/agent"

	"change.codes/torsten/better-swarm/internal/ingress"
	"change.codes/torsten/better-swarm/internal/network"
)

// Probe separates read-only inspection from the installer mutation interface.
type Probe struct {
	Read        func(string) ([]byte, error)
	Exists      func(string) bool
	Run         Command
	GOOS, Arch  string
	UID         int
	Addresses   []string
	FreeBytes   uint64
	Trusted     func(string) error
	OwnListener func(string, string) bool
	Label       func(string) (string, error)
}

func Doctor(ctx context.Context, ip string) Report {
	addresses := []string{}
	if list, err := net.InterfaceAddrs(); err == nil {
		for _, a := range list {
			addresses = append(addresses, strings.Split(a.String(), "/")[0])
		}
	}
	report := Inspect(ctx, ip, Probe{Read: os.ReadFile, Exists: func(p string) bool { _, err := os.Lstat(p); return err == nil }, Run: runCommand, GOOS: runtime.GOOS, Arch: runtime.GOARCH, UID: os.Geteuid(), Addresses: addresses, FreeBytes: freeSpace("/var/lib"), Trusted: trustedInstalledFile, OwnListener: ownedListener, Label: securityLabel})
	if runtime.GOOS == "linux" {
		if _, err := os.Lstat(ManifestPath); err == nil {
			receipt, err := agentctl.ReadLocalStatus(AgentDir, time.Now())
			pid, _ := timeoutCommand(runCommand, ctx, "systemctl", "show", "orch-agent.service", "--property=MainPID", "--value")
			if err != nil || !receipt.Ready || !receipt.Connected || strings.TrimSpace(pid) != strconv.Itoa(receipt.PID) {
				report.Checks = append(report.Checks, Check{"agent_readiness", "fail", "No fresh authenticated, network-ready agent receipt", "Inspect journalctl -u orch-agent and controller reachability; install resumes waiting", true})
				report.Ready = false
			} else {
				report.Checks = append(report.Checks, Check{ID: "agent_readiness", State: "pass", Detail: "Authenticated assignment and networking ready for " + receipt.NodeID})
			}
		}
	}
	return report
}
func Inspect(ctx context.Context, ip string, p Probe) Report {
	r := Report{Architecture: p.Arch, Checks: []Check{}, Ready: true, Installable: true}
	add := func(id, state, detail, remedy string, fix bool) {
		r.Checks = append(r.Checks, Check{id, state, detail, remedy, fix})
		if state == "fail" || state == "unknown" {
			r.Ready = false
			if !fix {
				r.Installable = false
			}
		}
	}
	if p.GOOS != "linux" {
		add("platform", "fail", "Node installation requires Linux", "Run on Ubuntu 24.04 LTS or Fedora Server 44 amd64", false)
		return r
	}
	data, err := p.Read("/etc/os-release")
	if err == nil {
		r.OS, r.Version = Distro(string(data))
	}
	if !supported(r.OS, r.Version, p.Arch) || r.OS == "fedora" && (releaseValue(string(data), "VARIANT_ID") != "server" || p.Exists("/run/ostree-booted")) {
		add("distribution", "fail", r.OS+" "+r.Version+" "+p.Arch, "Use Ubuntu 24.04 LTS or Fedora Server 44 amd64", false)
	} else {
		add("distribution", "pass", r.OS+" "+r.Version, "", false)
	}
	if p.UID != 0 {
		add("privilege", "unknown", "Root-only checks are unavailable", "Run doctor with sudo before installation", false)
	} else {
		add("privilege", "pass", "Running as root", "", false)
	}
	comm, _ := p.Read("/proc/1/comm")
	if strings.TrimSpace(string(comm)) != "systemd" {
		add("systemd", "fail", "systemd is not PID 1", "Use a VM booted with systemd", false)
	} else {
		add("systemd", "pass", "systemd is PID 1", "", false)
	}
	controllers, err := p.Read("/sys/fs/cgroup/cgroup.controllers")
	if err != nil || !slices.Contains(strings.Fields(string(controllers)), "cpu") || !slices.Contains(strings.Fields(string(controllers)), "memory") || !slices.Contains(strings.Fields(string(controllers)), "pids") {
		add("cgroups", "fail", "Unified cgroups v2 with cpu/memory/pids controllers is required", "Enable cgroups v2 and reboot", false)
	} else {
		add("cgroups", "pass", "cgroups v2 controllers available", "", false)
	}
	found := false
	for _, a := range p.Addresses {
		if a == ip {
			found = true
		}
	}
	parsed, err := address(ip)
	if err != nil || !found {
		add("advertise_address", "fail", "Unicast IPv4 address is not assigned locally", "Choose a private or public address on this host that the other nodes can reach", false)
	} else {
		add("advertise_address", "pass", ip, "", false)
	}
	if p.FreeBytes < 2<<30 {
		add("disk", "fail", "At least 2 GiB free under /var/lib is required", "Free space for dependencies and workload images", false)
	} else {
		add("disk", "pass", fmt.Sprintf("%d MiB available", p.FreeBytes>>20), "", false)
	}
	forwarding, _ := p.Read("/proc/sys/net/ipv4/ip_forward")
	if strings.TrimSpace(string(forwarding)) != "1" {
		add("forwarding", "fail", "IPv4 forwarding is disabled", "Install writes its owned sysctl setting", true)
	} else {
		add("forwarding", "pass", "IPv4 forwarding enabled", "", false)
	}
	for _, module := range []string{"overlay", "wireguard"} {
		if p.Exists("/sys/module/" + module) {
			add("module_"+module, "pass", "Kernel module available", "", false)
			continue
		}
		if _, err := timeoutCommand(p.Run, ctx, "modinfo", module); err != nil {
			add("module_"+module, "unknown", "Kernel module availability could not be established", "Install a kernel providing "+module+" (and modinfo for inspection)", false)
		} else {
			add("module_"+module, "fail", "Kernel module is available but not loaded", "Install loads and persists this module", true)
		}
	}
	owned := p.Exists(ManifestPath)
	configured := false
	if data, err := p.Read(ManifestPath); err == nil {
		var state manifest
		if json.Unmarshal(data, &state) == nil {
			configured = state.Done["configuration"]
		}
	}
	if owned && p.Exists(Prefix+"/bin/caddy") {
		out, err := timeoutCommand(p.Run, ctx, "getcap", "-n", Prefix+"/bin/caddy")
		fields := strings.Fields(out)
		if err != nil || len(fields) != 2 || fields[0] != Prefix+"/bin/caddy" || fields[1] != "cap_net_bind_service=ep" {
			add("caddy_capability", "fail", "Caddy lacks its exact low-port capability", "Restore only cap_net_bind_service=ep on the owned Caddy binary", !configured)
		} else {
			add("caddy_capability", "pass", "Caddy has only cap_net_bind_service=ep", "", false)
		}
	}

	if !owned {
		if _, err := timeoutCommand(p.Run, ctx, "getent", "passwd", "orch-ingress"); err == nil {
			add("ownership", "fail", "Unowned orch-ingress account exists", "Use a fresh host; existing accounts are not adopted", false)
		}
		for _, path := range []string{Prefix, StateDir, AgentDir, VethLinkPath, "/var/lib/orch-containerd", "/var/lib/orch-ingress", "/etc/systemd/system/orch-agent.service", "/etc/systemd/system/orch-containerd.service", "/etc/sysctl.d/90-orch.conf", "/etc/modules-load.d/orch.conf"} {
			if p.Exists(path) {
				add("ownership", "fail", "Unowned installation path exists: "+path, "Use a fresh host; manual state takeover is unsupported", false)
			}
		}
		for _, service := range []string{"containerd.service", "docker.service", "caddy.service", "orch-agent.service", "orch-containerd.service"} {
			out, _ := timeoutCommand(p.Run, ctx, "systemctl", "show", service, "--property=LoadState", "--value")
			if strings.TrimSpace(out) == "loaded" {
				add("foreign_service", "fail", service+" already exists", "Use a fresh host; unrelated services are not replaced", false)
			}
		}
	}
	for _, item := range []struct {
		id, path, version string
		args              []string
	}{
		{"containerd", Prefix + "/bin/containerd", "2.3.4", []string{"--version"}},
		{"runc", Prefix + "/bin/runc", "1.4.3", []string{"--version"}},
		{"cni", Prefix + "/cni/bridge", network.CNIPluginsVersion, []string{"--version"}},
		{"caddy", Prefix + "/bin/caddy", ingress.RequiredCaddyVersion, []string{"version"}},
	} {
		if !p.Exists(item.path) {
			add(item.id, "fail", "Pinned "+item.id+" is not installed", "Install verified "+item.version+" artifact", true)
			continue
		}
		// Only run installed paths once the installation owns them.
		if !owned {
			continue
		}
		if p.Trusted == nil || p.Trusted(item.path) != nil {
			add(item.id, "fail", "Installed file ownership or checksum could not be verified", "Inspect installation manifest and owned files", false)
			continue
		}
		out, err := timeoutCommand(p.Run, ctx, item.path, item.args...)
		if err != nil || !strings.Contains(out, item.version) {
			add(item.id, "fail", "Installed version is unavailable or incompatible", "Inspect owned installation; upgrades are not supported", false)
		} else {
			add(item.id, "pass", item.version, "", false)
		}
	}
	for _, utility := range []string{"nft", "ip", "setcap"} {
		available := false
		for _, dir := range []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
			if p.Exists(filepath.Join(dir, utility)) {
				available = true
				break
			}
		}
		if !available {
			add("utility_"+utility, "fail", utility+" is missing", "Install supplies the distribution package", true)
		}
	}
	if r.OS == "fedora" {
		enforcing, _ := p.Read("/sys/fs/selinux/enforce")
		if strings.TrimSpace(string(enforcing)) != "1" {
			add("selinux", "fail", "SELinux enforcing is required for the Fedora installer", "Restore SELinux enforcing; the installer never disables it", false)
		} else {
			add("selinux", "pass", "SELinux enforcing; owned labels are installed with distribution policy", "", false)
		}
		if owned && configured {
			for _, label := range selinuxLabels() {
				path := strings.TrimSuffix(label[0], "(/.*)?")
				if !p.Exists(path) {
					continue
				}
				actual := ""
				if p.Label != nil {
					actual, _ = p.Label(path)
				}
				fields := strings.Split(actual, ":")
				if len(fields) < 3 || fields[2] != label[1] {
					add("selinux_context", "fail", "Unexpected SELinux context on "+path, "Restore the owned file-context mapping and run restorecon on this path", false)
				}
			}
		}
		out, _ := timeoutCommand(p.Run, ctx, "systemctl", "is-active", "firewalld")
		add("firewalld", "warn", "firewalld state: "+strings.TrimSpace(out), "Keep existing rules; allow mesh UDP 51820 between peers and ingress TCP 80/443 as needed", false)
	}
	out, err := timeoutCommand(p.Run, ctx, "ss", "-H", "-lntup")
	if err != nil {
		add("ports", "unknown", "Local socket inspection unavailable", "Install iproute utilities and rerun doctor", true)
	} else {
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(line)
			if len(f) < 5 {
				continue
			}
			endpoint := f[4]
			host, port, err := net.SplitHostPort(endpoint)
			if err != nil {
				continue
			}
			if port == "51820" || (port == "80" || port == "443") && (host == "0.0.0.0" || host == "*" || host == "::" || host == ip) || port == "53" && (host == "0.0.0.0" || host == "*" || host == "::") {
				if owned && p.OwnListener != nil && p.OwnListener(port, line) {
					continue
				}
				add("ports", "fail", "Conflicting listener: "+endpoint, "Resolve the existing listener before installing", false)
			}
		}
	}
	routes, err := timeoutCommand(p.Run, ctx, "ip", "-4", "route", "show")
	if err != nil {
		add("routes", "unknown", "Routes could not be inspected", "Install iproute utilities; verify workload CIDR does not overlap host routes", true)
	} else {
		add("routes", "warn", "Host routes inspected; controller workload CIDR is verified during agent initialization", "Ensure the controller workload CIDR does not overlap host/VPC routes; runtime rejects conflicts", false)
		_ = routes
	}
	add("peer_reachability", "warn", "Provider firewall and peer reachability are not verified by local inspection", "Allow UDP 51820 between advertised peer addresses; configure ingress access explicitly", false)
	if err == nil && found && !parsed.IsPrivate() {
		add("public_wireguard_endpoint", "warn", "WireGuard listens on a public address", "Allow UDP 51820 only from the other nodes' advertised addresses in the host or provider firewall", false)
	}
	return r
}

// Subprocess output is bounded; package-manager errors must not dump environment.
func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	resolved := name
	if !filepath.IsAbs(name) {
		resolved = ""
		for _, dir := range []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
			candidate := filepath.Join(dir, name)
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
				resolved = candidate
				break
			}
		}
		if resolved == "" {
			return "", fmt.Errorf("required command %s is unavailable", name)
		}
	}
	cmd := exec.CommandContext(ctx, resolved, args...)
	cmd.Env = append(os.Environ(), "PATH=/opt/orch/bin:/usr/sbin:/usr/bin:/sbin:/bin", "DEBIAN_FRONTEND=noninteractive")
	var output cappedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if err != nil {
		// These fixed local utilities never receive join credentials or repository
		// URLs. Include a short diagnostic; package-manager output may contain
		// authenticated mirror URLs and stays in the distribution's private logs.
		switch filepath.Base(name) {
		case "semanage", "restorecon", "modprobe", "systemd-analyze", "setcap", "useradd", "chown", "sysctl", "udevadm":
			detail := strings.TrimSpace(output.String())
			if len(detail) > 1024 {
				detail = detail[:1024] + "…"
			}
			return output.String(), fmt.Errorf("%s failed: %w: %s", filepath.Base(name), err, detail)
		}
		return output.String(), fmt.Errorf("%s failed: %w", filepath.Base(name), err)
	}
	return output.String(), nil
}

type cappedBuffer struct{ data []byte }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(b.data) < 64<<10 {
		keep := min(n, (64<<10)-len(b.data))
		b.data = append(b.data, p[:keep]...)
	}
	return n, nil
}
func (b *cappedBuffer) String() string { return string(b.data) }

func trustedInstalledFile(path string) error {
	if err := rootOwned(StateDir, true); err != nil {
		return err
	}
	if err := rootOwned(ManifestPath, false); err != nil {
		return err
	}
	if err := rootOwned(path, false); err != nil {
		return err
	}
	data, err := os.ReadFile(ManifestPath)
	if err != nil {
		return err
	}
	var state manifest
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	return verifyHash(path, state.Files[path])
}
