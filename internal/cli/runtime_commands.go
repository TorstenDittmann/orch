package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strings"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	agentctl "change.codes/torsten/better-swarm/internal/agent"
	controlclient "change.codes/torsten/better-swarm/internal/client"
	"change.codes/torsten/better-swarm/internal/config"
	orchruntime "change.codes/torsten/better-swarm/internal/runtime"
	containerdruntime "change.codes/torsten/better-swarm/internal/runtime/containerd"
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
	var name, contextPath, dataDir string
	var jsonOutput bool
	flags.StringVar(&name, "name", "", "agent DNS-label name")
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.StringVar(&dataDir, "data-dir", "", "agent state directory")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 || name == "" || contextPath == "" || dataDir == "" {
		writeError(stderr, errors.New("agent init requires --name, --context, and --data-dir"), jsonOutput)
		return ExitInvalid
	}
	if err := config.ValidateApplicationName(name); err != nil {
		writeError(stderr, fmt.Errorf("invalid agent name: %w", err), jsonOutput)
		return ExitInvalid
	}
	result, err := agentctl.Enroll(ctx, agentctl.EnrollOptions{Name: name, ContextPath: contextPath, DataDir: dataDir})
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
	var dataDir, socket, snapshotter string
	flags.StringVar(&dataDir, "data-dir", "", "agent state directory")
	flags.StringVar(&socket, "containerd-socket", "", "containerd socket path")
	flags.StringVar(&snapshotter, "snapshotter", "overlayfs", "containerd snapshotter")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 || dataDir == "" || socket == "" || snapshotter == "" {
		fmt.Fprintln(stderr, "agent serve requires --data-dir, --containerd-socket, and --snapshotter")
		return ExitInvalid
	}
	credentials, err := agentctl.Load(dataDir, time.Now())
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	runtimeClient, err := containerdruntime.New(socket, orchruntime.Options{
		ClusterID: credentials.Config.ClusterID, NodeID: credentials.Config.NodeID,
		Snapshotter: snapshotter, LogsDir: filepath.Join(dataDir, agentctl.LogsDirectory),
		VolumesDir: filepath.Join(dataDir, agentctl.VolumesDirectory),
	})
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	fmt.Fprintf(stdout, "Agent %s connecting to %s\n", credentials.Config.NodeName, credentials.Config.Server)
	if err := agentctl.Serve(ctx, agentctl.ServeOptions{
		DataDir: dataDir, Snapshotter: snapshotter, Runtime: runtimeClient,
	}); err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	return ExitOK
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
		fmt.Fprintf(stdout, "%s  %s  %s/%s  assignment %d (acked %d)  last seen %s\n",
			node.GetName(), node.GetConnectionStatus().String(), node.GetOperatingSystem(), node.GetArchitecture(),
			node.GetAssignmentGeneration(), node.GetAcknowledgedGeneration(), lastSeen)
	}
	return ExitOK
}

func runApp(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "status" {
		fmt.Fprintln(stderr, "usage: orch app status NAME --context FILE [--json]")
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
	fmt.Fprintf(stdout, "Application: %s\nDesired:     %d\nObserved:    %d\nStatus:      %s\nReason:      %s\nMessage:     %s\n",
		application.GetName(), application.GetGeneration(), application.GetObservedGeneration(),
		application.GetExecutionStatus().String(), application.GetExecutionReason(), application.GetExecutionMessage())
	for _, allocation := range application.GetAllocations() {
		fmt.Fprintf(stdout, "  %s/%d  %s  restarts=%d\n", allocation.GetService(), allocation.GetReplica(), allocation.GetPhase().String(), allocation.GetRestartCount())
	}
	return ExitOK
}

func runLogs(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("logs", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var replica, tail uint
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.UintVar(&replica, "replica", 1, "replica ordinal")
	flags.UintVar(&tail, "tail", 100, "maximum lines per stream")
	reordered := reorderInterspersed(args, map[string]bool{"-context": true, "--context": true, "-replica": true, "--replica": true, "-tail": true, "--tail": true})
	if err := flags.Parse(reordered); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 2 || contextPath == "" || replica == 0 || replica > math.MaxUint32 || tail == 0 || tail > 10_000 {
		fmt.Fprintln(stderr, "logs requires APP SERVICE, --context, and bounded --replica/--tail values")
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
	stream, err := remote.TailLogs(ctx, flags.Arg(0), flags.Arg(1), uint32(replica), uint32(tail))
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
	fmt.Fprintln(output, `orch agent manages the single execution agent.

Usage:
  orch agent init --name NAME --context FILE --data-dir DIR [--json]
  orch agent serve --data-dir DIR --containerd-socket PATH --snapshotter NAME`)
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
