package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/store"
)

type BackupOptions struct {
	DataDir string
	Output  string
}

type BackupResult struct {
	Cluster model.Cluster
	Output  string
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
	state, err := store.OpenInitialized(ctx, databasePath)
	if err != nil {
		return BackupResult{}, fmt.Errorf("open cluster state: %w", err)
	}
	defer func() {
		if err := state.Close(); err != nil && finalErr == nil {
			finalErr = err
		}
	}()
	stored, err := state.Cluster(ctx)
	if err != nil {
		return BackupResult{}, fmt.Errorf("read cluster state: %w", err)
	}
	if err := state.Backup(ctx, output); err != nil {
		return BackupResult{}, err
	}
	return BackupResult{Cluster: stored, Output: output}, nil
}
