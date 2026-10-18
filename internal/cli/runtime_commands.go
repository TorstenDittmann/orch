package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	agentctl "github.com/TorstenDittmann/orch/internal/agent"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/discovery"
	"github.com/TorstenDittmann/orch/internal/ingress"
	"github.com/TorstenDittmann/orch/internal/network"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
	containerdruntime "github.com/TorstenDittmann/orch/internal/runtime/containerd"
)

func runAgent(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printAgentHelp(stderr)
		return ExitInvalid
	}
	switch args[0] {
	case "init":
		return runAgentInit(ctx, args[1:], stdout, stderr)
	case "serve":
		return runAgentServe(ctx, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printAgentHelp(stdout)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "unknown agent command %q\n\n", args[0])
		printAgentHelp(stderr)
		return ExitInvalid
	}
}

func runAgentInit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("agent init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var joinFile, dataDir string
	var jsonOutput bool
	flags.StringVar(&joinFile, "join-file", "", "private join artifact path or - for stdin")
	flags.StringVar(&dataDir, "data-dir", "", "agent state directory")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 || joinFile == "" || dataDir == "" {
		writeError(stderr, errors.New("agent init requires --join-file and --data-dir"), jsonOutput)
		return ExitInvalid
	}
	result, err := agentctl.EnrollWithJoin(ctx, agentctl.JoinEnrollOptions{JoinFile: joinFile, JoinInput: os.Stdin, DataDir: dataDir})
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			Created   bool   `json:"created"`
			NodeID    string `json:"node_id"`
			Name      string `json:"name"`
			ClusterID string `json:"cluster_id"`
			Server    string `json:"server"`
			DataDir   string `json:"data_dir"`
		}{result.Created, result.Config.NodeID, result.Config.NodeName, result.Config.ClusterID, result.Config.Server, result.DataDir})
	}
	verb := "Initialized"
	if !result.Created {
		verb = "Loaded existing"
	}
	fmt.Fprintf(stdout, "%s agent %s (%s)\nCluster: %s\nServer:  %s\nData:    %s\n",
		verb, result.Config.NodeName, result.Config.NodeID, result.Config.ClusterID, result.Config.Server, result.DataDir)
	return ExitOK
}

func runAgentServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("agent serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var dataDir, socket, snapshotter, cniBinDir, caddyBinary, ingressUser, ingressDataDir, advertiseAddress string
	var systemReservedCPU, systemReservedMemory string
	var wireGuardPort uint
	flags.StringVar(&dataDir, "data-dir", "", "agent state directory")
	flags.StringVar(&socket, "containerd-socket", "", "containerd socket path")
	flags.StringVar(&snapshotter, "snapshotter", "overlayfs", "containerd snapshotter")
	flags.StringVar(&cniBinDir, "cni-bin-dir", "/opt/cni/bin", "directory containing official CNI plugins")
	flags.StringVar(&caddyBinary, "caddy-binary", "/usr/bin/caddy", "pinned Caddy executable")
	flags.StringVar(&ingressUser, "ingress-user", "orch-ingress", "dedicated unprivileged Caddy user")
	flags.StringVar(&ingressDataDir, "ingress-data-dir", "/var/lib/orch-ingress", "private Caddy data directory owned by --ingress-user")
	flags.StringVar(&advertiseAddress, "advertise-address", "", "private or public IPv4 address reachable by the other nodes")
	flags.UintVar(&wireGuardPort, "wireguard-port", 51820, "WireGuard UDP listen port")
	flags.StringVar(&systemReservedCPU, "system-reserved-cpu", "500m", "CPU reserved for the host (for example 500m)")
	flags.StringVar(&systemReservedMemory, "system-reserved-memory", "512Mi", "memory reserved for the host (for example 512Mi)")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 || dataDir == "" || socket == "" || snapshotter == "" || cniBinDir == "" || caddyBinary == "" || ingressUser == "" || ingressDataDir == "" || advertiseAddress == "" {
		fmt.Fprintln(stderr, "agent serve requires nonempty runtime, CNI, Caddy, and --advertise-address values")
		return ExitInvalid
	}
	reservedCPU, err := parseReservedCPU(systemReservedCPU)
	if err != nil {
		writeError(stderr, fmt.Errorf("invalid --system-reserved-cpu: %w", err), false)
		return ExitInvalid
	}
	reservedMemory, err := parseReservedMemory(systemReservedMemory)
	if err != nil {
		writeError(stderr, fmt.Errorf("invalid --system-reserved-memory: %w", err), false)
		return ExitInvalid
	}
	credentials, err := agentctl.Load(dataDir, time.Now())
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	localNetwork, err := network.New(network.Options{
		ClusterID: credentials.Config.ClusterID, NodeID: credentials.Config.NodeID,
		BinDir: cniBinDir, StateDir: filepath.Join(dataDir, "network"),
	})
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	if wireGuardPort == 0 || wireGuardPort > math.MaxUint16 {
		_ = localNetwork.Close()
		writeError(stderr, errors.New("--wireguard-port must be between 1 and 65535"), false)
		return ExitInvalid
	}
	wireGuard, wireGuardErr := network.NewWireGuard(network.WireGuardOptions{
		ClusterID: credentials.Config.ClusterID, NodeID: credentials.Config.NodeID,
		StateDir: filepath.Join(dataDir, "network", "wireguard"),
	})
	if wireGuardErr != nil {
		_ = localNetwork.Close()
		writeError(stderr, wireGuardErr, false)
		return ExitRuntime
	}
	networkManager, err := network.NewMesh(localNetwork, wireGuard, advertiseAddress, uint16(wireGuardPort))
	if err != nil {
		_ = wireGuard.Close()
		_ = localNetwork.Close()
		writeError(stderr, err, false)
		return ExitInvalid
	}
	runtimeClient, err := containerdruntime.New(socket, orchruntime.Options{
		ClusterID: credentials.Config.ClusterID, NodeID: credentials.Config.NodeID,
		Snapshotter: snapshotter, LogsDir: filepath.Join(dataDir, agentctl.LogsDirectory),
		VolumesDir:   filepath.Join(dataDir, agentctl.VolumesDirectory),
		SecretsDir:   filepath.Join(dataDir, "runtime-secrets"),
		ResolverPath: filepath.Join(dataDir, "network", "resolv.conf"), Network: networkManager,
	})
	if err != nil {
		_ = networkManager.Close()
		writeError(stderr, err, false)
		return ExitRuntime
	}
	forwarders, err := discovery.SystemForwarders("/etc/resolv.conf")
	if err != nil {
		_ = runtimeClient.Close()
		writeError(stderr, err, false)
		return ExitRuntime
	}
	dnsServer, err := discovery.New(discovery.DefaultZone, forwarders)
	if err != nil {
		_ = runtimeClient.Close()
		writeError(stderr, err, false)
		return ExitRuntime
	}
	caddy, err := ingress.NewCaddy(ingress.CaddyOptions{
		Binary: caddyBinary, DataDir: ingressDataDir, StateDir: filepath.Join(dataDir, "ingress"), User: ingressUser,
		AdminSocket: filepath.Join(ingressDataDir, "admin.sock"),
	})
	if err != nil {
		_ = runtimeClient.Close()
		writeError(stderr, err, false)
		return ExitRuntime
	}
	fmt.Fprintf(stdout, "Agent %s connecting to %s\n", credentials.Config.NodeName, credentials.Config.Server)
	if err := agentctl.Serve(ctx, agentctl.ServeOptions{
		DataDir: dataDir, Snapshotter: snapshotter, Runtime: runtimeClient,
		Discovery: dnsServer, Ingress: caddy, NodePorts: networkManager, SystemReservedCPUMillis: reservedCPU,
		SystemReservedMemoryBytes: reservedMemory,
	}); err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	return ExitOK
}

func parseReservedCPU(value string) (uint64, error) {
	if !strings.HasSuffix(value, "m") {
		return 0, errors.New("must use positive millicores with an m suffix")
	}
	amount, err := strconv.ParseUint(strings.TrimSuffix(value, "m"), 10, 64)
	if err != nil || amount == 0 {
		return 0, errors.New("must use positive millicores with an m suffix")
	}
	return amount, nil
}

func parseReservedMemory(value string) (uint64, error) {
	units := map[string]uint64{"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40}
	for suffix, multiplier := range units {
		if !strings.HasSuffix(value, suffix) {
			continue
		}
		amount, err := strconv.ParseUint(strings.TrimSuffix(value, suffix), 10, 64)
		if err != nil || amount == 0 || amount > math.MaxUint64/multiplier {
			return 0, errors.New("must use a positive binary unit such as 512Mi")
		}
		return amount * multiplier, nil
	}
	return 0, errors.New("must use a positive binary unit such as 512Mi")
}

func runNodes(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("nodes", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 || contextPath == "" {
		writeError(stderr, errors.New("nodes requires --context"), jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	nodes, err := remote.ListNodes(ctx)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, nodes)
	}
	if len(nodes) == 0 {
		fmt.Fprintln(stdout, "No agent is enrolled.")
		return ExitOK
	}
	for _, node := range nodes {
		lastSeen := "never"
		if node.GetLastSeenAt() != nil && node.GetLastSeenAt().CheckValid() == nil {
			lastSeen = node.GetLastSeenAt().AsTime().Format(time.RFC3339Nano)
		}
		fmt.Fprintf(stdout, "%s  %s/%s  %s/%s  assignment %d (acked %d)  last seen %s\n",
			node.GetName(), node.GetLifecycle().String(), node.GetConnectionStatus().String(), node.GetOperatingSystem(), node.GetArchitecture(),
			node.GetAssignmentGeneration(), node.GetAcknowledgedGeneration(), lastSeen)
		fmt.Fprintf(stdout, "  capacity=%dm/%dB allocatable=%dm/%dB assigned=%dm/%dB labels=%s\n",
			node.GetTotalCpuMillis(), node.GetTotalMemoryBytes(), node.GetAllocatableCpuMillis(), node.GetAllocatableMemoryBytes(),
			node.GetAssignedCpuMillis(), node.GetAssignedMemoryBytes(), formatNodeLabels(node.GetLabels()))
		if usage := node.GetUsage(); usage != nil {
			fmt.Fprintf(stdout, "  usage cpu=%dm memory=%dB disk=%dB/%dB\n",
				usage.GetCpuUsedMillis(), usage.GetMemoryUsedBytes(), usage.GetDiskUsedBytes(), usage.GetDiskTotalBytes())
		}
		fmt.Fprintf(stdout, "  network=%s reason=%s subnet=%s mesh=%s:%d key=%t cni=%s plugins=%s nftables=%t dns-capable=%t dns=%s ingress-role=%t caddy=%s ingress=%s draining=%d\n",
			node.GetNetworkState(), statusValue(node.GetNetworkReason()), node.GetNodeSubnet(), node.GetAdvertiseAddress(), node.GetWireguardListenPort(), node.GetWireguardPublicKey() != "", node.GetCniVersion(), node.GetCniPluginsVersion(), node.GetNftables(),
			node.GetDnsAvailable(), node.GetDnsState(), node.GetIngress(), node.GetCaddyVersion(), node.GetIngressState(), node.GetDrainingObjects())
		fmt.Fprintf(stdout, "  drain-blockers=%s\n", statusValue(strings.Join(node.GetDrainBlockers(), ",")))
		certificateExpiry := "unknown"
		if node.GetCertificateNotAfter() != nil && node.GetCertificateNotAfter().CheckValid() == nil {
			certificateExpiry = node.GetCertificateNotAfter().AsTime().Format(time.RFC3339Nano)
		}
		fmt.Fprintf(stdout, "  certificate=%s expires=%s reason=%s\n", node.GetCertificateState(), certificateExpiry, statusValue(node.GetCertificateReason()))
	}
	return ExitOK
}

func formatNodeLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, key+"="+labels[key])
	}
	return strings.Join(values, ",")
}

func runApp(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "history" || args[0] == "diff" || args[0] == "watch") {
		return runAppDeployments(ctx, args, stdout, stderr)
	}
	if len(args) > 0 && args[0] == "list" {
		return runAppList(ctx, args[1:], stdout, stderr)
	}
	if len(args) == 0 || args[0] != "status" {
		fmt.Fprintln(stderr, "usage: orch app list|status|history|diff|watch ...")
		return ExitInvalid
	}
	flags := flag.NewFlagSet("app status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	reordered := reorderInterspersed(args[1:], map[string]bool{"-context": true, "--context": true})
	if err := flags.Parse(reordered); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 1 || contextPath == "" {
		writeError(stderr, errors.New("app status requires NAME and --context"), jsonOutput)
		return ExitInvalid
	}
	if err := config.ValidateApplicationName(flags.Arg(0)); err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	application, err := remote.GetApplication(ctx, flags.Arg(0))
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, application)
	}
	transition := "-"
	if application.GetExecutionUpdatedAt() != nil && application.GetExecutionUpdatedAt().CheckValid() == nil {
		transition = application.GetExecutionUpdatedAt().AsTime().Format(time.RFC3339Nano)
	}
	fmt.Fprintf(stdout, "Application: %s\nDesired:     %d (%s)\nObserved:    %d\nStatus:      %s\nReason:      %s\nMessage:     %s\nTransition:  %s\n",
		application.GetName(), application.GetGeneration(), statusValue(application.GetRevisionId()),
		application.GetObservedGeneration(), application.GetExecutionStatus().String(),
		statusValue(application.GetExecutionReason()), statusValue(application.GetExecutionMessage()), transition)
	if application.GetActiveGeneration() != 0 {
		fmt.Fprintf(stdout, "Active:      %d (%s)\n", application.GetActiveGeneration(), statusValue(application.GetActiveRevisionId()))
	} else {
		fmt.Fprintln(stdout, "Active:      -")
	}
	for _, allocation := range application.GetAllocations() {
		fmt.Fprintf(stdout, "  %s/%d  %s  health=%s address=%s port=%d active=%d/%s active_spec=%s restarts=%d reason=%s\n",
			allocation.GetService(), allocation.GetReplica(), allocation.GetPhase().String(),
			allocation.GetHealthStatus().String(), statusValue(allocation.GetAddress()), allocation.GetDeclaredPort(),
			allocation.GetActiveApplicationGeneration(), statusValue(allocation.GetActiveRevisionId()),
			statusValue(allocation.GetActiveSpecDigest()), allocation.GetRestartCount(), statusValue(allocation.GetReason()))
		if failingAllocation(allocation) {
			fmt.Fprintf(stdout, "    %s\n", allocation.GetMessage())
		}
	}
	for _, route := range application.GetRoutes() {
		protocol := "http"
		if route.GetHttps() {
			protocol = "https"
		}
		fmt.Fprintf(stdout, "  route %s://%s  %s  certificate=%s upstreams=%d reason=%s\n",
			protocol, route.GetDomain(), route.GetState().String(), statusValue(route.GetCertificateState()),
			route.GetUpstreamCount(), statusValue(route.GetReason()))
	}
	for _, route := range application.GetTcpRoutes() {
		fmt.Fprintf(stdout, "  route tcp://0.0.0.0:%d -> %s:%d  %s upstreams=%d reason=%s\n",
			route.GetPublicPort(), route.GetService(), route.GetTargetPort(), route.GetState().String(),
			route.GetUpstreamCount(), statusValue(route.GetReason()))
	}
	for _, port := range application.GetNodePorts() {
		fmt.Fprintf(stdout, "  node port %s/%d on %s -> %s:%d  %s reason=%s\n",
			port.GetProtocol(), port.GetNodePort(), statusValue(port.GetNodeName()), port.GetService(),
			port.GetTargetPort(), port.GetState().String(), statusValue(port.GetReason()))
	}
	for _, job := range application.GetCronJobs() {
		next := "-"
		if job.GetNextRunAt() != nil && job.GetNextRunAt().CheckValid() == nil {
			next = job.GetNextRunAt().AsTime().Format(time.RFC3339)
		}
		fmt.Fprintf(stdout, "  cron %s  next=%s active=%s last=%s/%s reason=%s\n", job.GetJob(), next,
			statusValue(job.GetActiveRunId()), statusValue(job.GetLastRunId()), job.GetLastState().String(), statusValue(job.GetReason()))
	}
	return ExitOK
}

// runAppList prints every application on the cluster with its rollout state.
func runAppList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("app list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 || contextPath == "" {
		writeError(stderr, errors.New("app list accepts flags only and requires --context"), jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	applications, err := remote.ListApplications(ctx)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if applications == nil {
		applications = []*orchv1alpha1.ApplicationSummary{}
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			Applications []*orchv1alpha1.ApplicationSummary `json:"applications"`
		}{Applications: applications})
	}
	if len(applications) == 0 {
		fmt.Fprintln(stdout, "No applications. Deploy one with: orch deploy")
		return ExitOK
	}
	for _, application := range applications {
		fmt.Fprintf(stdout, "%s  generation=%d revision=%s status=%s reason=%s\n",
			application.GetName(), application.GetGeneration(), statusValue(application.GetRevisionId()),
			application.GetExecutionStatus().String(), statusValue(application.GetExecutionReason()))
	}
	return ExitOK
}

func runCron(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: orch cron run|runs|logs ...")
		return ExitInvalid
	}
	switch args[0] {
	case "run":
		return runCronRun(ctx, args[1:], stdout, stderr)
	case "runs":
		return runCronRuns(ctx, args[1:], stdout, stderr)
	case "logs":
		return runCronLogs(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, "usage: orch cron run|runs|logs ...")
		return ExitInvalid
	}
}

// runCronRun starts one run of a scheduled job now, with the settings and on
// the node its schedule uses.
func runCronRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("cron run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath, key string
	var wait, jsonOutput bool
	var waitTimeout time.Duration
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.StringVar(&key, "idempotency-key", "", "stable key for safe retries")
	flags.BoolVar(&wait, "wait", false, "wait for the run to finish")
	flags.DurationVar(&waitTimeout, "wait-timeout", 20*time.Minute, "maximum time --wait waits (1s-24h)")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	reordered := reorderInterspersed(args, map[string]bool{"-context": true, "--context": true, "-idempotency-key": true, "--idempotency-key": true, "-wait-timeout": true, "--wait-timeout": true})
	if err := flags.Parse(reordered); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 2 || contextPath == "" || waitTimeout < time.Second || waitTimeout > 24*time.Hour {
		fmt.Fprintln(stderr, "cron run requires APP JOB, --context, and --wait-timeout from 1s to 24h")
		return ExitInvalid
	}
	if err := validateApplicationService(flags.Arg(0), flags.Arg(1)); err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitInvalid
	}
	if key == "" {
		var err error
		if key, err = newIdempotencyKey(rand.Reader); err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
	}
	fmt.Fprintf(stderr, "Idempotency key: %s\n", key)
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	job, err := remote.RunCronJob(ctx, flags.Arg(0), flags.Arg(1), key)
	if err == nil && wait {
		waitContext, cancel := context.WithTimeout(ctx, waitTimeout)
		job, err = remote.WaitJob(waitContext, job.GetId())
		cancel()
	}
	if job != nil {
		if jsonOutput {
			if code := writeJSON(stdout, stderr, job); code != ExitOK {
				return code
			}
		} else {
			printJob(stdout, job)
			fmt.Fprintf(stdout, "Logs: orch job logs %s\n", job.GetId())
		}
	}
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if wait && job.GetStatus().GetState() != orchv1alpha1.JobRunState_JOB_RUN_STATE_SUCCEEDED {
		return ExitRuntime
	}
	return ExitOK
}

func runCronRuns(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("cron runs", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var limit uint
	var jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.UintVar(&limit, "limit", 20, "maximum runs (1-100)")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	reordered := reorderInterspersed(args, map[string]bool{"-context": true, "--context": true, "-limit": true, "--limit": true})
	if err := flags.Parse(reordered); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 2 || contextPath == "" || limit == 0 || limit > 100 {
		fmt.Fprintln(stderr, "cron runs requires APP JOB, --context, and --limit from 1 to 100")
		return ExitInvalid
	}
	if err := validateApplicationService(flags.Arg(0), flags.Arg(1)); err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	runs, err := remote.ListCronRuns(ctx, flags.Arg(0), flags.Arg(1), uint32(limit))
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, runs)
	}
	for _, run := range runs {
		scheduled := "-"
		if run.GetScheduledAt() != nil {
			scheduled = run.GetScheduledAt().AsTime().Format(time.RFC3339Nano)
		}
		fmt.Fprintf(stdout, "%s  %s  %s  reason=%s\n", run.GetId(), scheduled, run.GetState().String(), statusValue(run.GetReason()))
	}
	return ExitOK
}

func runCronLogs(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("cron logs", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var tail uint
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.UintVar(&tail, "tail", 100, "maximum lines per stream")
	reordered := reorderInterspersed(args, map[string]bool{"-context": true, "--context": true, "-tail": true, "--tail": true})
	if err := flags.Parse(reordered); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 3 || contextPath == "" || tail == 0 || tail > 10_000 {
		fmt.Fprintln(stderr, "cron logs requires APP JOB RUN_ID, --context, and bounded --tail")
		return ExitInvalid
	}
	if err := validateApplicationService(flags.Arg(0), flags.Arg(1)); err != nil {
		writeError(stderr, err, false)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	stream, err := remote.TailCronLogs(ctx, flags.Arg(0), flags.Arg(1), flags.Arg(2), uint32(tail))
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	defer stream.Close()
	for stream.Receive() {
		frame := stream.Msg()
		output := stdout
		if frame.GetStream() == orchv1alpha1.LogStream_LOG_STREAM_STDERR {
			output = stderr
		}
		if _, err := output.Write(frame.GetData()); err != nil {
			writeError(stderr, err, false)
			return ExitRuntime
		}
	}
	if err := stream.Err(); err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	return ExitOK
}

func statusValue(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func runLogs(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("logs", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var replica, tail uint
	var follow, previous bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.UintVar(&replica, "replica", 1, "replica ordinal")
	flags.UintVar(&tail, "tail", 100, "maximum lines per stream")
	flags.BoolVar(&follow, "follow", false, "keep printing new output until interrupted")
	flags.BoolVar(&follow, "f", false, "keep printing new output until interrupted")
	flags.BoolVar(&previous, "previous", false, "show the replica's previous instance, kept for 24 hours after it was replaced")
	flags.BoolVar(&previous, "p", false, "show the replica's previous instance, kept for 24 hours after it was replaced")
	reordered := reorderInterspersed(args, map[string]bool{"-context": true, "--context": true, "-replica": true, "--replica": true, "-tail": true, "--tail": true})
	if err := flags.Parse(reordered); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 2 || contextPath == "" || replica == 0 || replica > math.MaxUint32 || tail == 0 || tail > 10_000 {
		fmt.Fprintln(stderr, "logs requires APP SERVICE, --context, and bounded --replica/--tail values")
		return ExitInvalid
	}
	if follow && previous {
		fmt.Fprintln(stderr, "logs --previous cannot be combined with --follow")
		return ExitInvalid
	}
	if err := validateApplicationService(flags.Arg(0), flags.Arg(1)); err != nil {
		writeError(stderr, err, false)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	if follow {
		return followLogs(ctx, remote, flags.Arg(0), flags.Arg(1), uint32(replica), uint32(tail), stdout, stderr)
	}
	tailLogs := remote.TailLogs
	if previous {
		tailLogs = remote.TailPreviousLogs
	}
	stream, err := tailLogs(ctx, flags.Arg(0), flags.Arg(1), uint32(replica), uint32(tail))
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	defer stream.Close()
	for stream.Receive() {
		frame := stream.Msg()
		output := stdout
		if frame.GetStream() == orchv1alpha1.LogStream_LOG_STREAM_STDERR {
			output = stderr
		}
		if _, err := output.Write(frame.GetData()); err != nil {
			writeError(stderr, err, false)
			return ExitRuntime
		}
	}
	if err := stream.Err(); err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	return ExitOK
}

// logFollowInterval is how often orch logs -f asks for new output.
var logFollowInterval = time.Second

// followLogs prints a replica's output as it arrives until ctx ends. It keeps
// going through restarts and short disconnects, saying so on stderr.
func followLogs(ctx context.Context, remote *controlclient.Client, application, service string, replica, tail uint32, stdout, stderr io.Writer) int {
	cursor := ""
	waiting := ""
	for {
		stream, err := remote.TailLogsAfter(ctx, application, service, replica, tail, cursor)
		if err == nil {
			for stream.Receive() {
				frame := stream.Msg()
				if waiting != "" {
					fmt.Fprintln(stderr, "--- logs resumed ---")
					waiting = ""
				}
				if frame.GetCursorReset() && cursor != "" {
					fmt.Fprintln(stderr, "--- the replica restarted ---")
				}
				if frame.GetNextCursor() != "" {
					cursor = frame.GetNextCursor()
				}
				output := stdout
				if frame.GetStream() == orchv1alpha1.LogStream_LOG_STREAM_STDERR {
					output = stderr
				}
				if _, err := output.Write(frame.GetData()); err != nil {
					_ = stream.Close()
					writeError(stderr, err, false)
					return ExitRuntime
				}
			}
			err = stream.Err()
			_ = stream.Close()
		}
		if ctx.Err() != nil {
			return ExitOK
		}
		if err != nil {
			code := connect.CodeOf(err)
			if code != connect.CodeNotFound && code != connect.CodeUnavailable && code != connect.CodeDeadlineExceeded {
				writeError(stderr, err, false)
				return ExitRuntime
			}
			if message := err.Error(); message != waiting {
				fmt.Fprintf(stderr, "--- waiting for logs: %s ---\n", message)
				waiting = message
			}
		}
		select {
		case <-ctx.Done():
			return ExitOK
		case <-time.After(logFollowInterval):
		}
	}
}

func runExec(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	separator := -1
	for index, value := range args {
		if value == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator == len(args)-1 {
		fmt.Fprintln(stderr, "exec requires -- followed by a non-interactive command")
		return ExitInvalid
	}
	command := append([]string(nil), args[separator+1:]...)
	flags := flag.NewFlagSet("exec", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var replica, timeout uint
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.UintVar(&replica, "replica", 1, "replica ordinal")
	flags.UintVar(&timeout, "timeout", 30, "timeout in seconds")
	reordered := reorderInterspersed(args[:separator], map[string]bool{"-context": true, "--context": true, "-replica": true, "--replica": true, "-timeout": true, "--timeout": true})
	if err := flags.Parse(reordered); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 2 || contextPath == "" || replica == 0 || replica > math.MaxUint32 || timeout == 0 || timeout > 300 {
		fmt.Fprintln(stderr, "exec requires APP SERVICE, --context, and bounded --replica/--timeout values")
		return ExitInvalid
	}
	if err := validateApplicationService(flags.Arg(0), flags.Arg(1)); err != nil {
		writeError(stderr, err, false)
		return ExitInvalid
	}
	if err := validateExecCommand(command); err != nil {
		writeError(stderr, err, false)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	stream, err := remote.Exec(ctx, flags.Arg(0), flags.Arg(1), uint32(replica), command, uint32(timeout))
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	defer stream.Close()
	exitCode := int32(0)
	exited := false
	for stream.Receive() {
		frame := stream.Msg()
		output := stdout
		if frame.GetStream() == orchv1alpha1.LogStream_LOG_STREAM_STDERR {
			output = stderr
		}
		if _, err := output.Write(frame.GetData()); err != nil {
			writeError(stderr, err, false)
			return ExitRuntime
		}
		if frame.GetExited() {
			exited = true
			exitCode = frame.GetExitCode()
		}
	}
	if err := stream.Err(); err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	if !exited {
		fmt.Fprintln(stderr, "agent did not return an exec exit status")
		return ExitRuntime
	}
	if exitCode != 0 {
		fmt.Fprintf(stderr, "command exited with status %d\n", exitCode)
		return ExitRuntime
	}
	return ExitOK
}

func runRemove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("remove", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath, idempotencyKey string
	var jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.StringVar(&idempotencyKey, "idempotency-key", "", "stable key for safe retries")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	reordered := reorderInterspersed(args, map[string]bool{"-context": true, "--context": true, "-idempotency-key": true, "--idempotency-key": true})
	if err := flags.Parse(reordered); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 1 || contextPath == "" {
		writeError(stderr, errors.New("remove requires APP and --context"), jsonOutput)
		return ExitInvalid
	}
	if err := config.ValidateApplicationName(flags.Arg(0)); err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitInvalid
	}
	var err error
	if idempotencyKey == "" {
		idempotencyKey, err = newIdempotencyKey(rand.Reader)
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
	}
	if !cliIdempotencyKeyPattern.MatchString(idempotencyKey) {
		writeError(stderr, errors.New("idempotency key must be 16-128 URL-safe characters"), jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	response, err := remote.RemoveApplication(ctx, flags.Arg(0), idempotencyKey)
	if err != nil {
		writeError(stderr, fmt.Errorf("remove request failed; retry with --idempotency-key %s: %w", idempotencyKey, err), jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			Application    string `json:"application"`
			Generation     uint64 `json:"generation"`
			Changed        bool   `json:"changed"`
			EventSequence  uint64 `json:"event_sequence"`
			Status         string `json:"status"`
			IdempotencyKey string `json:"idempotency_key"`
		}{response.GetName(), response.GetGeneration(), response.GetChanged(), response.GetEventSequence(), response.GetExecutionStatus().String(), idempotencyKey})
	}
	fmt.Fprintf(stdout, "Application %s: %s (generation %d).\nIdempotency key: %s\n",
		response.GetName(), response.GetExecutionStatus().String(), response.GetGeneration(), idempotencyKey)
	return ExitOK
}

func reorderInterspersed(args []string, valueFlags map[string]bool) []string {
	var options, positional []string
	for index := 0; index < len(args); index++ {
		value := args[index]
		if strings.HasPrefix(value, "-") {
			options = append(options, value)
			name := value
			if before, _, found := strings.Cut(value, "="); found {
				name = before
			}
			if valueFlags[name] && !strings.Contains(value, "=") && index+1 < len(args) {
				index++
				options = append(options, args[index])
			}
		} else {
			positional = append(positional, value)
		}
	}
	return append(options, positional...)
}

func validateApplicationService(application, service string) error {
	if err := config.ValidateApplicationName(application); err != nil {
		return fmt.Errorf("invalid application name: %w", err)
	}
	if err := config.ValidateApplicationName(service); err != nil {
		return fmt.Errorf("invalid service name: %w", err)
	}
	return nil
}

func validateExecCommand(command []string) error {
	if len(command) == 0 || len(command) > 64 {
		return errors.New("exec command must contain 1-64 arguments")
	}
	total := 0
	for _, argument := range command {
		if argument == "" || strings.ContainsRune(argument, 0) {
			return errors.New("exec command arguments must be nonempty and contain no NUL bytes")
		}
		total += len(argument)
	}
	if total > 8<<10 {
		return errors.New("exec command must not exceed 8 KiB")
	}
	return nil
}

func printAgentHelp(output io.Writer) {
	fmt.Fprintln(output, `orch agent manages an execution agent.

Usage:
  orch agent init --join-file FILE|- --data-dir DIR [--json]
	  orch agent serve --data-dir DIR --containerd-socket PATH --snapshotter NAME \
	    --advertise-address IPV4 [--wireguard-port PORT] \
	    [--system-reserved-cpu CPU] [--system-reserved-memory MEMORY] \
	    [--cni-bin-dir DIR] [--caddy-binary PATH] [--ingress-user USER] \
	    [--ingress-data-dir DIR]`)
}

func runRuntimeLogSink(ctx context.Context, args []string, stderr io.Writer) int {
	if len(args) != 1 {
		return ExitInvalid
	}
	if err := containerdruntime.RunLogSink(ctx, args[0]); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitRuntime
	}
	return ExitOK
}

// failingAllocation reports whether a replica is failing with a message worth
// printing under it, such as the registry error of a failed image pull.
func failingAllocation(allocation *orchv1alpha1.AllocationStatus) bool {
	if allocation.GetMessage() == "" {
		return false
	}
	switch allocation.GetPhase() {
	case orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_BACKOFF, orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_FAILED:
		return true
	default:
		return false
	}
}
