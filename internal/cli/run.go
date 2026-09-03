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
	"regexp"
	"strings"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	controlclient "change.codes/torsten/better-swarm/internal/client"
	clusterctl "change.codes/torsten/better-swarm/internal/cluster"
	"change.codes/torsten/better-swarm/internal/config"
	"change.codes/torsten/better-swarm/internal/control"
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
	switch args[0] {
	case "cluster":
		return runCluster(args[1:], stdout, stderr)
	case "agent":
		return runAgent(ctx, args[1:], stdout, stderr)
	case "controller":
		return runController(ctx, args[1:], stdout, stderr)
	case "nodes":
		return runNodes(ctx, args[1:], stdout, stderr)
	case "network":
		return runNetwork(ctx, args[1:], stdout, stderr)
	case "app":
		return runApp(ctx, args[1:], stdout, stderr)
	case "logs":
		return runLogs(ctx, args[1:], stdout, stderr)
	case "exec":
		return runExec(ctx, args[1:], stdout, stderr)
	case "remove":
		return runRemove(ctx, args[1:], stdout, stderr)
	case "runtime-log-sink":
		return runRuntimeLogSink(ctx, args[1:], stderr)
	case "status":
		return runStatus(ctx, args[1:], stdout, stderr)
	case "events":
		return runEvents(ctx, args[1:], stdout, stderr)
	case "deploy":
		return runDeploy(ctx, args[1:], stdout, stderr)
	case "validate":
		return runValidate(args[1:], stdout, stderr)
	case "plan":
		return runPlan(args[1:], stdout, stderr)
	case "render":
		return runRender(args[1:], stdout, stderr)
	case "schema":
		return runSchema(args[1:], stdout, stderr)
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
	var file, contextPath, idempotencyKey string
	var jsonOutput bool
	flags.StringVar(&file, "file", "orch.toml", "manifest path")
	flags.StringVar(&file, "f", "orch.toml", "manifest path")
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.StringVar(&idempotencyKey, "idempotency-key", "", "stable key for safe retries")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
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
	manifestTOML, err := readLimitedFile(file, 1<<20)
	if err != nil {
		writeError(stderr, fmt.Errorf("read manifest %q: %w", file, err), jsonOutput)
		return ExitInvalid
	}
	manifest, err := config.Parse(manifestTOML)
	if err != nil {
		writeError(stderr, fmt.Errorf("%s: %w", file, err), jsonOutput)
		return ExitInvalid
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
	response, err := remote.ApplyApplication(ctx, manifestTOML, idempotencyKey)
	if err != nil {
		writeError(stderr, fmt.Errorf("deploy request failed; retry with --idempotency-key %s: %w", idempotencyKey, err), jsonOutput)
		return ExitRuntime
	}
	digest, _ := config.Digest(manifest)
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

func readLimitedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, fmt.Errorf("manifest exceeds %d bytes", limit)
	}
	return contents, nil
}

func runCluster(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printClusterHelp(stderr)
		return ExitInvalid
	}
	switch args[0] {
	case "init":
		return runClusterInit(args[1:], stdout, stderr)
	case "backup":
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
		"--name": name, "--server": server, "--data-dir": dataDir, "--context": contextPath,
	}); len(missing) != 0 {
		writeError(stderr, fmt.Errorf("cluster init requires %s", strings.Join(missing, ", ")), jsonOutput)
		return ExitInvalid
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
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			Created   bool   `json:"created"`
			ClusterID string `json:"cluster_id"`
			Name      string `json:"name"`
			Server    string `json:"server"`
			DataDir   string `json:"data_dir"`
			Context   string `json:"context"`
		}{
			Created: result.Created, ClusterID: result.Cluster.ID, Name: result.Cluster.Name,
			Server: result.Cluster.Endpoint, DataDir: result.DataDir, Context: result.ContextPath,
		})
	}
	if result.Created {
		fmt.Fprintf(stdout, "Initialized cluster %s (%s)\n", result.Cluster.Name, result.Cluster.ID)
	} else {
		fmt.Fprintf(stdout, "Cluster %s already initialized (%s)\n", result.Cluster.Name, result.Cluster.ID)
	}
	fmt.Fprintf(stdout, "Server:  %s\nData:    %s\nContext: %s\n", result.Cluster.Endpoint, result.DataDir, result.ContextPath)
	return ExitOK
}

func runClusterBackup(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("cluster backup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var dataDir, output string
	var jsonOutput bool
	flags.StringVar(&dataDir, "data-dir", "", "controller state directory")
	flags.StringVar(&output, "output", "", "new backup database path")
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
	result, err := clusterctl.Backup(context.Background(), clusterctl.BackupOptions{DataDir: dataDir, Output: output})
	if err != nil {
		writeError(stderr, err, jsonOutput)
		if errors.Is(err, clusterctl.ErrInvalidOptions) {
			return ExitInvalid
		}
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			ClusterID string `json:"cluster_id"`
			Name      string `json:"name"`
			Output    string `json:"output"`
		}{ClusterID: result.Cluster.ID, Name: result.Cluster.Name, Output: result.Output})
	}
	fmt.Fprintf(stdout, "Backed up cluster %s (%s)\nOutput: %s\n", result.Cluster.Name, result.Cluster.ID, result.Output)
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

func runValidate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var file string
	var jsonOutput bool
	flags.StringVar(&file, "file", "orch.toml", "manifest path")
	flags.StringVar(&file, "f", "orch.toml", "manifest path")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "validate accepts flags only")
		return ExitInvalid
	}

	manifest, err := config.Load(file)
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
			Digest   string `json:"digest"`
		}{
			Valid:    true,
			Name:     manifest.Name,
			Schema:   manifest.Schema,
			Services: len(manifest.Services),
			Digest:   digest,
		})
	}
	fmt.Fprintf(stdout, "valid %s (%d services) %s\n", manifest.Name, len(manifest.Services), digest)
	return ExitOK
}

func runPlan(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var file string
	var against string
	var jsonOutput bool
	flags.StringVar(&file, "file", "orch.toml", "target manifest path")
	flags.StringVar(&file, "f", "orch.toml", "target manifest path")
	flags.StringVar(&against, "against", "", "existing manifest to compare against")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "plan accepts flags only")
		return ExitInvalid
	}

	target, err := config.Load(file)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitInvalid
	}
	var current *config.Manifest
	if against != "" {
		current, err = config.Load(against)
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
	data, err := config.SchemaJSON()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return ExitRuntime
	}
	if _, err := stdout.Write(data); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitRuntime
	}
	return ExitOK
}

func runRender(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("render", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var file string
	flags.StringVar(&file, "file", "orch.toml", "manifest path")
	flags.StringVar(&file, "f", "orch.toml", "manifest path")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "render accepts flags only")
		return ExitInvalid
	}
	manifest, err := config.Load(file)
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
  orch cluster init --name NAME --server HTTPS_URL --data-dir DIR --context FILE [--json]
  orch cluster backup --data-dir DIR --output FILE [--json]
  orch controller serve --data-dir DIR --listen ADDRESS
  orch agent init --name NAME --context FILE --data-dir DIR [--json]
  orch agent serve --data-dir DIR --containerd-socket PATH --snapshotter NAME [CNI/Caddy flags]
  orch network configure --cidr CIDR --context FILE [--idempotency-key KEY] [--json]
  orch nodes --context FILE [--json]
  orch app status NAME --context FILE [--json]
  orch logs APP SERVICE --replica N --tail N --context FILE
  orch exec APP SERVICE --replica N --context FILE -- COMMAND [ARG...]
  orch remove APP --context FILE [--idempotency-key KEY] [--json]
  orch status --context FILE [--json]
  orch events --context FILE [--after SEQUENCE] [--limit N] [--json]
  orch deploy [-f orch.toml] --context FILE [--idempotency-key KEY] [--json]
  orch validate [-f orch.toml] [--json]
  orch plan [-f orch.toml] [--against previous.toml] [--json]
  orch render [-f orch.toml]
  orch schema
  orch version

Exit codes:
  0  command succeeded
  1  operational failure
  2  usage or manifest validation failure`)
}

func printClusterHelp(output io.Writer) {
	fmt.Fprintln(output, `orch cluster manages local controller state.

Usage:
  orch cluster init --name NAME --server HTTPS_URL --data-dir DIR --context FILE [--json]
  orch cluster backup --data-dir DIR --output FILE [--json]`)
}

func printControllerHelp(output io.Writer) {
	fmt.Fprintln(output, `orch controller runs the single control-plane process.

Usage:
  orch controller serve --data-dir DIR --listen ADDRESS`)
}
