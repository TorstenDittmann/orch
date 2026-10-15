package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/TorstenDittmann/orch/internal/agent/state"
	"github.com/TorstenDittmann/orch/internal/discovery"
	"github.com/TorstenDittmann/orch/internal/ingress"
	"github.com/TorstenDittmann/orch/internal/network"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
	"github.com/TorstenDittmann/orch/internal/security"
	"github.com/TorstenDittmann/orch/internal/tcpingress"
)

type ServeOptions struct {
	DataDir                   string
	Snapshotter               string
	Runtime                   orchruntime.Runtime
	Discovery                 discovery.Controller
	Ingress                   ingress.Controller
	TCPIngress                TCPIngressController
	NodePorts                 network.NodePortPublisher
	Now                       func() time.Time
	Random                    io.Reader
	SystemReservedCPUMillis   uint64
	SystemReservedMemoryBytes uint64
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
	secretBox, _, err := security.LoadOrCreateSecretBox(filepath.Join(dataDir, SecretCacheKeyFile), rand.Reader)
	if err != nil {
		return fmt.Errorf("load agent secret cache key: %w", err)
	}
	if err := cache.ConfigureSecretBox(ctx, secretBox); err != nil {
		return fmt.Errorf("configure agent secret cache: %w", err)
	}
	if configurable, ok := options.Runtime.(interface{ SetAttachmentStore(network.AttachmentStore) }); ok {
		configurable.SetAttachmentStore(cache)
	}
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
	if options.SystemReservedCPUMillis != 0 || options.SystemReservedMemoryBytes != 0 {
		if options.SystemReservedCPUMillis >= capabilities.TotalCPUMillis ||
			options.SystemReservedMemoryBytes >= capabilities.TotalMemoryBytes {
			return fmt.Errorf("system reservations must leave positive allocatable CPU and memory")
		}
		capabilities.AllocatableCPUMillis = capabilities.TotalCPUMillis - options.SystemReservedCPUMillis
		capabilities.AllocatableMemoryBytes = capabilities.TotalMemoryBytes - options.SystemReservedMemoryBytes
	}
	ingressController := options.Ingress
	if options.Ingress != nil {
		if version, ingressErr := options.Ingress.Preflight(ctx); ingressErr == nil {
			capabilities.CaddyVersion = version
		} else {
			ingressController = nil
		}
		defer options.Ingress.Close()
	}
	tcpController := options.TCPIngress
	if tcpController == nil {
		tcpController = tcpingress.New(tcpingress.DefaultDrainTimeout)
	}
	defer tcpController.Close()
	if options.Discovery != nil {
		capabilities.DNSAvailable = true
		defer func() {
			closeContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = options.Discovery.Close(closeContext)
		}()
	}
	randomSource := options.Random
	if randomSource == nil {
		randomSource = rand.Reader
	}
	supervisor := NewSupervisorWithServices(cache, options.Runtime, options.Now, SupervisorServices{
		Discovery: options.Discovery, Ingress: ingressController, TCPIngress: tcpController,
		NodePorts: options.NodePorts,
	}, randomSource)
	bootID, err := newBootID(randomSource)
	if err != nil {
		return err
	}
	runner := sessionRunner{
		credentials: credentials, cache: cache, supervisor: supervisor,
		capabilities: capabilities, bootID: bootID, now: options.Now, random: randomSource, dataDir: dataDir,
	}
	// run reconciles the cached assignment before its first connection
	// attempt. A reconcile failure there is logged and retried instead of
	// exiting, so the agent can still receive the assignment that fixes it.
	return runner.run(ctx)
}

func newBootID(source io.Reader) (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(source, value); err != nil {
		return "", fmt.Errorf("generate agent boot id: %w", err)
	}
	return "boot_" + hex.EncodeToString(value), nil
}
