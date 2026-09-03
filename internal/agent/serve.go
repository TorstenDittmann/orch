package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"change.codes/torsten/better-swarm/internal/agent/state"
	orchruntime "change.codes/torsten/better-swarm/internal/runtime"
)

type ServeOptions struct {
	DataDir     string
	Snapshotter string
	Runtime     orchruntime.Runtime
	Now         func() time.Time
	Random      io.Reader
}

func Serve(ctx context.Context, options ServeOptions) error {
	dataDir, err := normalizeDataDir(options.DataDir)
	if err != nil {
		return err
	}
	credentials, err := Load(dataDir, currentTime(options.Now))
	if err != nil {
		return err
	}
	lock, err := acquireDataLock(dataDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	if options.Runtime == nil {
		return fmt.Errorf("runtime implementation is required")
	}
	defer options.Runtime.Close()
	cache, err := state.Open(ctx, filepath.Join(dataDir, DatabaseFilename), credentials.Config.ClusterID, credentials.Config.NodeID)
	if err != nil {
		return fmt.Errorf("open agent cache: %w", err)
	}
	defer cache.Close()
	capabilities, err := options.Runtime.Preflight(ctx)
	if err != nil {
		return fmt.Errorf("runtime preflight: %w", err)
	}
	if capabilities.OperatingSystem != "linux" {
		return fmt.Errorf("agent requires Linux: %w", orchruntime.ErrUnsupported)
	}
	if !capabilities.CgroupsV2 {
		return fmt.Errorf("agent requires cgroups v2: %w", orchruntime.ErrUnsupported)
	}
	if options.Snapshotter == "" || capabilities.Snapshotter != options.Snapshotter {
		return fmt.Errorf("configured snapshotter %q is unavailable (runtime reports %q)", options.Snapshotter, capabilities.Snapshotter)
	}
	randomSource := options.Random
	if randomSource == nil {
		randomSource = rand.Reader
	}
	supervisor := NewSupervisor(cache, options.Runtime, options.Now, randomSource)
	if cached, err := cache.Assignment(ctx); err == nil {
		if _, err := supervisor.Reconcile(ctx, cached); err != nil {
			return fmt.Errorf("reconcile cached assignment: %w", err)
		}
	} else if !errors.Is(err, state.ErrNoAssignment) {
		return err
	}
	bootID, err := newBootID(randomSource)
	if err != nil {
		return err
	}
	runner := sessionRunner{
		credentials: credentials, cache: cache, supervisor: supervisor,
		capabilities: capabilities, bootID: bootID, now: options.Now, random: randomSource,
	}
	return runner.run(ctx)
}

func newBootID(source io.Reader) (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(source, value); err != nil {
		return "", fmt.Errorf("generate agent boot id: %w", err)
	}
	return "boot_" + hex.EncodeToString(value), nil
}
