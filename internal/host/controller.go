package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	clusterctl "change.codes/torsten/better-swarm/internal/cluster"
	"change.codes/torsten/better-swarm/internal/security"
	"change.codes/torsten/better-swarm/internal/store"
	controllerbundle "change.codes/torsten/better-swarm/packaging/controller"
)

var controllerName = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

const ControllerPrefix = "/opt/orch-controller"
const ControllerDataDir = "/var/lib/orch-controller"
const ControllerStateDir = "/var/lib/orch-controller-install"
const ControllerManifestPath = ControllerStateDir + "/manifest.json"
const ControllerUnitPath = "/etc/systemd/system/orch-controller.service"

type ControllerOptions struct {
	Name, Server, Listen, AdminDir, RecoveryDir string
}

type ControllerPlan struct {
	Report  Report   `json:"report"`
	Name    string   `json:"name,omitempty"`
	Server  string   `json:"server"`
	Listen  string   `json:"listen"`
	Files   []string `json:"files"`
	Actions []string `json:"actions"`
}

func normalizeControllerOptions(options ControllerOptions, requireOutputs bool) (ControllerOptions, error) {
	options.Name = strings.TrimSpace(options.Name)
	options.Server = strings.TrimSpace(options.Server)
	options.Listen = strings.TrimSpace(options.Listen)
	if options.Server == "" || options.Listen == "" || requireOutputs && (options.Name == "" || options.AdminDir == "" || options.RecoveryDir == "") {
		return ControllerOptions{}, errors.New("controller name, HTTPS server, listen address, admin directory, and recovery directory are required")
	}
	if requireOutputs && !controllerName.MatchString(options.Name) {
		return ControllerOptions{}, errors.New("controller name must be a lowercase DNS label of at most 63 characters")
	}
	parsed, err := url.ParseRequestURI(options.Server)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return ControllerOptions{}, errors.New("server must be an absolute HTTPS URL without credentials, path, query, or fragment")
	}
	if ip := net.ParseIP(parsed.Hostname()); ip != nil && (ip.IsUnspecified() || ip.IsMulticast() || ip.IsLoopback()) {
		return ControllerOptions{}, errors.New("server endpoint must be a reachable unicast address")
	} else if ip == nil && !validControllerHostname(strings.ToLower(parsed.Hostname())) {
		return ControllerOptions{}, errors.New("server URL has an invalid hostname")
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return ControllerOptions{}, errors.New("server URL has an invalid port")
		}
	}
	host, port, err := net.SplitHostPort(options.Listen)
	if err != nil {
		return ControllerOptions{}, errors.New("listen must be HOST:PORT")
	}
	value, err := strconv.ParseUint(port, 10, 16)
	if err != nil || value < 1024 {
		return ControllerOptions{}, errors.New("controller listen port must be between 1024 and 65535")
	}
	if host != "" && net.ParseIP(host) == nil {
		return ControllerOptions{}, errors.New("listen host must be an IP address")
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsMulticast() {
		return ControllerOptions{}, errors.New("listen host cannot be multicast")
	}
	if requireOutputs {
		for _, field := range []*string{&options.AdminDir, &options.RecoveryDir} {
			if !filepath.IsAbs(*field) {
				return ControllerOptions{}, errors.New("admin and recovery directories must be absolute")
			}
			*field = filepath.Clean(*field)
		}
		if options.AdminDir == options.RecoveryDir || hostPathsOverlap(options.AdminDir, options.RecoveryDir) ||
			hostPathsOverlap(options.AdminDir, ControllerDataDir) || hostPathsOverlap(options.RecoveryDir, ControllerDataDir) ||
			hostPathsOverlap(options.AdminDir, ControllerStateDir) || hostPathsOverlap(options.RecoveryDir, ControllerStateDir) {
			return ControllerOptions{}, errors.New("admin, recovery, controller data, and installation state paths must not overlap")
		}
	}
	return options, nil
}

func BuildControllerPlan(ctx context.Context, options ControllerOptions) (ControllerPlan, error) {
	normalized, err := normalizeControllerOptions(options, options.Name != "" || options.AdminDir != "" || options.RecoveryDir != "")
	if err != nil {
		return ControllerPlan{}, err
	}
	report := ControllerDoctor(ctx, normalized.Server, normalized.Listen)
	return ControllerPlan{Report: report, Name: normalized.Name, Server: normalized.Server, Listen: normalized.Listen,
		Files: []string{ControllerPrefix + "/bin/orch", ControllerDataDir, ControllerManifestPath, ControllerUnitPath, normalized.AdminDir, normalized.RecoveryDir},
		Actions: []string{
			"Create a dedicated non-login orch-controller account",
			"Copy this trusted orch executable into the controller-owned bundle",
			"Initialize private controller state and a portable administrator directory",
			"Validate and enable the owned systemd controller service",
			"Wait for an authenticated controller response",
			"Publish a verified day-zero recovery set for immediate off-host transfer",
			"Leave firewall, DNS, routing, sysctl, and unrelated services unchanged",
		}}, nil
}

// controllerCertificateWarning is how early doctor warns before expiry.
const controllerCertificateWarning = 30 * 24 * time.Hour

// controllerCertificateCheck reports the installed controller leaf lifetime.
func controllerCertificateCheck(dataDir string, now time.Time) (state, detail, remedy string) {
	remedy = "Run orch cluster certificates renew --data-dir " + dataDir + " --admin-dir NEW_DIR, then restart orch-controller"
	contents, err := os.ReadFile(filepath.Join(dataDir, "pki", "controller.pem"))
	if err != nil {
		return "fail", "Controller certificate is unreadable", "Inspect the private controller identity directory"
	}
	notAfter, err := security.CertificateNotAfter(contents)
	if err != nil {
		return "fail", "Controller certificate is invalid", remedy
	}
	expiry := notAfter.UTC().Format(time.RFC3339)
	switch {
	case !now.Before(notAfter):
		return "fail", "Controller certificate expired at " + expiry, remedy
	case notAfter.Sub(now) < controllerCertificateWarning:
		return "warn", "Controller certificate expires at " + expiry, remedy
	default:
		return "pass", "Controller certificate valid until " + expiry, ""
	}
}

func ControllerDoctor(ctx context.Context, server, listen string) Report {
	r := Report{Architecture: runtime.GOARCH, Ready: true, Installable: true}
	add := func(id, state, detail, remedy string, installable bool) {
		r.Checks = append(r.Checks, Check{ID: id, State: state, Detail: detail, Remedy: remedy, Installable: installable})
		if state == "fail" || state == "unknown" {
			r.Ready = false
			if !installable {
				r.Installable = false
			}
		}
	}
	options, optionErr := normalizeControllerOptions(ControllerOptions{Server: server, Listen: listen}, false)
	if optionErr != nil {
		add("controller_addresses", "fail", optionErr.Error(), "Use a reachable HTTPS endpoint and unprivileged IP listen address", false)
		return r
	}
	data, err := os.ReadFile("/etc/os-release")
	if err == nil {
		r.OS, r.Version = Distro(string(data))
	}
	if runtime.GOOS != "linux" || !supported(r.OS, r.Version, runtime.GOARCH) || r.OS == "fedora" && (releaseValue(string(data), "VARIANT_ID") != "server" || pathExistsLocal("/run/ostree-booted")) {
		add("distribution", "fail", r.OS+" "+r.Version+" "+r.Architecture, "Use Ubuntu 24.04 LTS or Fedora Server 44 amd64", false)
	} else {
		add("distribution", "pass", r.OS+" "+r.Version, "", false)
	}
	if os.Geteuid() != 0 {
		add("privilege", "unknown", "Root-only ownership checks are unavailable", "Run with sudo before installation", false)
	} else {
		add("privilege", "pass", "Running as root", "", false)
	}
	if now := time.Now().UTC(); now.Year() < 2024 || now.Year() > 2100 {
		add("clock", "fail", "System time is outside the supported certificate window", "Synchronize the host clock before initialization", false)
	} else {
		add("clock", "pass", "System time is plausible for certificate generation", "", false)
	}
	if available := freeSpace("/var/lib"); available < 512<<20 {
		add("controller_disk", "fail", "At least 512 MiB free under /var/lib is required", "Free space for controller state and backups", false)
	} else {
		add("controller_disk", "pass", fmt.Sprintf("%d MiB available", available>>20), "", false)
	}
	comm, _ := os.ReadFile("/proc/1/comm")
	if strings.TrimSpace(string(comm)) != "systemd" {
		add("systemd", "fail", "systemd is not PID 1", "Use a supported host booted with systemd", false)
	} else {
		add("systemd", "pass", "systemd is PID 1", "", false)
	}
	owned := pathExistsLocal(ControllerManifestPath)
	if !owned {
		add("controller_installation", "fail", "The managed controller is not installed", "Run cluster install with explicit administrator and recovery directories", true)
		if _, err := timeoutCommand(runCommand, ctx, "getent", "passwd", "orch-controller"); err == nil {
			add("controller_account", "fail", "Unowned orch-controller account exists", "Use a fresh host; existing accounts are not adopted", false)
		}
		for _, path := range []string{ControllerPrefix, ControllerDataDir, ControllerStateDir, ControllerUnitPath} {
			if pathExistsLocal(path) {
				add("controller_ownership", "fail", "Unowned controller path exists: "+path, "Use a fresh path; manual controller state is not adopted", false)
			}
		}
	}
	_, port, _ := net.SplitHostPort(options.Listen)
	out, ssErr := timeoutCommand(runCommand, ctx, "ss", "-H", "-lntp")
	if ssErr != nil {
		add("controller_listener", "unknown", "TCP listeners could not be inspected", "Install iproute utilities and verify the port is free", false)
	} else {
		conflict := ""
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				_, candidate, err := net.SplitHostPort(fields[3])
				if err == nil && candidate == port && !(owned && ownedControllerListener(line)) {
					conflict = fields[3]
					break
				}
			}
		}
		if conflict != "" {
			add("controller_listener", "fail", "Conflicting TCP listener: "+conflict, "Choose a free port or stop the foreign service", false)
		} else {
			add("controller_listener", "pass", "No foreign listener uses the configured port", "", false)
		}
	}
	add("controller_reachability", "warn", "External DNS, routing and firewall reachability are not proven locally", "Route the server endpoint to this listener and allow trusted clients", false)
	if owned {
		var state controllerManifest
		blob, err := os.ReadFile(ControllerManifestPath)
		if err != nil || json.Unmarshal(blob, &state) != nil {
			add("controller_manifest", "fail", "Controller installation manifest is invalid", "Inspect the owned installation state", false)
		} else if state.Server != options.Server || state.Listen != options.Listen {
			add("controller_manifest", "fail", "Installed controller intent differs", "Reconfiguration is not supported by cluster install", false)
		} else {
			add("controller_manifest", "pass", "Owned controller intent matches", "", false)
			if state.Done["administrator"] {
				if err := validatePrivateTree(state.AdminDir); err != nil {
					add("administrator_output", "fail", err.Error(), "Restore the private administrator handoff directory", false)
				} else {
					add("administrator_output", "pass", "Administrator handoff directory is private", "", false)
				}
			}
			if state.Done["recovery"] {
				if err := validatePrivateTree(state.RecoveryDir); err != nil {
					add("recovery_output", "fail", err.Error(), "Restore the immutable recovery directory", false)
				} else {
					add("recovery_output", "pass", "Recovery directory is private", "", false)
				}
			}
			for path, digest := range state.Files {
				if err := rootOwned(path, false); err != nil || verifyHash(path, digest) != nil {
					resumable := path == ControllerPrefix+"/bin/orch" && !state.Done["binary"] || path == ControllerUnitPath && !state.Done["configuration"]
					add("controller_files", "fail", "Owned controller file failed ownership or hash validation", "Rerun the matching installation if publication was interrupted", resumable)
					break
				}
			}
			if os.Geteuid() == 0 {
				database, openErr := store.OpenInitialized(ctx, filepath.Join(ControllerDataDir, clusterctl.DatabaseFilename))
				if openErr != nil {
					add("controller_state", "fail", "Controller database is unavailable", "Inspect the private controller data directory", false)
				} else {
					cluster, clusterErr := database.Cluster(ctx)
					fingerprint, fingerprintErr := database.SecretKeyFingerprint(ctx)
					closeErr := database.Close()
					box, keyErr := security.LoadSecretBox(filepath.Join(ControllerDataDir, clusterctl.SecretKeyFilename), nil)
					if clusterErr != nil || fingerprintErr != nil || closeErr != nil || keyErr != nil || cluster.ID != state.ClusterID || fingerprint == "" || fingerprint != box.Fingerprint() {
						add("controller_state", "fail", "Controller database, identity, or secret key does not match the installation", "Inspect controller state and recovery material", false)
					} else {
						add("controller_state", "pass", "Controller state and secret key match cluster "+cluster.ID, "", false)
					}
				}
				certificateState, detail, remedy := controllerCertificateCheck(ControllerDataDir, time.Now())
				add("controller_certificate", certificateState, detail, remedy, false)
				if err := exactControllerProcess(ctx, state.Listen); err != nil {
					add("controller_service", "fail", err.Error(), "Inspect systemctl status and journalctl -u orch-controller", true)
				} else {
					add("controller_service", "pass", "Owned controller process has the configured listener", "", false)
				}
			}
		}
	}
	if r.OS == "fedora" {
		enforcing, _ := os.ReadFile("/sys/fs/selinux/enforce")
		if strings.TrimSpace(string(enforcing)) != "1" {
			add("selinux", "fail", "SELinux enforcing is required for supported Fedora installation", "Restore enforcing mode; installation never disables SELinux", false)
		} else {
			add("selinux", "pass", "SELinux enforcing", "", false)
		}
		firewall, err := timeoutCommand(runCommand, ctx, "systemctl", "is-active", "firewalld")
		if err == nil && strings.TrimSpace(firewall) == "active" {
			r.Checks = append(r.Checks, Check{ID: "firewalld", State: "warn", Detail: "firewalld is active and remains operator-managed", Remedy: "Allow the controller port from trusted operators and workers"})
		}
	}
	return r
}

func pathExistsLocal(path string) bool { _, err := os.Lstat(path); return err == nil }

func ownedControllerListener(line string) bool {
	pidMatches := listenerPID.FindAllStringSubmatch(line, -1)
	for _, match := range pidMatches {
		executable, err := os.Readlink("/proc/" + match[1] + "/exe")
		if err == nil && executable == ControllerPrefix+"/bin/orch" {
			return true
		}
	}
	return false
}

func controllerUnit(listen string) ([]byte, error) {
	data, err := controllerbundle.Files.ReadFile("orch-controller.service")
	if err != nil {
		return nil, err
	}
	return []byte(strings.ReplaceAll(string(data), "{{LISTEN}}", listen)), nil
}

func controllerUID(ctx context.Context) (int, int, error) {
	out, err := runCommand(ctx, "getent", "passwd", "orch-controller")
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Split(strings.TrimSpace(out), ":")
	if len(fields) != 7 || fields[2] == "0" || fields[3] == "0" || fields[5] != ControllerDataDir || !strings.HasSuffix(fields[6], "/nologin") {
		return 0, 0, errors.New("orch-controller account conflicts with required identity")
	}
	uid, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(fields[3])
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

func validateControllerOutputPath(path string) error {
	if filepath.Dir(path) == path {
		return fmt.Errorf("output directory cannot be a filesystem root")
	}
	return noSymlinks(filepath.Dir(path))
}

func hostPathsOverlap(first, second string) bool {
	within := func(path, parent string) bool {
		relative, err := filepath.Rel(parent, path)
		return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
	}
	return within(first, second) || within(second, first)
}

func validControllerHostname(host string) bool {
	if host == "" || host == "localhost" || len(host) > 253 || strings.HasSuffix(host, ".") {
		return false
	}
	label := regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	for _, part := range strings.Split(host, ".") {
		if !label.MatchString(part) {
			return false
		}
	}
	return true
}
