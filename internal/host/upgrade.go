package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	agentctl "change.codes/torsten/better-swarm/internal/agent"
	agentstate "change.codes/torsten/better-swarm/internal/agent/state"
	"change.codes/torsten/better-swarm/internal/buildinfo"
	"change.codes/torsten/better-swarm/internal/store"
)

type UpgradeOptions struct {
	Binary      string
	DryRun      bool
	WaitTimeout time.Duration
	Progress    io.Writer
}

type UpgradeResult struct {
	Kind              string               `json:"kind"`
	Candidate         buildinfo.Descriptor `json:"candidate"`
	CandidateHash     string               `json:"candidate_hash"`
	Compatible        bool                 `json:"compatible"`
	Changed           bool                 `json:"changed"`
	Ready             bool                 `json:"ready"`
	Phase             string               `json:"phase"`
	RecoveryPath      string               `json:"recovery_path,omitempty"`
	AutomaticRollback bool                 `json:"automatic_rollback"`
	Actions           []string             `json:"actions"`
}

type upgradeJournal struct {
	Version       int    `json:"version"`
	Kind          string `json:"kind"`
	CandidateHash string `json:"candidate_hash"`
	PreviousHash  string `json:"previous_hash"`
	PreviousPath  string `json:"previous_path"`
	RecoveryPath  string `json:"recovery_path,omitempty"`
	Phase         string `json:"phase"`
}

func inspectCandidate(ctx context.Context, path string) (buildinfo.Descriptor, string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return buildinfo.Descriptor{}, "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return buildinfo.Descriptor{}, "", fmt.Errorf("candidate binary: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return buildinfo.Descriptor{}, "", errors.New("candidate must be a regular file that is not group/world writable")
	}
	if os.Geteuid() == 0 {
		if err := rootOwned(resolved, false); err != nil {
			return buildinfo.Descriptor{}, "", fmt.Errorf("candidate binary: %w", err)
		}
	}
	hash, err := hashFile(resolved)
	if err != nil {
		return buildinfo.Descriptor{}, "", err
	}
	commandCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(commandCtx, resolved, "version", "--json").Output()
	if err != nil {
		return buildinfo.Descriptor{}, "", errors.New("candidate does not expose a valid compatibility descriptor")
	}
	var descriptor buildinfo.Descriptor
	decoder := json.NewDecoder(strings.NewReader(string(out)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&descriptor) != nil || descriptor.Version == "" || descriptor.BuildID == "" {
		return buildinfo.Descriptor{}, "", errors.New("candidate compatibility descriptor is invalid")
	}
	if descriptor.OS != "linux" || descriptor.Architecture != "amd64" || descriptor.InstallFormat != 1 || descriptor.ReleaseGeneration < 1 {
		return buildinfo.Descriptor{}, "", errors.New("candidate OS, architecture, install format, or release generation is incompatible")
	}
	return descriptor, hash, nil
}

func UpgradeController(ctx context.Context, options UpgradeOptions) (UpgradeResult, error) {
	if os.Geteuid() != 0 || platform() != "linux" {
		return UpgradeResult{Kind: "controller", Phase: "preflight"}, errors.New("cluster upgrade requires root on Linux")
	}
	result, state, candidate, err := controllerUpgradePreflight(ctx, options.Binary)
	if err != nil {
		return result, err
	}
	if result.CandidateHash == state.BinaryHash {
		result.Ready, result.Phase = true, "ready"
		return result, nil
	}
	if options.DryRun {
		return result, nil
	}
	if options.WaitTimeout <= 0 {
		options.WaitTimeout = 2 * time.Minute
	}
	unlock, err := controllerInstallationLock()
	if err != nil {
		return result, err
	}
	defer unlock()
	journalPath := filepath.Join(ControllerStateDir, "upgrade.json")
	journal, err := loadUpgradeJournal(journalPath, "controller", result.CandidateHash, state.BinaryHash, ControllerPrefix+"/bin/orch")
	if err != nil {
		return result, err
	}
	result.RecoveryPath = journal.RecoveryPath
	if journal.Phase == "ready" {
		result.Ready = true
		result.Phase = "ready"
		return result, nil
	}
	if journal.Phase == "planned" {
		recovery := filepath.Join(ControllerStateDir, "upgrades", result.CandidateHash, "recovery")
		installer := controllerInstaller{state: state, run: runCommand}
		installer.state.RecoveryDir = recovery
		err := prepareControllerRecovery(ctx, runCommand, recovery, func(ctx context.Context) error {
			if err := installer.createRecoverySet(ctx); err != nil {
				return fmt.Errorf("create pre-upgrade recovery set: %w", err)
			}
			rehearsal := filepath.Join(ControllerStateDir, "upgrades", result.CandidateHash, "migration-rehearsal")
			if err := os.RemoveAll(rehearsal); err != nil {
				return err
			}
			if err := os.MkdirAll(rehearsal, 0700); err != nil {
				return err
			}
			rehearsalDB := filepath.Join(rehearsal, "control.db")
			if err := copyRegular(filepath.Join(recovery, "control.db"), rehearsalDB, 0600); err != nil {
				return fmt.Errorf("stage controller migration rehearsal: %w", err)
			}
			database, _, err := store.OpenRecoveryCopy(ctx, rehearsalDB)
			if err != nil {
				return fmt.Errorf("candidate controller migration rehearsal: %w", err)
			}
			if err = database.Close(); err != nil {
				return err
			}
			journal.RecoveryPath, journal.Phase = recovery, "backed_up"
			return saveUpgradeJournal(journalPath, journal)
		})
		if err != nil {
			return result, err
		}
	}
	result.RecoveryPath = journal.RecoveryPath
	if journal.Phase == "backed_up" {
		if _, err := runCommand(ctx, "systemctl", "stop", "orch-controller.service"); err != nil {
			return result, err
		}
		journal.Phase = "replacing"
		if err := saveUpgradeJournal(journalPath, journal); err != nil {
			return result, err
		}
	}
	if journal.Phase == "replacing" {
		if _, err := runCommand(ctx, "systemctl", "stop", "orch-controller.service"); err != nil {
			return result, err
		}
		if err := applyCandidate(journal.PreviousPath, candidate, journal.PreviousHash, journal.CandidateHash); err != nil {
			return result, err
		}
		journal.Phase = "replaced"
		if err := saveUpgradeJournal(journalPath, journal); err != nil {
			return result, err
		}
	}
	if _, err := runCommand(ctx, "systemctl", "start", "orch-controller.service"); err != nil {
		return result, rollbackController(ctx, runCommand, journalPath, ControllerDataDir, journal, err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, options.WaitTimeout)
	defer cancel()
	if err := waitForController(waitCtx, filepath.Join(state.AdminDir, "context.json"), state); err != nil {
		return result, rollbackController(ctx, runCommand, journalPath, ControllerDataDir, journal, err)
	}
	state.BinaryHash, state.Build = result.CandidateHash, result.Candidate
	state.Files[ControllerPrefix+"/bin/orch"] = result.CandidateHash
	data, _ := json.MarshalIndent(state, "", "  ")
	if err := atomicOwned(ControllerManifestPath, append(data, '\n'), 0600); err != nil {
		return result, err
	}
	if err := finishUpgradeJournal(journalPath); err != nil {
		return result, err
	}
	result.Changed, result.Ready, result.Phase, result.AutomaticRollback = true, true, "ready", false
	return result, nil
}

// prepareControllerRecovery stops the controller so the recovery set is a
// consistent snapshot, and discards any set left by an earlier attempt: until
// the journal records backed_up the live database is authoritative, so a stale
// set must never be reused. The old controller is restarted if preparation fails.
func prepareControllerRecovery(ctx context.Context, run Command, recovery string, prepare func(context.Context) error) error {
	if err := stopUnitConfirmed(ctx, run, "orch-controller.service"); err != nil {
		return err
	}
	err := os.RemoveAll(recovery)
	if err == nil {
		err = prepare(ctx)
	}
	if err != nil {
		if _, startErr := run(ctx, "systemctl", "start", "orch-controller.service"); startErr != nil {
			return fmt.Errorf("upgrade preparation failed: %v; restart old controller: %w", err, startErr)
		}
		return err
	}
	return nil
}

func controllerUpgradePreflight(ctx context.Context, binary string) (UpgradeResult, controllerManifest, string, error) {
	r := UpgradeResult{Kind: "controller", Phase: "preflight", AutomaticRollback: true, Actions: []string{"Stop only orch-controller", "Create a fresh private recovery set", "Atomically replace the managed orch binary", "Start and authenticate the original cluster identity"}}
	candidate, hash, err := inspectCandidate(ctx, binary)
	r.Candidate, r.CandidateHash = candidate, hash
	if err != nil {
		return r, controllerManifest{}, "", err
	}
	data, err := os.ReadFile(ControllerManifestPath)
	if err != nil {
		return r, controllerManifest{}, "", errors.New("managed controller installation was not found")
	}
	var state controllerManifest
	if json.Unmarshal(data, &state) != nil || state.Version != 1 || state.ClusterID == "" {
		return r, state, "", errors.New("controller installation manifest is invalid")
	}
	osData, _ := os.ReadFile("/etc/os-release")
	distro, version := Distro(string(osData))
	if !supported(distro, version, "amd64") {
		return r, state, "", errors.New("controller distribution is unsupported")
	}
	if err := rootOwned(ControllerStateDir, true); err != nil {
		return r, state, "", err
	}
	if err := rootOwned(ControllerManifestPath, false); err != nil {
		return r, state, "", err
	}
	if err := rootOwned(ControllerPrefix+"/bin/orch", false); err != nil {
		return r, state, "", err
	}
	if freeSpace(ControllerStateDir) < 512<<20 {
		return r, state, "", errors.New("at least 512 MiB free is required for the controller recovery set")
	}
	if err := verifyInstalledBinary(ControllerPrefix+"/bin/orch", state.BinaryHash, filepath.Join(ControllerStateDir, "upgrade.json"), "controller", hash); err != nil {
		return r, state, "", errors.New("installed controller binary differs from its manifest")
	}
	installed := installedDescriptor(state.Build)
	if err := compatibleTransition(installed, candidate, true); err != nil {
		return r, state, "", err
	}
	r.Compatible = true
	resolved, _ := filepath.EvalSymlinks(binary)
	return r, state, resolved, nil
}

func UpgradeNode(ctx context.Context, options UpgradeOptions) (UpgradeResult, error) {
	if os.Geteuid() != 0 || platform() != "linux" {
		return UpgradeResult{Kind: "node", Phase: "preflight"}, errors.New("node upgrade requires root on Linux")
	}
	result, state, candidate, err := nodeUpgradePreflight(ctx, options.Binary)
	if err != nil {
		return result, err
	}
	if result.CandidateHash == state.BinaryHash {
		result.Ready, result.Phase = true, "ready"
		return result, nil
	}
	if options.DryRun {
		return result, nil
	}
	if options.WaitTimeout <= 0 {
		options.WaitTimeout = 5 * time.Minute
	}
	unlock, err := installationLock()
	if err != nil {
		return result, err
	}
	defer unlock()
	journalPath := filepath.Join(StateDir, "upgrade.json")
	journal, err := loadUpgradeJournal(journalPath, "node", result.CandidateHash, state.BinaryHash, Prefix+"/bin/orch")
	if err != nil {
		return result, err
	}
	if journal.Phase == "ready" {
		result.Ready = true
		result.Phase = "ready"
		return result, nil
	}
	backup := filepath.Join(StateDir, "upgrades", result.CandidateHash, "agent-state")
	if journal.Phase == "planned" {
		if _, err = runCommand(ctx, "systemctl", "stop", "orch-agent.service"); err != nil {
			return result, err
		}
		failPreparation := func(preparationErr error) (UpgradeResult, error) {
			_, startErr := runCommand(ctx, "systemctl", "start", "orch-agent.service")
			if startErr != nil {
				return result, fmt.Errorf("upgrade preparation failed: %v; restart old agent: %w", preparationErr, startErr)
			}
			return result, preparationErr
		}
		if err = os.RemoveAll(backup); err != nil {
			return failPreparation(err)
		}
		if err = copyTreePrivate(AgentDir, backup); err != nil {
			return failPreparation(err)
		}
		rehearsal := filepath.Join(StateDir, "upgrades", result.CandidateHash, "agent-rehearsal")
		if err = os.RemoveAll(rehearsal); err != nil {
			return failPreparation(err)
		}
		if err = copyTreePrivate(backup, rehearsal); err != nil {
			return failPreparation(fmt.Errorf("stage agent migration rehearsal: %w", err))
		}
		credentials, loadErr := agentctl.Load(rehearsal, time.Now())
		if loadErr != nil {
			return failPreparation(fmt.Errorf("load agent rehearsal identity: %w", loadErr))
		}
		cache, openErr := agentstate.Open(ctx, filepath.Join(rehearsal, "agent.db"), credentials.Config.ClusterID, credentials.Config.NodeID)
		if openErr != nil {
			return failPreparation(fmt.Errorf("candidate agent migration rehearsal: %w", openErr))
		}
		if closeErr := cache.Close(); closeErr != nil {
			return failPreparation(closeErr)
		}
		journal.RecoveryPath, journal.Phase = backup, "backed_up"
		if err = saveUpgradeJournal(journalPath, journal); err != nil {
			return failPreparation(err)
		}
	}
	result.RecoveryPath = journal.RecoveryPath
	if journal.Phase == "backed_up" {
		if _, err = runCommand(ctx, "systemctl", "stop", "orch-agent.service"); err != nil {
			return result, err
		}
		journal.Phase = "replacing"
		if err = saveUpgradeJournal(journalPath, journal); err != nil {
			return result, err
		}
	}
	if journal.Phase == "replacing" {
		if _, err = runCommand(ctx, "systemctl", "stop", "orch-agent.service"); err != nil {
			return result, err
		}
		if err = applyCandidate(journal.PreviousPath, candidate, journal.PreviousHash, journal.CandidateHash); err != nil {
			return result, err
		}
		journal.Phase = "replaced"
		if err = saveUpgradeJournal(journalPath, journal); err != nil {
			return result, err
		}
	}
	if _, err = runCommand(ctx, "systemctl", "start", "orch-agent.service"); err != nil {
		return result, rollbackNode(ctx, runCommand, journalPath, AgentDir, journal, err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, options.WaitTimeout)
	defer cancel()
	if err = waitForAgent(waitCtx, state.NodeID); err != nil {
		return result, rollbackNode(ctx, runCommand, journalPath, AgentDir, journal, err)
	}
	state.BinaryHash, state.Build = result.CandidateHash, result.Candidate
	state.Files[Prefix+"/bin/orch"] = result.CandidateHash
	data, _ := json.MarshalIndent(state, "", "  ")
	if err = atomicOwned(ManifestPath, data, 0600); err != nil {
		return result, err
	}
	if err = finishUpgradeJournal(journalPath); err != nil {
		return result, err
	}
	result.Changed, result.Ready, result.Phase, result.AutomaticRollback = true, true, "ready", false
	return result, nil
}

func nodeUpgradePreflight(ctx context.Context, binary string) (UpgradeResult, manifest, string, error) {
	r := UpgradeResult{Kind: "node", Phase: "preflight", AutomaticRollback: true, Actions: []string{"Stop only orch-agent; keep containerd and workloads running", "Back up private agent state", "Atomically replace the managed orch binary", "Reconnect with the same node identity and adopt workloads"}}
	candidate, hash, err := inspectCandidate(ctx, binary)
	r.Candidate, r.CandidateHash = candidate, hash
	if err != nil {
		return r, manifest{}, "", err
	}
	data, err := os.ReadFile(ManifestPath)
	if err != nil {
		return r, manifest{}, "", errors.New("managed node installation was not found")
	}
	var state manifest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&state) != nil || state.Version != 1 || state.NodeID == "" {
		return r, state, "", errors.New("node installation manifest is invalid")
	}
	if !supported(state.Distro, state.OSVersion, "amd64") {
		return r, state, "", errors.New("installed node distribution is unsupported")
	}
	if err := rootOwned(StateDir, true); err != nil {
		return r, state, "", err
	}
	if err := rootOwned(ManifestPath, false); err != nil {
		return r, state, "", err
	}
	if err := rootOwned(Prefix+"/bin/orch", false); err != nil {
		return r, state, "", err
	}
	if freeSpace(StateDir) < 256<<20 {
		return r, state, "", errors.New("at least 256 MiB free is required for the agent state backup")
	}
	if err = verifyInstalledBinary(Prefix+"/bin/orch", state.BinaryHash, filepath.Join(StateDir, "upgrade.json"), "node", hash); err != nil {
		return r, state, "", errors.New("installed node binary differs from its manifest")
	}
	installed := installedDescriptor(state.Build)
	if err := compatibleTransition(installed, candidate, false); err != nil {
		return r, state, "", err
	}
	r.Compatible = true
	resolved, _ := filepath.EvalSymlinks(binary)
	return r, state, resolved, nil
}

func installedDescriptor(recorded buildinfo.Descriptor) buildinfo.Descriptor {
	if recorded.InstallFormat != 0 {
		return recorded
	}
	return buildinfo.Descriptor{Version: "managed-v1", BuildID: "change-32-baseline", OS: "linux", Architecture: "amd64", InstallFormat: 1, ReleaseGeneration: 1, ControllerSchema: 11, AgentSchema: 4, AgentProtocol: "v1alpha1-workloads-5", DependencyGeneration: "managed-bundle-1"}
}

func compatibleTransition(installed, candidate buildinfo.Descriptor, controller bool) error {
	if candidate.InstallFormat != installed.InstallFormat || candidate.AgentProtocol != installed.AgentProtocol || candidate.DependencyGeneration != installed.DependencyGeneration {
		return errors.New("candidate install format, protocol, or dependency generation is incompatible")
	}
	if candidate.ReleaseGeneration < installed.ReleaseGeneration {
		return errors.New("candidate is an unsupported downgrade")
	}
	if controller && candidate.ControllerSchema < installed.ControllerSchema {
		return errors.New("candidate controller schema is older than installed state")
	}
	if !controller && candidate.AgentSchema < installed.AgentSchema {
		return errors.New("candidate agent schema is older than installed state")
	}
	return nil
}

// verifyInstalledBinary accepts the manifest's binary, or the pinned candidate
// when an interrupted upgrade replaced the binary before the manifest recorded it.
func verifyInstalledBinary(installed, recorded, journalPath, kind, candidate string) error {
	err := verifyHash(installed, recorded)
	if err == nil {
		return nil
	}
	var j upgradeJournal
	data, readErr := os.ReadFile(journalPath)
	if readErr != nil || json.Unmarshal(data, &j) != nil || j.Version != 1 || j.Kind != kind || j.CandidateHash != candidate || j.PreviousHash != recorded || j.Phase == "planned" {
		return err
	}
	return verifyHash(installed, candidate)
}

func loadUpgradeJournal(path, kind, candidate, previous, installed string) (upgradeJournal, error) {
	var j upgradeJournal
	data, err := os.ReadFile(path)
	if err == nil {
		if json.Unmarshal(data, &j) != nil || j.Version != 1 || j.Kind != kind {
			return j, errors.New("another managed upgrade is incomplete; resume it with the same candidate")
		}
		if j.CandidateHash == candidate {
			return j, nil
		}
		// A journal that reached readiness, or whose candidate the manifest
		// already records, belongs to a finished upgrade and is superseded.
		if j.Phase != "ready" && j.CandidateHash != previous {
			return j, errors.New("another managed upgrade is incomplete; resume it with the same candidate")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return j, err
	}
	dir := filepath.Join(filepath.Dir(path), "upgrades", candidate)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return j, err
	}
	previousPath := filepath.Join(dir, "previous-orch")
	j = upgradeJournal{Version: 1, Kind: kind, CandidateHash: candidate, PreviousHash: previous, PreviousPath: installed, Phase: "planned"}
	if err = copyRegular(installed, previousPath, 0755); err != nil {
		return j, err
	}
	return j, saveUpgradeJournal(path, j)
}

// finishUpgradeJournal removes the journal once the manifest records the
// candidate. Recovery material under upgrades/ is retained for the operator.
func finishUpgradeJournal(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func saveUpgradeJournal(path string, j upgradeJournal) error {
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return atomicOwned(path, append(data, '\n'), 0600)
}
func applyCandidate(installed, candidate, previousHash, candidateHash string) error {
	current, err := hashFile(installed)
	if err != nil {
		return err
	}
	if current == candidateHash {
		return nil
	}
	if current != previousHash {
		return errors.New("installed binary changed during the upgrade")
	}
	return replaceRegular(candidate, installed, 0755)
}
func replaceRegular(source, destination string, mode os.FileMode) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return atomicOwned(destination, data, mode)
}
func copyRegular(source, destination string, mode os.FileMode) error {
	if _, err := os.Lstat(destination); err == nil {
		digest, hashErr := hashFile(source)
		if hashErr != nil {
			return hashErr
		}
		return verifyHash(destination, digest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return writeExclusive(destination, data, mode)
}

// rollbackTimeout bounds a rollback that must outlive operator cancellation.
const rollbackTimeout = 3 * time.Minute

// stopUnitConfirmed stops unit and confirms systemd reports it inactive, so
// recovery state is never swapped under a still-running candidate.
func stopUnitConfirmed(ctx context.Context, run Command, unit string) error {
	_, stopErr := run(ctx, "systemctl", "stop", unit)
	state, err := run(ctx, "systemctl", "show", unit, "--property=ActiveState", "--value")
	if err == nil {
		state = strings.TrimSpace(state)
		if state == "inactive" || state == "failed" {
			return nil
		}
		err = fmt.Errorf("unit is %s", state)
	}
	return fmt.Errorf("%s could not be confirmed stopped: %w", unit, errors.Join(stopErr, err))
}

func rollbackController(ctx context.Context, run Command, journalPath, dataDir string, j upgradeJournal, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	if err := stopUnitConfirmed(ctx, run, "orch-controller.service"); err != nil {
		return fmt.Errorf("candidate failed: %v; rollback was not attempted: %v; rerun the upgrade with the same candidate to resume; recovery set: %s", cause, err, j.RecoveryPath)
	}
	_ = os.Remove(filepath.Join(dataDir, "control.db-wal"))
	_ = os.Remove(filepath.Join(dataDir, "control.db-shm"))
	restoreErr := replaceRegular(filepath.Join(j.RecoveryPath, "control.db"), filepath.Join(dataDir, "control.db"), 0600)
	if uid, gid, err := controllerUID(ctx); err == nil && restoreErr == nil {
		restoreErr = os.Chown(filepath.Join(dataDir, "control.db"), uid, gid)
	}
	if err := replaceRegular(filepath.Join(filepath.Dir(journalPath), "upgrades", j.CandidateHash, "previous-orch"), j.PreviousPath, 0755); restoreErr == nil {
		restoreErr = err
	}
	// After a full restore the previous version runs and changes state again,
	// so a retry starts from planned and takes a fresh backup. If the restore
	// failed, the backup is still the only good copy and stays pinned.
	j.Phase = "planned"
	if restoreErr != nil {
		j.Phase = "backed_up"
	}
	if err := saveUpgradeJournal(journalPath, j); restoreErr == nil {
		restoreErr = err
	}
	_, startErr := run(ctx, "systemctl", "start", "orch-controller.service")
	if restoreErr != nil || startErr != nil {
		return fmt.Errorf("candidate failed: %v; restoring the previous controller also failed: %v; recovery set: %s", cause, errors.Join(restoreErr, startErr), j.RecoveryPath)
	}
	return fmt.Errorf("candidate failed and the previous controller was restored: %w", cause)
}
func rollbackNode(ctx context.Context, run Command, journalPath, agentDir string, j upgradeJournal, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	if err := stopUnitConfirmed(ctx, run, "orch-agent.service"); err != nil {
		return fmt.Errorf("candidate failed: %v; rollback was not attempted: %v; rerun the upgrade with the same candidate to resume; state backup: %s", cause, err, j.RecoveryPath)
	}
	restoreErr := os.RemoveAll(agentDir)
	if err := copyTreePrivate(j.RecoveryPath, agentDir); restoreErr == nil {
		restoreErr = err
	}
	if err := replaceRegular(filepath.Join(filepath.Dir(journalPath), "upgrades", j.CandidateHash, "previous-orch"), j.PreviousPath, 0755); restoreErr == nil {
		restoreErr = err
	}
	// As for the controller, a retry after a full restore takes a fresh backup.
	j.Phase = "planned"
	if restoreErr != nil {
		j.Phase = "backed_up"
	}
	if err := saveUpgradeJournal(journalPath, j); restoreErr == nil {
		restoreErr = err
	}
	_, startErr := run(ctx, "systemctl", "start", "orch-agent.service")
	if restoreErr != nil || startErr != nil {
		return fmt.Errorf("candidate failed: %v; restoring the previous agent also failed: %v; state backup: %s", cause, errors.Join(restoreErr, startErr), j.RecoveryPath)
	}
	return fmt.Errorf("candidate failed and the previous agent was restored: %w", cause)
}
func waitForAgent(ctx context.Context, nodeID string) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		status, err := agentctl.ReadLocalStatus(AgentDir, time.Now())
		if err == nil && status.Ready && status.Connected && status.NodeID == nodeID {
			pid, _ := timeoutCommand(runCommand, ctx, "systemctl", "show", "orch-agent.service", "--property=MainPID", "--value")
			if strings.TrimSpace(pid) == strconv.Itoa(status.PID) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("agent readiness timed out before assignment acknowledgement")
		case <-ticker.C:
		}
	}
}
func copyTreePrivate(source, destination string) error {
	if _, err := os.Lstat(destination); err == nil {
		return validatePrivateTree(destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	stage := destination + ".copying"
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	if err := copyTreeContents(source, stage); err != nil {
		_ = os.RemoveAll(stage)
		return err
	}
	if err := syncTree(stage); err != nil {
		_ = os.RemoveAll(stage)
		return err
	}
	if err := renameExclusive(stage, destination); err != nil {
		_ = os.RemoveAll(stage)
		return err
	}
	return syncDir(filepath.Dir(destination))
}

func copyTreeContents(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, _ := filepath.Rel(source, path)
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in agent state: %s", path)
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular agent state: %s", path)
		}
		return copyRegular(path, target, 0600)
	})
}
