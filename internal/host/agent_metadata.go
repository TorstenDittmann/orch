package host

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	agentctl "github.com/TorstenDittmann/orch/internal/agent"
)

// Only durable agent metadata belongs in upgrade recovery. Never walk the live
// agent tree: volumes, logs, backups, network identity and secret mounts must
// stay in place, with their ownership and permissions unchanged.
var agentMetadataFiles = []string{
	agentctl.ConfigFilename,
	agentctl.DatabaseFilename,
	agentctl.DatabaseFilename + "-wal",
	agentctl.DatabaseFilename + "-shm",
	agentctl.DatabaseFilename + "-journal",
	agentctl.SecretCacheKeyFile,
	filepath.Join(agentctl.IdentityDirectory, agentctl.CACertificateFile),
	filepath.Join(agentctl.IdentityDirectory, agentctl.AgentCertificateFile),
	filepath.Join(agentctl.IdentityDirectory, agentctl.AgentPrivateKeyFile),
}

func checkMetadataDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("unsafe agent metadata directory: %s", path)
	}
	return nil
}

func snapshotAgentMetadata(source, destination string) error {
	if err := checkMetadataDirectory(source); err != nil {
		return err
	}
	if err := checkMetadataDirectory(filepath.Join(source, agentctl.IdentityDirectory)); err != nil {
		return err
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return err
	}
	// Load requires these directories. Their live contents are deliberately not
	// included in the recovery set or the migration rehearsal.
	for _, name := range []string{agentctl.IdentityDirectory, agentctl.LogsDirectory, agentctl.VolumesDirectory} {
		if err := os.Mkdir(filepath.Join(destination, name), 0700); err != nil {
			return err
		}
	}
	for _, name := range agentMetadataFiles {
		path := filepath.Join(source, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular agent metadata: %s", path)
		}
		if err := copyRegular(path, filepath.Join(destination, name), 0600); err != nil {
			return err
		}
	}
	if err := validatePrivateTree(destination); err != nil {
		return err
	}
	return syncTree(destination)
}

func restoreAgentMetadata(source, destination string) error {
	// Validate all inputs before changing any live metadata. Legacy recovery
	// sets may contain application data; select only the same explicit allowlist.
	stage, err := os.MkdirTemp(filepath.Dir(source), "agent-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	snapshot := filepath.Join(stage, "metadata")
	if err := snapshotAgentMetadata(source, snapshot); err != nil {
		return err
	}
	if err := checkMetadataDirectory(destination); err != nil {
		return err
	}
	if err := checkMetadataDirectory(filepath.Join(destination, agentctl.IdentityDirectory)); err != nil {
		return err
	}
	for _, name := range agentMetadataFiles {
		path := filepath.Join(destination, name)
		if info, err := os.Lstat(path); err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("non-regular agent metadata: %s", path)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, name := range agentMetadataFiles {
		from, to := filepath.Join(snapshot, name), filepath.Join(destination, name)
		if _, err := os.Lstat(from); errors.Is(err, os.ErrNotExist) {
			// In particular, remove candidate SQLite sidecars absent at backup.
			if err := os.Remove(to); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else if err != nil {
			return err
		} else if err := replaceRegular(from, to, 0600); err != nil {
			return err
		}
	}
	if err := syncDir(filepath.Join(destination, agentctl.IdentityDirectory)); err != nil {
		return err
	}
	return syncDir(destination)
}
