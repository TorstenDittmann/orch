package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/TorstenDittmann/orch/internal/security"
	"github.com/TorstenDittmann/orch/internal/store"
)

// BackupsDirectory holds the controller's automatic backup sets inside its
// data directory. Each set is a directory named by its UTC creation time.
const BackupsDirectory = "backups"

// backupSetLayout is the name format of backup set directories; it sorts in
// creation order.
const backupSetLayout = "20060102T150405Z"

// BackupSet is one complete, restorable controller backup.
type BackupSet struct {
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"created_at"`
}

// WriteBackupSet writes a consistent snapshot of the live database, the
// secret key, and the controller identity into a new directory under
// DATA_DIR/backups. The set is staged and renamed into place, so a listed set
// is always complete.
func WriteBackupSet(ctx context.Context, state *store.Store, dataDir string, now time.Time) (BackupSet, error) {
	root := filepath.Join(dataDir, BackupsDirectory)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return BackupSet{}, fmt.Errorf("create backups directory: %w", err)
	}
	created := now.UTC().Truncate(time.Second)
	destination := filepath.Join(root, created.Format(backupSetLayout))
	if _, err := os.Lstat(destination); err == nil {
		return BackupSet{}, fmt.Errorf("backup set already exists: %s", destination)
	}
	stage, err := os.MkdirTemp(root, ".partial-*")
	if err != nil {
		return BackupSet{}, fmt.Errorf("stage backup set: %w", err)
	}
	defer os.RemoveAll(stage)
	if err := state.Backup(ctx, filepath.Join(stage, DatabaseFilename)); err != nil {
		return BackupSet{}, err
	}
	key := filepath.Join(dataDir, SecretKeyFilename)
	if exists, err := pathExists(key); err != nil {
		return BackupSet{}, err
	} else if exists {
		if _, err := security.LoadSecretBox(key, nil); err != nil {
			return BackupSet{}, fmt.Errorf("validate cluster secret key: %w", err)
		}
		if err := copyRecoveryFile(ctx, key, filepath.Join(stage, SecretKeyFilename), security.SecretKeySize); err != nil {
			return BackupSet{}, fmt.Errorf("copy secret key: %w", err)
		}
	}
	pki := filepath.Join(stage, controllerIdentityDirectory)
	if err := os.Mkdir(pki, 0o700); err != nil {
		return BackupSet{}, err
	}
	for _, name := range controllerPKIFiles {
		if err := copyRecoveryFile(ctx, filepath.Join(dataDir, controllerIdentityDirectory, name), filepath.Join(pki, name), 1<<20); err != nil {
			return BackupSet{}, fmt.Errorf("copy controller identity %s: %w", name, err)
		}
	}
	for _, directory := range []string{pki, stage} {
		if err := syncDirectory(directory); err != nil {
			return BackupSet{}, err
		}
	}
	if err := os.Rename(stage, destination); err != nil {
		return BackupSet{}, fmt.Errorf("publish backup set: %w", err)
	}
	if err := syncDirectory(root); err != nil {
		return BackupSet{}, err
	}
	return BackupSet{Path: destination, CreatedAt: created}, nil
}

// ListBackupSets returns the complete backup sets under DATA_DIR/backups,
// newest first. A missing directory means there are none.
func ListBackupSets(dataDir string) ([]BackupSet, error) {
	root := filepath.Join(dataDir, BackupsDirectory)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sets []BackupSet
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		created, err := time.Parse(backupSetLayout, entry.Name())
		if err != nil {
			continue
		}
		sets = append(sets, BackupSet{Path: filepath.Join(root, entry.Name()), CreatedAt: created})
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].CreatedAt.After(sets[j].CreatedAt) })
	return sets, nil
}

// PruneBackupSets keeps the newest keep sets and removes the rest, along
// with partial sets an interrupted backup left behind.
func PruneBackupSets(dataDir string, keep int) error {
	root := filepath.Join(dataDir, BackupsDirectory)
	if entries, err := os.ReadDir(root); err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".partial-") {
				_ = os.RemoveAll(filepath.Join(root, entry.Name()))
			}
		}
	}
	sets, err := ListBackupSets(dataDir)
	if err != nil {
		return err
	}
	var errs []error
	for index := keep; index < len(sets); index++ {
		if err := os.RemoveAll(sets[index].Path); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// resolveBackupSet lets recovery take a backup set directory as --backup and
// fills in its key and identity unless they were given explicitly.
func resolveBackupSet(options RecoveryOptions) (RecoveryOptions, error) {
	if strings.TrimSpace(options.Backup) == "" {
		return options, nil
	}
	info, err := os.Lstat(options.Backup)
	if err != nil || !info.IsDir() {
		return options, nil
	}
	set := options.Backup
	options.Backup = filepath.Join(set, DatabaseFilename)
	if options.PKIDir == "" {
		options.PKIDir = filepath.Join(set, controllerIdentityDirectory)
	}
	if options.SecretKey == "" {
		key := filepath.Join(set, SecretKeyFilename)
		if exists, err := pathExists(key); err != nil {
			return options, err
		} else if exists {
			options.SecretKey = key
		}
	}
	return options, nil
}
