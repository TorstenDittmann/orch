package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	controlclient "change.codes/torsten/better-swarm/internal/client"
	clusterctl "change.codes/torsten/better-swarm/internal/cluster"
)

type ControllerInstallOptions struct {
	ControllerOptions
	WaitTimeout time.Duration
	Progress    interface{ Write([]byte) (int, error) }
}

type ControllerInstallResult struct {
	ClusterID   string `json:"cluster_id,omitempty"`
	Name        string `json:"name,omitempty"`
	Server      string `json:"server,omitempty"`
	Ready       bool   `json:"ready"`
	Changed     bool   `json:"changed"`
	Phase       string `json:"phase"`
	DataDir     string `json:"data_dir"`
	AdminPath   string `json:"admin_context,omitempty"`
	RecoveryDir string `json:"recovery_dir,omitempty"`
}

type controllerManifest struct {
	Version     int               `json:"version"`
	Name        string            `json:"name"`
	Server      string            `json:"server"`
	Listen      string            `json:"listen"`
	AdminDir    string            `json:"admin_dir"`
	RecoveryDir string            `json:"recovery_dir"`
	BinaryHash  string            `json:"binary_hash"`
	BundleHash  string            `json:"bundle_hash"`
	ClusterID   string            `json:"cluster_id,omitempty"`
	Files       map[string]string `json:"files"`
	Done        map[string]bool   `json:"done"`
}

type controllerInstaller struct {
	state   controllerManifest
	changed bool
	run     Command
}

func InstallController(ctx context.Context, options ControllerInstallOptions) (ControllerInstallResult, error) {
	if platform() != "linux" || os.Geteuid() != 0 {
		return ControllerInstallResult{}, errors.New("cluster install requires root on a supported Linux host")
	}
	normalized, err := normalizeControllerOptions(options.ControllerOptions, true)
	if err != nil {
		return ControllerInstallResult{}, err
	}
	options.ControllerOptions = normalized
	if options.WaitTimeout <= 0 {
		options.WaitTimeout = 2 * time.Minute
	}
	plan, err := BuildControllerPlan(ctx, normalized)
	if err != nil {
		return ControllerInstallResult{}, err
	}
	if !plan.Report.Installable {
		return ControllerInstallResult{}, errors.New("host has controller installation blockers; run cluster doctor or install --dry-run")
	}
	unlock, err := controllerInstallationLock()
	if err != nil {
		return ControllerInstallResult{}, err
	}
	defer unlock()
	executable, err := os.Executable()
	if err != nil {
		return ControllerInstallResult{}, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return ControllerInstallResult{}, err
	}
	if err := rootOwned(executable, false); err != nil {
		return ControllerInstallResult{}, err
	}
	binaryHash, err := hashFile(executable)
	if err != nil {
		return ControllerInstallResult{}, err
	}
	unit, err := controllerUnit(normalized.Listen)
	if err != nil {
		return ControllerInstallResult{}, err
	}
	sum := sha256.Sum256(unit)
	i := &controllerInstaller{run: runCommand}
	if err := i.loadOrCreate(normalized, binaryHash, hex.EncodeToString(sum[:])); err != nil {
		return ControllerInstallResult{}, err
	}
	result := func(phase string) ControllerInstallResult {
		return ControllerInstallResult{ClusterID: i.state.ClusterID, Name: i.state.Name, Server: i.state.Server,
			Ready: phase == "ready", Changed: i.changed, Phase: phase, DataDir: ControllerDataDir,
			AdminPath: filepath.Join(i.state.AdminDir, "context.json"), RecoveryDir: i.state.RecoveryDir}
	}
	phase := func(name string, fn func() error) error {
		if i.state.Done[name] {
			return nil
		}
		if options.Progress != nil {
			_, _ = fmt.Fprintln(options.Progress, "Installing:", name)
		}
		if err := fn(); err != nil {
			return fmt.Errorf("%s: %w; controller state retained, rerun the same command to resume", name, err)
		}
		i.state.Done[name] = true
		i.changed = true
		return i.save()
	}
	if err := phase("account", func() error { return i.ensureAccount(ctx) }); err != nil {
		return result("account"), err
	}
	if err := phase("binary", func() error { return i.publishFile(ControllerPrefix+"/bin/orch", executable, 0755) }); err != nil {
		return result("binary"), err
	}
	adminStage := i.state.AdminDir + ".installing"
	if err := phase("identity", func() error {
		if err := os.MkdirAll(filepath.Dir(adminStage), 0700); err != nil {
			return err
		}
		bootstrap, err := clusterctl.Bootstrap(ctx, clusterctl.BootstrapOptions{Name: i.state.Name, Server: i.state.Server, DataDir: ControllerDataDir, ContextPath: filepath.Join(adminStage, "context.json")})
		if err != nil {
			return err
		}
		i.state.ClusterID = bootstrap.Cluster.ID
		if err := i.chownControllerState(ctx); err != nil {
			return err
		}
		return i.save()
	}); err != nil {
		return result("identity"), err
	}
	if err := phase("configuration", func() error {
		if err := i.publishBytes(ControllerUnitPath, unit, 0644); err != nil {
			return err
		}
		if _, err := i.run(ctx, "systemd-analyze", "verify", ControllerUnitPath); err != nil {
			return err
		}
		distro, _ := Distro(mustReadOSRelease())
		if distro == "fedora" {
			if _, err := i.run(ctx, "restorecon", "-RF", ControllerPrefix, ControllerDataDir, ControllerStateDir, ControllerUnitPath); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return result("configuration"), err
	}
	if err := phase("service", func() error {
		if _, err := i.run(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
		_, err := i.run(ctx, "systemctl", "enable", "--now", "orch-controller.service")
		return err
	}); err != nil {
		return result("service"), err
	}
	active, _ := timeoutCommand(i.run, ctx, "systemctl", "is-active", "orch-controller.service")
	if strings.TrimSpace(active) != "active" {
		if _, err := i.run(ctx, "systemctl", "start", "orch-controller.service"); err != nil {
			return result("waiting"), err
		}
		i.changed = true
	}
	waitCtx, cancel := context.WithTimeout(ctx, options.WaitTimeout)
	defer cancel()
	contextPath := filepath.Join(adminStage, "context.json")
	if pathExistsLocal(i.state.AdminDir) {
		contextPath = filepath.Join(i.state.AdminDir, "context.json")
	}
	if err := waitForController(waitCtx, contextPath, i.state); err != nil {
		return result("waiting"), fmt.Errorf("controller readiness: %w; rerun install to resume waiting", err)
	}
	if err := phase("administrator", func() error {
		if pathExistsLocal(i.state.AdminDir) {
			return validatePrivateTree(i.state.AdminDir)
		}
		if err := renameExclusive(adminStage, i.state.AdminDir); err != nil {
			return err
		}
		return syncDir(filepath.Dir(i.state.AdminDir))
	}); err != nil {
		return result("administrator"), err
	}
	if err := phase("recovery", func() error { return i.createRecoverySet(ctx) }); err != nil {
		return result("recovery"), err
	}
	return result("ready"), nil
}

func (i *controllerInstaller) loadOrCreate(options ControllerOptions, binaryHash, bundleHash string) error {
	if data, err := os.ReadFile(ControllerManifestPath); err == nil {
		if err := rootOwned(ControllerStateDir, true); err != nil {
			return err
		}
		if err := rootOwned(ControllerManifestPath, false); err != nil {
			return err
		}
		if err := json.Unmarshal(data, &i.state); err != nil {
			return errors.New("controller installation manifest is invalid")
		}
		if i.state.Version != 1 || i.state.Name != options.Name || i.state.Server != options.Server || i.state.Listen != options.Listen || i.state.AdminDir != options.AdminDir || i.state.RecoveryDir != options.RecoveryDir || i.state.BinaryHash != binaryHash || i.state.BundleHash != bundleHash {
			return errors.New("existing controller installation intent differs; upgrades and reconfiguration are unsupported")
		}
		return i.validateFiles()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, path := range []string{ControllerPrefix, ControllerDataDir, ControllerStateDir, ControllerUnitPath, options.AdminDir, options.RecoveryDir, options.AdminDir + ".installing", options.RecoveryDir + ".installing"} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("refusing unowned controller path: %s", path)
		}
	}
	for _, path := range []string{options.AdminDir, options.RecoveryDir} {
		if err := validateControllerOutputPath(path); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(ControllerStateDir, 0700); err != nil {
		return err
	}
	if err := rootOwned(ControllerStateDir, true); err != nil {
		return err
	}
	i.state = controllerManifest{Version: 1, Name: options.Name, Server: options.Server, Listen: options.Listen, AdminDir: options.AdminDir, RecoveryDir: options.RecoveryDir, BinaryHash: binaryHash, BundleHash: bundleHash, Files: map[string]string{}, Done: map[string]bool{}}
	i.changed = true
	return i.save()
}

func (i *controllerInstaller) save() error {
	data, err := json.MarshalIndent(i.state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicOwned(ControllerManifestPath, data, 0600)
}

func (i *controllerInstaller) ensureAccount(ctx context.Context) error {
	if _, _, err := controllerUID(ctx); err == nil {
		return nil
	}
	if _, err := i.run(ctx, "getent", "passwd", "orch-controller"); err == nil {
		return errors.New("existing orch-controller account conflicts")
	}
	_, err := i.run(ctx, "useradd", "--system", "--user-group", "--home-dir", ControllerDataDir, "--shell", "/usr/sbin/nologin", "orch-controller")
	return err
}

func (i *controllerInstaller) chownControllerState(ctx context.Context) error {
	uid, gid, err := controllerUID(ctx)
	if err != nil {
		return err
	}
	return filepath.WalkDir(ControllerDataDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in controller state: %s", path)
		}
		return os.Chown(path, uid, gid)
	})
}

func (i *controllerInstaller) publishBytes(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(ControllerStateDir, "publish-*")
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
	return i.publishFile(path, name, mode)
}

func (i *controllerInstaller) publishFile(path, source string, mode os.FileMode) error {
	digest, err := hashFile(source)
	if err != nil {
		return err
	}
	if expected, ok := i.state.Files[path]; ok {
		if expected != digest {
			return fmt.Errorf("owned controller content changed: %s", path)
		}
		if _, err := os.Lstat(path); err == nil {
			if err := rootOwned(path, false); err != nil {
				return err
			}
			return verifyHash(path, digest)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("refusing unowned controller file: %s", path)
	}
	i.state.Files[path] = digest
	if err := i.save(); err != nil {
		return err
	}
	if err := noSymlinks(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(path)
		}
	}()
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
	remove = false
	return syncDir(filepath.Dir(path))
}

func (i *controllerInstaller) validateFiles() error {
	for path, digest := range i.state.Files {
		if path != ControllerPrefix+"/bin/orch" && path != ControllerUnitPath {
			return fmt.Errorf("unexpected owned controller file: %s", path)
		}
		phase := "binary"
		if path == ControllerUnitPath {
			phase = "configuration"
		}
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) && !i.state.Done[phase] {
			continue
		} else if err != nil {
			return err
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

func waitForController(ctx context.Context, contextPath string, expected controllerManifest) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var last error
	for {
		if err := exactControllerProcess(ctx, expected.Listen); err != nil {
			last = err
			select {
			case <-ctx.Done():
				return fmt.Errorf("%w: %v", ctx.Err(), last)
			case <-ticker.C:
				continue
			}
		}
		client, err := controlclient.New(contextPath)
		if err == nil {
			requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			cluster, requestErr := client.GetCluster(requestCtx)
			cancel()
			client.CloseIdleConnections()
			if requestErr == nil && cluster.GetId() == expected.ClusterID && cluster.GetName() == expected.Name && cluster.GetEndpoint() == expected.Server {
				return nil
			}
			if requestErr != nil {
				last = requestErr
			} else {
				last = errors.New("controller returned a different cluster identity")
			}
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			if last != nil {
				return fmt.Errorf("%w: %v", ctx.Err(), last)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func exactControllerProcess(ctx context.Context, listen string) error {
	out, err := timeoutCommand(runCommand, ctx, "systemctl", "show", "orch-controller.service", "--property=MainPID", "--value")
	if err != nil {
		return err
	}
	pid := strings.TrimSpace(out)
	if pid == "" || pid == "0" {
		return errors.New("controller service has no main process")
	}
	executable, err := os.Readlink("/proc/" + pid + "/exe")
	if err != nil || executable != ControllerPrefix+"/bin/orch" {
		return errors.New("controller service is not executing the owned binary")
	}
	_, port, _ := net.SplitHostPort(listen)
	listeners, err := timeoutCommand(runCommand, ctx, "ss", "-H", "-lntp")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(listeners, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		_, candidate, splitErr := net.SplitHostPort(fields[3])
		if splitErr == nil && candidate == port && strings.Contains(line, "pid="+pid+",") && ownedControllerListener(line) {
			return nil
		}
	}
	return errors.New("controller process does not own the configured listener")
}

type recoverySetManifest struct {
	Version   int               `json:"version"`
	ClusterID string            `json:"cluster_id"`
	Name      string            `json:"name"`
	Server    string            `json:"server"`
	Schema    int               `json:"schema_version"`
	CreatedAt string            `json:"created_at"`
	Files     map[string]string `json:"files"`
}

func (i *controllerInstaller) createRecoverySet(ctx context.Context) error {
	if pathExistsLocal(i.state.RecoveryDir) {
		return i.validateRecoverySet(ctx)
	}
	stage := i.state.RecoveryDir + ".installing"
	if pathExistsLocal(stage) {
		if err := os.RemoveAll(stage); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Join(stage, "pki"), 0700); err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	backup := filepath.Join(stage, "control.db")
	key := filepath.Join(stage, clusterctl.SecretKeyFilename)
	backed, err := clusterctl.Backup(ctx, clusterctl.BackupOptions{DataDir: ControllerDataDir, Output: backup, SecretKeyOutput: key})
	if err != nil {
		return err
	}
	for _, name := range []string{"ca.pem", "ca-key.pem", "controller.pem", "controller-key.pem"} {
		if err := copyPrivateFile(filepath.Join(ControllerDataDir, "pki", name), filepath.Join(stage, "pki", name)); err != nil {
			return err
		}
	}
	inspected, err := clusterctl.InspectBackup(ctx, clusterctl.RecoveryOptions{Backup: backup, PKIDir: filepath.Join(stage, "pki"), SecretKey: key})
	if err != nil || !inspected.Ready || inspected.Cluster.ID != i.state.ClusterID {
		return errors.New("generated recovery set did not pass recovery inspection")
	}
	manifest := recoverySetManifest{Version: 1, ClusterID: backed.Cluster.ID, Name: backed.Cluster.Name, Server: backed.Cluster.Endpoint, Schema: inspected.SchemaVersion, CreatedAt: time.Now().UTC().Format(time.RFC3339), Files: map[string]string{}}
	for _, relative := range []string{"control.db", clusterctl.SecretKeyFilename, "pki/ca.pem", "pki/ca-key.pem", "pki/controller.pem", "pki/controller-key.pem"} {
		digest, err := hashFile(filepath.Join(stage, relative))
		if err != nil {
			return err
		}
		manifest.Files[relative] = digest
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), append(data, '\n'), 0600); err != nil {
		return err
	}
	if err := syncTree(stage); err != nil {
		return err
	}
	if err := renameExclusive(stage, i.state.RecoveryDir); err != nil {
		return err
	}
	return syncDir(filepath.Dir(i.state.RecoveryDir))
}

func (i *controllerInstaller) validateRecoverySet(ctx context.Context) error {
	if err := validatePrivateTree(i.state.RecoveryDir); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(i.state.RecoveryDir, "manifest.json"))
	if err != nil {
		return err
	}
	var manifest recoverySetManifest
	if json.Unmarshal(data, &manifest) != nil || manifest.Version != 1 || manifest.ClusterID != i.state.ClusterID || manifest.Name != i.state.Name || manifest.Server != i.state.Server || manifest.Schema <= 0 {
		return errors.New("recovery set manifest is invalid")
	}
	expected := map[string]bool{
		"control.db": true, clusterctl.SecretKeyFilename: true, "pki/ca.pem": true,
		"pki/ca-key.pem": true, "pki/controller.pem": true, "pki/controller-key.pem": true,
	}
	if len(manifest.Files) != len(expected) {
		return errors.New("recovery set manifest has unexpected files")
	}
	for relative, digest := range manifest.Files {
		if !expected[relative] {
			return errors.New("recovery set manifest has unexpected files")
		}
		if err := verifyHash(filepath.Join(i.state.RecoveryDir, relative), digest); err != nil {
			return err
		}
	}
	inspected, err := clusterctl.InspectBackup(ctx, clusterctl.RecoveryOptions{Backup: filepath.Join(i.state.RecoveryDir, "control.db"), PKIDir: filepath.Join(i.state.RecoveryDir, "pki"), SecretKey: filepath.Join(i.state.RecoveryDir, clusterctl.SecretKeyFilename)})
	if err != nil || !inspected.Ready || inspected.Cluster.ID != i.state.ClusterID {
		return errors.New("recovery set is not restorable")
	}
	return nil
}

func copyPrivateFile(source, destination string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
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
	return f.Close()
}

func validatePrivateTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() && info.Mode().Perm() != 0700 || !info.IsDir() && (!info.Mode().IsRegular() || info.Mode().Perm() != 0600) {
			return fmt.Errorf("private output has unsafe entry: %s", path)
		}
		if os.Geteuid() == 0 {
			if err := rootOwned(path, info.IsDir()); err != nil {
				return err
			}
		}
		return nil
	})
}

func syncTree(root string) error {
	var directories []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		err = f.Sync()
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if err := syncDir(directories[index]); err != nil {
			return err
		}
	}
	return nil
}

func mustReadOSRelease() string { data, _ := os.ReadFile("/etc/os-release"); return string(data) }
