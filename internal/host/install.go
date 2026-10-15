package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	agentctl "change.codes/torsten/better-swarm/internal/agent"
	"change.codes/torsten/better-swarm/internal/buildinfo"
	nodebundle "change.codes/torsten/better-swarm/packaging/node"
)

type InstallOptions struct {
	Address     string
	JoinFile    string
	JoinInput   io.Reader
	WaitTimeout time.Duration
	Progress    io.Writer
}
type InstallResult struct {
	NodeID  string `json:"node_id"`
	Ready   bool   `json:"ready"`
	Changed bool   `json:"changed"`
	Phase   string `json:"phase"`
	DataDir string `json:"data_dir"`
	Ingress *bool  `json:"ingress,omitempty"`
}
type manifest struct {
	Version    int                  `json:"version"`
	Address    string               `json:"advertise_address"`
	Distro     string               `json:"distro"`
	OSVersion  string               `json:"os_version"`
	BinaryHash string               `json:"binary_hash"`
	BundleHash string               `json:"bundle_hash"`
	Files      map[string]string    `json:"files"`
	Done       map[string]bool      `json:"done"`
	NodeID     string               `json:"node_id,omitempty"`
	JoinHash   string               `json:"join_hash"`
	Build      buildinfo.Descriptor `json:"build,omitempty"`
}
type installer struct {
	plan        Plan
	state       manifest
	run         Command
	changed     bool
	journalPath string // Defaults to ManifestPath; overridden only by unit tests.
}

func Install(ctx context.Context, options InstallOptions) (InstallResult, error) {
	if platform() != "linux" || os.Geteuid() != 0 {
		return InstallResult{}, errors.New("node install requires root on a supported Linux host")
	}
	if options.WaitTimeout <= 0 {
		options.WaitTimeout = 5 * time.Minute
	}
	p, err := BuildPlan(ctx, options.Address)
	if err != nil {
		return InstallResult{}, err
	}
	if !p.Report.Installable {
		return InstallResult{}, blockersError("installation", p.Report)
	}
	unlock, err := installationLock()
	if err != nil {
		return InstallResult{}, err
	}
	defer unlock()
	executable, err := os.Executable()
	if err != nil {
		return InstallResult{}, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return InstallResult{}, err
	}
	if err := rootOwned(executable, false); err != nil {
		return InstallResult{}, err
	}
	binaryHash, err := hashFile(executable)
	if err != nil {
		return InstallResult{}, err
	}
	// Inspect the private credential before package downloads or enrollment.
	// A completed installation may omit it, or replay its original expired file.
	var joinBytes []byte
	joinHash := ""
	if options.JoinFile != "" {
		validationTime := time.Now()
		if _, err := os.Lstat(ManifestPath); err == nil {
			validationTime = time.Unix(1, 0)
		}
		artifact, err := agentctl.ReadJoinArtifact(options.JoinFile, options.JoinInput, validationTime)
		if err != nil {
			return InstallResult{}, errors.New("join artifact is invalid, expired, unreadable, or not private")
		}
		joinBytes, err = agentctl.EncodeJoinArtifact(artifact)
		if err != nil {
			return InstallResult{}, errors.New("invalid join artifact")
		}
		sum := sha256.Sum256(joinBytes)
		joinHash = hex.EncodeToString(sum[:])
	} else if _, err := os.Lstat(ManifestPath); errors.Is(err, os.ErrNotExist) {
		return InstallResult{}, errors.New("--join-file is required for a new installation")
	}
	bundle, _ := json.Marshal(p.Artifacts)
	sum := sha256.Sum256(bundle)
	i := installer{plan: p, run: runCommand}
	if err := i.loadOrCreate(binaryHash, hex.EncodeToString(sum[:]), joinHash); err != nil {
		return InstallResult{}, err
	}
	result := func() InstallResult {
		return InstallResult{NodeID: i.state.NodeID, Changed: i.changed, DataDir: AgentDir}
	}
	phase := func(name string, fn func() error) error {
		if i.state.Done[name] {
			return nil
		}
		if options.Progress != nil {
			fmt.Fprintln(options.Progress, "Installing:", name)
		}
		if err := fn(); err != nil {
			return fmt.Errorf("%s: %w; installation state retained, rerun the same command to resume", name, err)
		}
		i.state.Done[name] = true
		i.changed = true
		return i.save()
	}
	if err := phase("packages", func() error { return i.installPackages(ctx) }); err != nil {
		return result(), err
	}
	if err := phase("artifacts", func() error { return i.installArtifacts(ctx, executable) }); err != nil {
		return result(), err
	}
	if err := phase("configuration", func() error { return i.configure(ctx) }); err != nil {
		return result(), err
	}
	if err := phase("enrollment", func() error {
		if _, err := os.Lstat(filepath.Join(AgentDir, agentctl.ConfigFilename)); err == nil {
			creds, err := agentctl.Load(AgentDir, time.Now())
			if err != nil {
				return err
			}
			i.state.NodeID = creds.Config.NodeID
			return nil
		}
		if options.JoinFile == "" {
			return errors.New("--join-file is required until enrollment completes")
		}
		joined, err := agentctl.EnrollWithJoin(ctx, agentctl.JoinEnrollOptions{DataDir: AgentDir, JoinFile: "-", JoinInput: bytes.NewReader(joinBytes)})
		if err != nil {
			return err
		}
		i.state.NodeID = joined.Config.NodeID
		return nil
	}); err != nil {
		return result(), err
	}
	if err := i.validateFiles(); err != nil {
		return result(), err
	}
	credentials, err := agentctl.Load(AgentDir, time.Now())
	if err != nil {
		return result(), fmt.Errorf("validate enrolled identity: %w", err)
	}
	if credentials.Config.NodeID != i.state.NodeID {
		return result(), errors.New("enrolled identity differs from installation journal")
	}
	if err := phase("services", func() error {
		if _, err := i.run(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
		_, err := i.run(ctx, "systemctl", "enable", "--now", "orch-containerd.service", "orch-agent.service")
		return err
	}); err != nil {
		return result(), err
	}
	// An identical invocation does not restart healthy services. Recover a stopped
	// owned service using start (never restart), including after interrupted boot.
	for _, unit := range []string{"orch-containerd.service", "orch-agent.service"} {
		out, _ := timeoutCommand(i.run, ctx, "systemctl", "is-active", unit)
		if strings.TrimSpace(out) != "active" {
			if _, err := i.run(ctx, "systemctl", "start", unit); err != nil {
				return result(), err
			}
			i.changed = true
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, options.WaitTimeout)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		status, err := agentctl.ReadLocalStatus(AgentDir, time.Now())
		if err == nil && status.Ready && status.Connected && status.NodeID == i.state.NodeID {
			pid, _ := timeoutCommand(i.run, waitCtx, "systemctl", "show", "orch-agent.service", "--property=MainPID", "--value")
			if strings.TrimSpace(pid) == strconv.Itoa(status.PID) {
				r := result()
				r.Ready = true
				r.Ingress = &status.Ingress
				r.Phase = "ready"
				return r, nil
			}
		}
		select {
		case <-waitCtx.Done():
			r := result()
			r.Phase = "waiting"
			return r, errors.New("installed, but agent readiness timed out; run node doctor and journalctl -u orch-agent; rerun install to resume waiting")
		case <-ticker.C:
		}
	}
}
func (i *installer) loadOrCreate(binaryHash, bundleHash, joinHash string) error {
	for _, p := range []string{Prefix, StateDir, AgentDir, "/var/lib/orch-ingress", "/var/lib/orch-containerd", "/etc/systemd/system", "/etc/systemd/network", "/etc/sysctl.d", "/etc/modules-load.d"} {
		if err := noSymlinks(p); err != nil {
			return err
		}
	}
	data, err := os.ReadFile(ManifestPath)
	if err == nil {
		if err := rootOwned(StateDir, true); err != nil {
			return err
		}
		for _, dir := range []string{Prefix, Prefix + "/bin", Prefix + "/cni", AgentDir, "/var/lib/orch-containerd"} {
			if _, err := os.Lstat(dir); err == nil {
				if err := rootOwned(dir, true); err != nil {
					return err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}

		if err := rootOwned(ManifestPath, false); err != nil {
			return err
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&i.state); err != nil {
			return errors.New("invalid installation manifest")
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return errors.New("manifest contains trailing data")
		}
		if i.state.JoinHash == "" || (joinHash != "" && i.state.JoinHash != joinHash) || i.state.Version != 1 || i.state.Address != i.plan.Address || i.state.Distro != i.plan.Report.OS || i.state.OSVersion != i.plan.Report.Version || i.state.BinaryHash != binaryHash || i.state.BundleHash != bundleHash || i.state.Files == nil || i.state.Done == nil {
			return errors.New("existing installation intent differs; upgrades and reconfiguration are unsupported")
		}
		return i.validateFiles()
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Recheck after acquiring the process lock; a parallel installer may have won.
	for _, p := range i.plan.Files {
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("refusing unowned path: %s", p)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, p := range []string{Prefix, StateDir} {
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("refusing unowned directory: %s", p)
		}
	}
	i.state = manifest{Version: 1, JoinHash: joinHash, Address: i.plan.Address, Distro: i.plan.Report.OS, OSVersion: i.plan.Report.Version, BinaryHash: binaryHash, BundleHash: bundleHash, Build: buildinfo.Current(), Files: map[string]string{}, Done: map[string]bool{}}
	parent := filepath.Dir(StateDir)
	stage, err := os.MkdirTemp(parent, ".orch-install-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	data, err = json.MarshalIndent(i.state, "", "  ")
	if err != nil {
		return err
	}
	if err := writeExclusive(filepath.Join(stage, "manifest.json"), data, 0600); err != nil {
		return err
	}
	if err := syncDir(stage); err != nil {
		return err
	}
	if err := renameExclusive(stage, StateDir); err != nil {
		return err
	}
	i.changed = true
	return syncDir(parent)
}
func (i *installer) save() error {
	data, err := json.MarshalIndent(i.state, "", "  ")
	if err != nil {
		return err
	}
	path := i.journalPath
	if path == "" {
		path = ManifestPath
	}
	return atomicOwned(path, data, 0600)
}
func (i *installer) validateFiles() error {
	allowed := map[string]bool{}
	for _, p := range i.plan.Files {
		allowed[p] = true
	}
	for path, digest := range i.state.Files {
		if !allowed[path] {
			return errors.New("manifest contains an unexpected owned file")
		}
		if err := noSymlinks(path); err != nil {
			return err
		}
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			artifact := strings.HasPrefix(path, Prefix+"/bin/") || strings.HasPrefix(path, Prefix+"/cni/")
			if i.state.Done["services"] || artifact && i.state.Done["artifacts"] || !artifact && i.state.Done["configuration"] {
				return fmt.Errorf("owned file missing: %s", path)
			}
			continue
		}
		if err := rootOwned(path, false); err != nil {
			return err
		}
		if err := verifyHash(path, digest); err != nil {
			return err
		}
	}
	return nil
}
func (i *installer) publish(path, source string, mode os.FileMode) error {
	digest, err := hashFile(source)
	if err != nil {
		return err
	}
	if _, owned := i.state.Files[path]; !owned {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("refusing unowned file: %s", path)
		}
	}
	if expected, ok := i.state.Files[path]; ok && expected != digest {
		return fmt.Errorf("owned content changed: %s", path)
	}
	i.state.Files[path] = digest
	if err := i.save(); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return verifyHash(path, digest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := noSymlinks(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	// Sources and destinations may reside on different filesystems. Stage on the
	// destination filesystem and hard-link the fully synced file without clobber.
	bytes, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	stage, err := os.CreateTemp(filepath.Dir(path), ".orch-publish-*")
	if err != nil {
		return err
	}
	name := stage.Name()
	defer os.Remove(name)
	if err := stage.Chmod(mode); err != nil {
		stage.Close()
		return err
	}
	if _, err := stage.Write(bytes); err != nil {
		stage.Close()
		return err
	}
	if err := stage.Sync(); err != nil {
		stage.Close()
		return err
	}
	if err := stage.Close(); err != nil {
		return err
	}
	if err := os.Link(name, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func (i *installer) installPackages(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if i.state.Distro == "ubuntu" {
		if _, err := i.run(ctx, "apt-get", "-o", "DPkg::Lock::Timeout=60", "update"); err != nil {
			return err
		}
		args := append([]string{"-o", "DPkg::Lock::Timeout=60", "install", "-y", "--no-install-recommends"}, i.plan.Packages...)
		_, err := i.run(ctx, "apt-get", args...)
		return err
	}
	args := append([]string{"-y", "--setopt=gpgcheck=1", "--setopt=install_weak_deps=False", "install"}, i.plan.Packages...)
	_, err := i.run(ctx, "dnf", args...)
	return err
}
func (i *installer) installArtifacts(ctx context.Context, executable string) error {
	stage, err := os.MkdirTemp(StateDir, "artifacts-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for _, a := range i.plan.Artifacts {
		downloaded := filepath.Join(stage, a.Name)
		if err := download(ctx, a, downloaded); err != nil {
			return err
		}
		unpacked := filepath.Join(stage, a.Name+"-unpacked")
		if err := extract(a, downloaded, unpacked); err != nil {
			return err
		}
		targets := []string{}
		for _, target := range a.Files {
			targets = append(targets, target)
		}
		sort.Strings(targets)
		for _, target := range targets {
			if err := i.publish(Prefix+"/"+target, filepath.Join(unpacked, target), 0755); err != nil {
				return err
			}
		}
	}
	return i.publish(Prefix+"/bin/orch", executable, 0755)
}
func (i *installer) publishBytes(path string, data []byte) error {
	f, err := os.CreateTemp(StateDir, "config-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return i.publish(path, name, 0644)
}
func (i *installer) configure(ctx context.Context) error {
	for _, d := range []string{AgentDir, "/var/lib/orch-containerd", "/var/lib/orch-ingress"} {
		if err := os.MkdirAll(d, 0700); err != nil {
			return err
		}
	}
	if out, err := i.run(ctx, "getent", "passwd", "orch-ingress"); err == nil {
		fields := strings.Split(strings.TrimSpace(out), ":")
		if len(fields) != 7 || fields[2] == "0" || fields[3] == "0" || fields[5] != "/var/lib/orch-ingress" || !strings.HasSuffix(fields[6], "/nologin") {
			return errors.New("existing orch-ingress account conflicts")
		}
	} else {
		if _, err := i.run(ctx, "useradd", "--system", "--user-group", "--home-dir", "/var/lib/orch-ingress", "--shell", "/usr/sbin/nologin", "orch-ingress"); err != nil {
			return err
		}
	}
	if _, err := i.run(ctx, "chown", "orch-ingress:orch-ingress", "/var/lib/orch-ingress"); err != nil {
		return err
	}
	if _, err := i.run(ctx, "setcap", "cap_net_bind_service=+ep", Prefix+"/bin/caddy"); err != nil {
		return err
	}
	for _, name := range []string{"containerd.toml", "orch-agent.service", "orch-containerd.service"} {
		data, err := nodebundle.Files.ReadFile(name)
		if err != nil {
			return err
		}
		data = []byte(strings.ReplaceAll(string(data), "{{ADDRESS}}", i.state.Address))
		target := Prefix + "/" + name
		if strings.HasSuffix(name, ".service") {
			target = "/etc/systemd/system/" + name
		}
		if err := i.publishBytes(target, data); err != nil {
			return err
		}
	}
	if err := i.publishBytes("/etc/sysctl.d/90-orch.conf", []byte("# Managed by orch node install\nnet.ipv4.ip_forward=1\n")); err != nil {
		return err
	}
	if err := i.publishBytes("/etc/modules-load.d/orch.conf", []byte("overlay\nwireguard\n")); err != nil {
		return err
	}
	for _, module := range []string{"overlay", "wireguard"} {
		if _, err := i.run(ctx, "modprobe", module); err != nil {
			return err
		}
	}
	if _, err := i.run(ctx, "sysctl", "-p", "/etc/sysctl.d/90-orch.conf"); err != nil {
		return err
	}
	if i.state.Distro == "fedora" {
		data, err := nodebundle.Files.ReadFile("10-orch-veth.link")
		if err != nil {
			return err
		}
		if err := i.publishBytes(VethLinkPath, data); err != nil {
			return err
		}
		// Only reload configuration for future devices; never trigger/reapply the
		// host's physical links. NetworkManager's existing veth exclusion remains.
		if _, err := i.run(ctx, "udevadm", "control", "--reload"); err != nil {
			return err
		}
		if err := i.selinux(ctx); err != nil {
			return err
		}
	}
	if _, err := i.run(ctx, "systemd-analyze", "verify", "/etc/systemd/system/orch-containerd.service", "/etc/systemd/system/orch-agent.service"); err != nil {
		return err
	}
	return nil
}
func (i *installer) selinux(ctx context.Context) error {
	// Prefer the distribution-maintained container runtime policy, without
	// permissive domains or custom blanket allow rules.
	labels := selinuxLabels()
	out, err := i.run(ctx, "semanage", "fcontext", "-l", "-C")
	if err != nil {
		return err
	}
	for _, label := range labels {
		present := false
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) > 0 && fields[0] == label[0] {
				if !strings.Contains(line, ":"+label[1]+":") {
					return fmt.Errorf("conflicting SELinux label for %s", label[0])
				}
				present = true
			}
		}
		if !present {
			if _, err := i.run(ctx, "semanage", "fcontext", "-a", "-t", label[1], label[0]); err != nil {
				return err
			}
		}
	}
	_, err = i.run(ctx, "restorecon", "-RF", Prefix, "/var/lib/orch-containerd", "/var/lib/orch-ingress", AgentDir, VethLinkPath)
	return err
}
func writeExclusive(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}
func atomicOwned(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".orch-manifest-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func selinuxLabels() [][2]string {
	return [][2]string{{Prefix + "/bin/containerd", "container_runtime_exec_t"}, {Prefix + "/bin/containerd-shim-runc-v2", "container_runtime_exec_t"}, {Prefix + "/bin/runc", "container_runtime_exec_t"}, {"/var/lib/orch-containerd(/.*)?", "container_var_lib_t"}, {"/run/orch-containerd(/.*)?", "container_var_run_t"}, {Prefix + "/containerd.toml", "container_config_t"}}
}
