package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	clusterctl "change.codes/torsten/better-swarm/internal/cluster"
	"change.codes/torsten/better-swarm/internal/config"
)

const Version = "0.1.0-dev"

const (
	ExitOK      = 0
	ExitRuntime = 1
	ExitInvalid = 2
)

func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHelp(stderr)
		return ExitInvalid
	}
	switch args[0] {
	case "cluster":
		return runCluster(args[1:], stdout, stderr)
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
	ordered := []string{"--name", "--server", "--data-dir", "--context", "--output"}
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
