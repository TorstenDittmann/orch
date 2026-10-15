package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/security"
	"change.codes/torsten/better-swarm/internal/store"
)

type BackupOptions struct {
	DataDir         string
	Output          string
	SecretKeyOutput string
}

type BackupResult struct {
	Cluster         model.Cluster
	Output          string
	SecretKeyOutput string
}

// Backup creates a verified online SQLite backup without replacing an
// existing destination.
func Backup(ctx context.Context, options BackupOptions) (result BackupResult, finalErr error) {
	if strings.TrimSpace(options.DataDir) == "" || strings.TrimSpace(options.Output) == "" {
		return BackupResult{}, fmt.Errorf("%w: data directory and backup output are required", ErrInvalidOptions)
	}
	dataDir, err := filepath.Abs(options.DataDir)
	if err != nil {
		return BackupResult{}, fmt.Errorf("%w: resolve data directory: %v", ErrInvalidOptions, err)
	}
	output, err := filepath.Abs(options.Output)
	if err != nil {
		return BackupResult{}, fmt.Errorf("%w: resolve backup output: %v", ErrInvalidOptions, err)
	}
	dataDir = filepath.Clean(dataDir)
	output = filepath.Clean(output)
	if pathWithin(output, dataDir) {
		return BackupResult{}, fmt.Errorf("%w: backup output must be outside the live data directory", ErrInvalidOptions)
	}
	if filepath.Dir(filepath.Dir(output)) == filepath.Dir(output) {
		return BackupResult{}, fmt.Errorf("%w: backup output must be inside a non-root directory", ErrInvalidOptions)
	}
	if err := requirePrivateDirectory(dataDir); err != nil {
		return BackupResult{}, fmt.Errorf("validate data directory: %w", err)
	}
	databasePath := filepath.Join(dataDir, DatabaseFilename)
	info, err := os.Lstat(databasePath)
	if err != nil {
		return BackupResult{}, fmt.Errorf("inspect cluster database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return BackupResult{}, fmt.Errorf("cluster database is not a regular file: %s", databasePath)
	}
	// Read-only: a running controller may be on an older schema, and backup
	// must neither migrate nor checkpoint the live database underneath it.
	state, err := store.OpenReadOnly(ctx, databasePath)
	if err != nil {
		return BackupResult{}, fmt.Errorf("open cluster state: %w", err)
	}
	var published []string
	defer func() {
		if err := state.Close(); err != nil && finalErr == nil {
			finalErr = err
		}
		if finalErr != nil {
			// Never leave outputs behind that would make a retry refuse.
			for _, path := range published {
				_ = os.Remove(path)
			}
		}
	}()
	stored, err := state.Cluster(ctx)
	if err != nil {
		return BackupResult{}, fmt.Errorf("read cluster state: %w", err)
	}
	hasEncrypted, err := state.HasEncryptedMaterial(ctx)
	if err != nil {
		return BackupResult{}, err
	}
	fingerprint, err := state.SecretKeyFingerprint(ctx)
	if err != nil {
		return BackupResult{}, err
	}
	secretKeyOutput := ""
	if hasEncrypted || fingerprint != "" {
		if strings.TrimSpace(options.SecretKeyOutput) == "" {
			return BackupResult{}, fmt.Errorf("%w: --secret-key-output is required because the database records a secret master key or encrypted secrets", ErrInvalidOptions)
		}
		secretKeyOutput, err = filepath.Abs(options.SecretKeyOutput)
		if err != nil || secretKeyOutput == output || pathWithin(secretKeyOutput, dataDir) {
			return BackupResult{}, fmt.Errorf("%w: secret key output must be distinct and outside the live data directory", ErrInvalidOptions)
		}
		if err := copySecretKey(filepath.Join(dataDir, SecretKeyFilename), secretKeyOutput); err != nil {
			return BackupResult{}, err
		}
		published = append(published, secretKeyOutput)
	}
	if err := state.Backup(ctx, output); err != nil {
		return BackupResult{}, err
	}
	published = append(published, output)
	return BackupResult{Cluster: stored, Output: output, SecretKeyOutput: secretKeyOutput}, nil
}

func copySecretKey(source, destination string) (finalErr error) {
	if _, err := security.LoadSecretBox(source, nil); err != nil {
		return fmt.Errorf("validate cluster secret key: %w", err)
	}
	contents, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read cluster secret key: %w", err)
	}
	defer func() {
		for index := range contents {
			contents[index] = 0
		}
	}()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("create secret key backup directory: %w", err)
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create secret key backup: %w", err)
	}
	remove := true
	closed := false
	defer func() {
		if !closed {
			if err := file.Close(); err != nil && finalErr == nil {
				finalErr = err
			}
		}
		if remove {
			_ = os.Remove(destination)
		}
	}()
	if _, err := file.Write(contents); err != nil {
		return fmt.Errorf("write secret key backup: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync secret key backup: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close secret key backup: %w", err)
	}
	closed = true
	remove = false
	return nil
}
