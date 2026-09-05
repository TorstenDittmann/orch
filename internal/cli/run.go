package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	controlclient "change.codes/torsten/better-swarm/internal/client"
	clusterctl "change.codes/torsten/better-swarm/internal/cluster"
	"change.codes/torsten/better-swarm/internal/config"
	"change.codes/torsten/better-swarm/internal/control"
	"change.codes/torsten/better-swarm/internal/target"
	pklschema "change.codes/torsten/better-swarm/pkl"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const Version = "0.1.0-dev"

const (
	ExitOK      = 0
	ExitRuntime = 1
	ExitInvalid = 2
)

func Run(args []string, stdout, stderr io.Writer) int {
	return RunContext(context.Background(), args, stdout, stderr)
}

func RunContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHelp(stderr)
		return ExitInvalid
	}
	prepared, err := prepareConnectionArgs(args)
	if err != nil {
		writeError(stderr, err, false)
		return ExitInvalid
	}
	args = prepared
	switch args[0] {
	case "target":
		return runTarget(ctx, args[1:], stdout, stderr)
	case "cluster":
		return runCluster(args[1:], stdout, stderr)
	case "agent":
		return runAgent(ctx, args[1:], stdout, stderr)
	case "controller":
		return runController(ctx, args[1:], stdout, stderr)
	case "nodes":
		return runNodes(ctx, args[1:], stdout, stderr)
	case "node":
		return runNode(ctx, args[1:], stdout, stderr)
	case "network":
		return runNetwork(ctx, args[1:], stdout, stderr)
	case "app":
		return runApp(ctx, args[1:], stdout, stderr)
	case "logs":
		return runLogs(ctx, args[1:], stdout, stderr)
	case "exec":
		return runExec(ctx, args[1:], stdout, stderr)
	case "job":
		return runJob(ctx, args[1:], stdout, stderr)
	case "cron":
		return runCron(ctx, args[1:], stdout, stderr)
	case "secret":
		return runSecret(ctx, args[1:], stdout, stderr)
	case "registry":
		return runRegistry(ctx, args[1:], stdout, stderr)
	case "remove":
		return runRemove(ctx, args[1:], stdout, stderr)
	case "runtime-log-sink":
		return runRuntimeLogSink(ctx, args[1:], stderr)
	case "runtime-secret-init":
		return runRuntimeSecretInit(args[1:], stderr)
	case "status":
		return runStatus(ctx, args[1:], stdout, stderr)
	case "events":
		return runEvents(ctx, args[1:], stdout, stderr)
	case "rollback":
		return runRollback(ctx, args[1:], stdout, stderr)
	case "deploy":
		return runDeploy(ctx, args[1:], stdout, stderr)
	case "tui":
		return runTUI(ctx, args[1:], stdout, stderr)
	case "validate":
		return runValidate(ctx, args[1:], stdout, stderr)
	case "plan":
		return runPlan(ctx, args[1:], stdout, stderr)
	case "render":
		return runRender(ctx, args[1:], stdout, stderr)
	case "schema":
		return runSchema(args[1:], stdout, stderr)
	case "pkl":
		return runPkl(args[1:], stdout, stderr)
	case "version":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "version accepts no arguments")
			return ExitInvalid
		}
		fmt.Fprintln(stdout, Version)
		return ExitOK
	case "help", "-h", "--help":
		printHelp(stdout)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		printHelp(stderr)
		return ExitInvalid
	}
}

func runController(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printControllerHelp(stderr)
		return ExitInvalid
	}
	switch args[0] {
	case "serve":
		return runControllerServe(ctx, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printControllerHelp(stdout)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "unknown controller command %q\n\n", args[0])
		printControllerHelp(stderr)
		return ExitInvalid
	}
}

func runControllerServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("controller serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var dataDir, listen string
	flags.StringVar(&dataDir, "data-dir", "", "initialized controller state directory")
	flags.StringVar(&listen, "listen", "", "TCP listen address")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "controller serve accepts flags only")
		return ExitInvalid
	}
	if missing := missingFlags(map[string]string{"--data-dir": dataDir, "--listen": listen}); len(missing) != 0 {
		writeError(stderr, fmt.Errorf("controller serve requires %s", strings.Join(missing, ", ")), false)
		return ExitInvalid
	}
	err := control.Serve(ctx, control.Options{DataDir: dataDir, Listen: listen}, func(address net.Addr) {
		fmt.Fprintf(stdout, "Controller listening on https://%s\n", address.String())
	})
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	return ExitOK
}

func runStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "status accepts flags only")
		return ExitInvalid
	}
	if strings.TrimSpace(contextPath) == "" {
		writeError(stderr, errors.New("status requires --context"), jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	cluster, err := remote.GetCluster(ctx)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	createdAt, err := protobufTime(cluster.GetCreatedAt())
	if err != nil {
		writeError(stderr, fmt.Errorf("controller returned invalid cluster creation time: %w", err), jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			ClusterID         string    `json:"cluster_id"`
			Name              string    `json:"name"`
			Server            string    `json:"server"`
			Generation        uint64    `json:"generation"`
			CreatedAt         time.Time `json:"created_at"`
			WorkloadCIDR      string    `json:"workload_cidr"`
			NetworkGeneration uint64    `json:"network_generation"`
			NetworkFrozen     bool      `json:"network_frozen"`
		}{
			ClusterID: cluster.GetId(), Name: cluster.GetName(), Server: cluster.GetEndpoint(),
			Generation: cluster.GetGeneration(), CreatedAt: createdAt, WorkloadCIDR: cluster.GetWorkloadCidr(),
			NetworkGeneration: cluster.GetNetworkGeneration(), NetworkFrozen: cluster.GetNetworkFrozen(),
		})
	}
	fmt.Fprintf(stdout, "Cluster:    %s (%s)\nServer:     %s\nGeneration: %d\nNetwork:    %s (generation %d, frozen %t)\nCreated:    %s\n",
		cluster.GetName(), cluster.GetId(), cluster.GetEndpoint(), cluster.GetGeneration(), cluster.GetWorkloadCidr(),
		cluster.GetNetworkGeneration(), cluster.GetNetworkFrozen(), createdAt.Format(time.RFC3339Nano))
	return ExitOK
}

func runEvents(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("events", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var after uint64
	var limit uint
	var jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.Uint64Var(&after, "after", 0, "exclusive event sequence cursor")
	flags.UintVar(&limit, "limit", 50, "maximum events to return (1-200)")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "events accepts flags only")
		return ExitInvalid
	}
	if strings.TrimSpace(contextPath) == "" || limit < 1 || limit > 200 {
		writeError(stderr, errors.New("events requires --context and --limit between 1 and 200"), jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	response, err := remote.ListEvents(ctx, after, uint32(limit))
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	type eventOutput struct {
		Sequence  uint64          `json:"sequence"`
		Kind      string          `json:"kind"`
		Payload   json.RawMessage `json:"payload"`
		CreatedAt time.Time       `json:"created_at"`
	}
	events := make([]eventOutput, 0, len(response.GetEvents()))
	for _, event := range response.GetEvents() {
		createdAt, err := protobufTime(event.GetCreatedAt())
		if err != nil || event.GetSequence() == 0 || !json.Valid([]byte(event.GetPayloadJson())) {
			writeError(stderr, errors.New("controller returned an invalid event"), jsonOutput)
			return ExitRuntime
		}
		events = append(events, eventOutput{
			Sequence: event.GetSequence(), Kind: event.GetKind(),
			Payload: json.RawMessage(event.GetPayloadJson()), CreatedAt: createdAt,
		})
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			Events            []eventOutput `json:"events"`
			NextAfterSequence uint64        `json:"next_after_sequence"`
		}{Events: events, NextAfterSequence: response.GetNextAfterSequence()})
	}
	for _, event := range events {
		fmt.Fprintf(stdout, "%d  %s  %s  %s\n", event.Sequence,
			event.CreatedAt.Format(time.RFC3339Nano), event.Kind, event.Payload)
	}
	return ExitOK
}

var cliIdempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$`)

func runDeploy(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("deploy", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var file, root, contextPath, idempotencyKey string
	var jsonOutput, wait bool
	var timeout time.Duration
	flags.StringVar(&file, "file", "orch.pkl", "Pkl manifest path")
	flags.StringVar(&file, "f", "orch.pkl", "Pkl manifest path")
	flags.StringVar(&root, "root", "", "Pkl evaluation root (defaults to the manifest directory)")
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.StringVar(&idempotencyKey, "idempotency-key", "", "stable key for safe retries")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	flags.BoolVar(&wait, "wait", false, "wait for this revision to become ready")
	flags.DurationVar(&timeout, "timeout", 10*time.Minute, "rollout wait deadline")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if timeout <= 0 {
		writeError(stderr, errors.New("timeout must be positive"), jsonOutput)
		return ExitInvalid
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "deploy accepts flags only")
		return ExitInvalid
	}
	if strings.TrimSpace(contextPath) == "" {
		writeError(stderr, errors.New("deploy requires --context"), jsonOutput)
		return ExitInvalid
	}
	manifest, err := config.Load(ctx, file, config.LoadOptions{RootDir: root})
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitInvalid
	}
	canonical, err := config.CanonicalJSON(manifest)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if idempotencyKey == "" {
		idempotencyKey, err = newIdempotencyKey(rand.Reader)
		if err != nil {
			writeError(stderr, errors.New("generate idempotency key"), jsonOutput)
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
	response, err := remote.ApplyApplication(ctx, canonical, idempotencyKey)
	if err != nil {
		writeError(stderr, fmt.Errorf("deploy request failed; retry with --idempotency-key %s: %w", idempotencyKey, err), jsonOutput)
		return ExitRuntime
	}
	digest := config.DigestCanonicalJSON(canonical)
	if response.GetName() != manifest.Name || response.GetDigest() != digest || response.GetGeneration() == 0 || response.GetRevisionId() == "" || response.GetExecutionStatus() != orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_PENDING {
		writeError(stderr, errors.New("controller returned inconsistent application state"), jsonOutput)
		return ExitRuntime
	}
	createdAt, err := protobufTime(response.GetCreatedAt())
	if err != nil {
		writeError(stderr, errors.New("controller returned an invalid application creation time"), jsonOutput)
		return ExitRuntime
	}
	updatedAt, err := protobufTime(response.GetUpdatedAt())
	if err != nil {
		writeError(stderr, errors.New("controller returned an invalid application update time"), jsonOutput)
		return ExitRuntime
	}
	if wait {
		return waitForDeployment(ctx, remote, response.GetName(), response.GetGeneration(), response.GetRevisionId(), idempotencyKey, timeout, jsonOutput, stdout, stderr)
	}
	const message = "desired state accepted; execution is asynchronous; use orch app status to observe it"
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			Application    string    `json:"application"`
			Generation     uint64    `json:"generation"`
			RevisionID     string    `json:"revision_id"`
			Digest         string    `json:"digest"`
			Changed        bool      `json:"changed"`
			EventSequence  uint64    `json:"event_sequence"`
			Status         string    `json:"status"`
			IdempotencyKey string    `json:"idempotency_key"`
			CreatedAt      time.Time `json:"created_at"`
			UpdatedAt      time.Time `json:"updated_at"`
			Message        string    `json:"message"`
		}{
			Application: response.GetName(), Generation: response.GetGeneration(),
			RevisionID: response.GetRevisionId(), Digest: response.GetDigest(), Changed: response.GetChanged(),
			EventSequence: response.GetEventSequence(), Status: "PENDING", IdempotencyKey: idempotencyKey,
			CreatedAt: createdAt, UpdatedAt: updatedAt, Message: message,
		})
	}
	if response.GetChanged() {
		fmt.Fprintf(stdout, "Accepted desired state for %s (generation %d, %s).\n", response.GetName(), response.GetGeneration(), response.GetDigest())
	} else {
		fmt.Fprintf(stdout, "Desired state for %s is already current (generation %d, %s).\n", response.GetName(), response.GetGeneration(), response.GetDigest())
	}
	fmt.Fprintln(stdout, "Status: PENDING — accepted for asynchronous execution; use orch app status to observe it.")
	fmt.Fprintf(stdout, "Idempotency key: %s\n", idempotencyKey)
	return ExitOK
}

func protobufTime(value *timestamppb.Timestamp) (time.Time, error) {
	if value == nil {
		return time.Time{}, errors.New("timestamp is missing")
	}
	if err := value.CheckValid(); err != nil {
		return time.Time{}, err
	}
	return value.AsTime(), nil
}

func newIdempotencyKey(random io.Reader) (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(random, value); err != nil {
		return "", err
	}
	return "idem_" + hex.EncodeToString(value), nil
}

func runCluster(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printClusterHelp(stderr)
		return ExitInvalid
	}
	switch args[0] {
	case "init":
		return runClusterInit(args[1:], stdout, stderr)
	case "restore":
		return runClusterRecovery(args[1:], true, stdout, stderr)
	case "backup":
		if len(args) > 1 && args[1] == "inspect" {
			return runClusterRecovery(args[2:], false, stdout, stderr)
		}
		return runClusterBackup(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printClusterHelp(stdout)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "unknown cluster command %q\n\n", args[0])
		printClusterHelp(stderr)
		return ExitInvalid
	}
}

func runClusterInit(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("cluster init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var name, server, dataDir, contextPath string
	var jsonOutput bool
	flags.StringVar(&name, "name", "", "cluster name")
	flags.StringVar(&server, "server", "", "controller HTTPS URL")
	flags.StringVar(&dataDir, "data-dir", "", "controller state directory")
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "cluster init accepts flags only")
		return ExitInvalid
	}
	if missing := missingFlags(map[string]string{
		"--name": name, "--server": server, "--data-dir": dataDir,
	}); len(missing) != 0 {
		writeError(stderr, fmt.Errorf("cluster init requires %s", strings.Join(missing, ", ")), jsonOutput)
		return ExitInvalid
	}
	managedTarget := contextPath == ""
	var targetStore *target.Store
	var stagingDirectory string
	targetExists := false
	if managedTarget {
		var err error
		targetStore, err = target.Default()
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		if existing, err := targetStore.Get(name); err == nil {
			contextPath = existing.ContextPath
			targetExists = true
		} else if !errors.Is(err, target.ErrNotFound) {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		if !targetExists {
			stagingDirectory, err = os.MkdirTemp("", "orch-cluster-init-")
			if err != nil {
				writeError(stderr, fmt.Errorf("stage administrator identity: %w", err), jsonOutput)
				return ExitRuntime
			}
			defer os.RemoveAll(stagingDirectory)
			if err := os.Chmod(stagingDirectory, 0o700); err != nil {
				writeError(stderr, fmt.Errorf("protect staged administrator identity: %w", err), jsonOutput)
				return ExitRuntime
			}
			contextPath = filepath.Join(stagingDirectory, "context.json")
		}
	}
	result, err := clusterctl.Bootstrap(context.Background(), clusterctl.BootstrapOptions{
		Name: name, Server: server, DataDir: dataDir, ContextPath: contextPath,
	})
	if err != nil {
		writeError(stderr, err, jsonOutput)
		if errors.Is(err, clusterctl.ErrInvalidOptions) {
			return ExitInvalid
		}
		return ExitRuntime
	}
	if managedTarget {
		if !result.Created && !targetExists {
			writeError(stderr, errors.New("the cluster already exists; pass its administrator context once with --context, then import it with 'orch target import'"), jsonOutput)
			return ExitInvalid
		}
		if targetExists {
			local, loadErr := clusterctl.LoadContext(contextPath)
			if loadErr != nil || local.Cluster.ID != result.Cluster.ID {
				writeError(stderr, fmt.Errorf("target %q does not belong to controller state %s", name, result.DataDir), jsonOutput)
				return ExitInvalid
			}
		} else {
			if _, err := targetStore.Import(name, contextPath); err != nil {
				writeError(stderr, fmt.Errorf("install local target: %w", err), jsonOutput)
				return ExitRuntime
			}
		}
		if err := targetStore.Use(name); err != nil {
			writeError(stderr, fmt.Errorf("select local target: %w", err), jsonOutput)
			return ExitRuntime
		}
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			Created   bool   `json:"created"`
			ClusterID string `json:"cluster_id"`
			Name      string `json:"name"`
			Server    string `json:"server"`
			DataDir   string `json:"data_dir"`
			Context   string `json:"context,omitempty"`
			Target    string `json:"target,omitempty"`
		}{
			Created: result.Created, ClusterID: result.Cluster.ID, Name: result.Cluster.Name,
			Server: result.Cluster.Endpoint, DataDir: result.DataDir,
			Context: func() string {
				if managedTarget {
					return ""
				}
				return result.ContextPath
			}(),
			Target: func() string {
				if managedTarget {
					return name
				}
				return ""
			}(),
		})
	}
	if result.Created {
		fmt.Fprintf(stdout, "Initialized cluster %s (%s)\n", result.Cluster.Name, result.Cluster.ID)
	} else {
		fmt.Fprintf(stdout, "Cluster %s already initialized (%s)\n", result.Cluster.Name, result.Cluster.ID)
	}
	fmt.Fprintf(stdout, "Server:  %s\nData:    %s\n", result.Cluster.Endpoint, result.DataDir)
	if managedTarget {
		fmt.Fprintf(stdout, "Target:  %s (selected)\n", name)
	} else {
		fmt.Fprintf(stdout, "Context: %s\n", result.ContextPath)
	}
	return ExitOK
}

func runClusterBackup(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("cluster backup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var dataDir, output, secretKeyOutput string
	var jsonOutput bool
	flags.StringVar(&dataDir, "data-dir", "", "controller state directory")
	flags.StringVar(&output, "output", "", "new backup database path")
	flags.StringVar(&secretKeyOutput, "secret-key-output", "", "new backup path for the secret encryption key when required")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "cluster backup accepts flags only")
		return ExitInvalid
	}
	if missing := missingFlags(map[string]string{"--data-dir": dataDir, "--output": output}); len(missing) != 0 {
		writeError(stderr, fmt.Errorf("cluster backup requires %s", strings.Join(missing, ", ")), jsonOutput)
		return ExitInvalid
	}
	result, err := clusterctl.Backup(context.Background(), clusterctl.BackupOptions{DataDir: dataDir, Output: output, SecretKeyOutput: secretKeyOutput})
	if err != nil {
		writeError(stderr, err, jsonOutput)
		if errors.Is(err, clusterctl.ErrInvalidOptions) {
			return ExitInvalid
		}
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			ClusterID       string `json:"cluster_id"`
			Name            string `json:"name"`
			Output          string `json:"output"`
			SecretKeyOutput string `json:"secret_key_output,omitempty"`
		}{ClusterID: result.Cluster.ID, Name: result.Cluster.Name, Output: result.Output, SecretKeyOutput: result.SecretKeyOutput})
	}
	fmt.Fprintf(stdout, "Backed up cluster %s (%s)\nOutput: %s\n", result.Cluster.Name, result.Cluster.ID, result.Output)
	if result.SecretKeyOutput != "" {
		fmt.Fprintf(stdout, "Secret key: %s\n", result.SecretKeyOutput)
	}
	return ExitOK
}

func missingFlags(values map[string]string) []string {
	ordered := []string{"--name", "--server", "--data-dir", "--context", "--output", "--listen"}
	missing := make([]string, 0)
	for _, name := range ordered {
		if value, present := values[name]; present && strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	return missing
}

func runValidate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var file, root string
	var jsonOutput bool
	flags.StringVar(&file, "file", "orch.pkl", "Pkl manifest path")
	flags.StringVar(&file, "f", "orch.pkl", "Pkl manifest path")
	flags.StringVar(&root, "root", "", "Pkl evaluation root (defaults to the manifest directory)")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "validate accepts flags only")
		return ExitInvalid
	}

	manifest, err := config.Load(ctx, file, config.LoadOptions{RootDir: root})
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitInvalid
	}
	digest, err := config.Digest(manifest)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			Valid    bool   `json:"valid"`
			Name     string `json:"name"`
			Schema   string `json:"schema"`
			Services int    `json:"services"`
			CronJobs int    `json:"cron_jobs"`
			Digest   string `json:"digest"`
		}{
			Valid:    true,
			Name:     manifest.Name,
			Schema:   manifest.Schema,
			Services: len(manifest.Services),
			CronJobs: len(manifest.CronJobs),
			Digest:   digest,
		})
	}
	fmt.Fprintf(stdout, "valid %s (%d services, %d cron jobs) %s\n", manifest.Name, len(manifest.Services), len(manifest.CronJobs), digest)
	return ExitOK
}

func runPlan(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var file, root, against string
	var jsonOutput bool
	flags.StringVar(&file, "file", "orch.pkl", "target Pkl manifest path")
	flags.StringVar(&file, "f", "orch.pkl", "target Pkl manifest path")
	flags.StringVar(&root, "root", "", "Pkl evaluation root (defaults to each manifest directory)")
	flags.StringVar(&against, "against", "", "existing Pkl manifest to compare against")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "plan accepts flags only")
		return ExitInvalid
	}

	target, err := config.Load(ctx, file, config.LoadOptions{RootDir: root})
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitInvalid
	}
	var current *config.Manifest
	if against != "" {
		current, err = config.Load(ctx, against, config.LoadOptions{RootDir: root})
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitInvalid
		}
		if current.Name != target.Name {
			writeError(stderr, fmt.Errorf("cannot compare applications %q and %q", current.Name, target.Name), jsonOutput)
			return ExitInvalid
		}
	}
	changes, err := config.Diff(current, target)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	digest, err := config.Digest(target)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}

	summary := summarize(changes)
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			Application string          `json:"application"`
			Digest      string          `json:"digest"`
			Changes     []config.Change `json:"changes"`
			Summary     planSummary     `json:"summary"`
		}{
			Application: target.Name,
			Digest:      digest,
			Changes:     changes,
			Summary:     summary,
		})
	}
	fmt.Fprintf(stdout, "Plan for %s (%s)\n", target.Name, digest)
	if len(changes) == 0 {
		fmt.Fprintln(stdout, "No changes.")
		return ExitOK
	}
	for _, change := range changes {
		fmt.Fprintln(stdout, config.FormatChange(change))
	}
	fmt.Fprintf(stdout, "\nPlan: %d to add, %d to change, %d to remove.\n", summary.Add, summary.Modify, summary.Remove)
	return ExitOK
}

func runSchema(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("schema", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "schema accepts no arguments")
		return ExitInvalid
	}
	data := pklschema.ApplicationSource()
	if _, err := stdout.Write(data); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitRuntime
	}
	return ExitOK
}

func runRender(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("render", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var file, root string
	flags.StringVar(&file, "file", "orch.pkl", "Pkl manifest path")
	flags.StringVar(&file, "f", "orch.pkl", "Pkl manifest path")
	flags.StringVar(&root, "root", "", "Pkl evaluation root (defaults to the manifest directory)")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "render accepts flags only")
		return ExitInvalid
	}
	manifest, err := config.Load(ctx, file, config.LoadOptions{RootDir: root})
	if err != nil {
		writeError(stderr, err, false)
		return ExitInvalid
	}
	data, err := config.CanonicalJSON(manifest)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	if _, err := stdout.Write(data); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitRuntime
	}
	return ExitOK
}

func runPkl(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "reader" {
		fmt.Fprintln(stderr, "usage: orch pkl reader")
		return ExitInvalid
	}
	if err := pklschema.RunExternalReader(os.Stdin, stdout); err != nil {
		fmt.Fprintf(stderr, "run Pkl schema reader: %v\n", err)
		return ExitRuntime
	}
	return ExitOK
}

type planSummary struct {
	Add    int `json:"add"`
	Modify int `json:"modify"`
	Remove int `json:"remove"`
}

func summarize(changes []config.Change) planSummary {
	var summary planSummary
	for _, change := range changes {
		switch change.Kind {
		case config.ChangeAdd:
			summary.Add++
		case config.ChangeModify:
			summary.Modify++
		case config.ChangeRemove:
			summary.Remove++
		}
	}
	return summary
}

func writeJSON(stdout, stderr io.Writer, value any) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitRuntime
	}
	return ExitOK
}

func writeError(output io.Writer, err error, jsonOutput bool) {
	if !jsonOutput {
		fmt.Fprintln(output, err)
		return
	}
	type errorOutput struct {
		Error    string           `json:"error"`
		Problems []config.Problem `json:"problems,omitempty"`
	}
	payload := errorOutput{Error: err.Error()}
	var validation *config.ValidationError
	if errors.As(err, &validation) {
		payload.Problems = validation.Problems
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(payload)
}

func printHelp(output io.Writer) {
	fmt.Fprintln(output, `orch manages small application clusters.

Usage:
  orch cluster init --name NAME --server HTTPS_URL --data-dir DIR [--context FILE] [--json]
  orch cluster backup --data-dir DIR --output FILE [--secret-key-output FILE] [--json]
  orch cluster backup inspect --backup FILE [--pki-dir DIR] [--secret-key FILE] [--json]
  orch cluster restore --backup FILE --pki-dir DIR [--secret-key FILE] --data-dir NEW_DIR [--json]
  orch target import NAME CONTEXT [--use]
  orch target use NAME
  orch target transport NAME direct|wireguard|ssh [OPTIONS]
  orch target test [NAME]
  orch tui [--target NAME] [--refresh 2s]
  orch controller serve --data-dir DIR --listen ADDRESS
  orch agent init --join-file FILE|- --data-dir DIR [--json]
  orch agent serve --data-dir DIR --containerd-socket PATH --snapshotter NAME [CNI/Caddy flags]
  orch network configure --cidr CIDR [--target NAME] [--idempotency-key KEY] [--json]
  orch nodes [--target NAME] [--json]
  orch node label|unlabel|cordon|uncordon|drain|ingress|remove NODE [--target NAME]
  orch node join create NODE --ttl 15m --output FILE [--target NAME]
  orch node join list|revoke [--target NAME]
  orch app status NAME [--target NAME] [--json]
  orch app history APP [--limit N] [--before-generation N] [--target NAME] [--json]
  orch app diff APP REVISION [--against REVISION] [--target NAME] [--json]
  orch app watch APP [--revision REVISION] [--timeout 10m] [--target NAME] [--json]
  orch rollback APP REVISION [--expected-generation N] [--wait] [--timeout 10m] [--target NAME] [--idempotency-key KEY] [--json]
  orch logs APP SERVICE --replica N --tail N [--target NAME]
  orch exec APP SERVICE --replica N [--target NAME] -- COMMAND [ARG...]
  orch job run APP SERVICE [--target NAME] [--wait] -- COMMAND [ARG...]
  orch job list [APP] [--target NAME] [--limit N] [--before ID] [--json]
  orch job show|logs|cancel|retry|wait ID [--target NAME]
  orch cron runs APP JOB [--target NAME] [--limit N] [--json]
  orch cron logs APP JOB RUN_ID [--target NAME] [--tail N]
  orch secret set APP NAME --from-stdin [--target NAME] [--json]
  orch secret list APP [--target NAME] [--json]
  orch secret delete APP NAME [--target NAME] [--json]
  orch registry login HOST --username USER --password-stdin [--target NAME] [--json]
  orch registry list [--target NAME] [--json]
  orch registry logout HOST [--target NAME] [--json]
  orch remove APP [--target NAME] [--idempotency-key KEY] [--json]
  orch status [--target NAME] [--json]
  orch events [--target NAME] [--after SEQUENCE] [--limit N] [--json]
  orch deploy [-f orch.pkl] [--root DIR] [--wait] [--timeout 10m] [--target NAME] [--idempotency-key KEY] [--json]
  orch validate [-f orch.pkl] [--root DIR] [--json]
  orch plan [-f orch.pkl] [--root DIR] [--against previous.pkl] [--json]
  orch render [-f orch.pkl] [--root DIR]
  orch schema
  orch pkl reader
  orch version

Exit codes:
  0  command succeeded
  1  operational failure
  2  usage or manifest validation failure

Remote commands use the selected target. --target overrides it for one command;
--context FILE remains available for compatibility.`)
}

func printClusterHelp(output io.Writer) {
	fmt.Fprintln(output, `orch cluster manages local controller state.

Usage:
  orch cluster init --name NAME --server HTTPS_URL --data-dir DIR [--context FILE] [--json]
  orch cluster backup --data-dir DIR --output FILE [--secret-key-output FILE] [--json]
  orch cluster backup inspect --backup FILE [--pki-dir DIR] [--secret-key FILE] [--json]
  orch cluster restore --backup FILE --pki-dir DIR [--secret-key FILE] --data-dir NEW_DIR [--json]`)
}

func printControllerHelp(output io.Writer) {
	fmt.Fprintln(output, `orch controller runs the single control-plane process.

Usage:
  orch controller serve --data-dir DIR --listen ADDRESS`)
}
