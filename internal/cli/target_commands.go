package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	controlclient "change.codes/torsten/better-swarm/internal/client"
	"change.codes/torsten/better-swarm/internal/target"
)

func runTarget(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printTargetHelp(stderr)
		return ExitInvalid
	}
	switch args[0] {
	case "list":
		return runTargetList(args[1:], stdout, stderr)
	case "use":
		return runTargetUse(args[1:], stdout, stderr)
	case "import":
		return runTargetImport(args[1:], stdout, stderr)
	case "transport":
		return runTargetTransport(args[1:], stdout, stderr)
	case "test":
		return runTargetTest(ctx, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printTargetHelp(stdout)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "unknown target command %q\n\n", args[0])
		printTargetHelp(stderr)
		return ExitInvalid
	}
}

func runTargetList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("target list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var jsonOutput bool
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return ExitInvalid
	}
	store, err := target.Default()
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	targets, err := store.List()
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	current, _ := store.Current()
	type item struct {
		Name      string `json:"name"`
		Current   bool   `json:"current"`
		Transport string `json:"transport"`
	}
	items := make([]item, 0, len(targets))
	for _, candidate := range targets {
		items = append(items, item{Name: candidate.Name, Current: candidate.Name == current.Name, Transport: candidate.Transport.Type})
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			Targets []item `json:"targets"`
		}{Targets: items})
	}
	if len(items) == 0 {
		fmt.Fprintln(stdout, "No targets configured. Import one with: orch target import NAME CONTEXT")
		return ExitOK
	}
	for _, item := range items {
		marker := " "
		if item.Current {
			marker = "*"
		}
		fmt.Fprintf(stdout, "%s %-24s %s\n", marker, item.Name, item.Transport)
	}
	return ExitOK
}

func runTargetUse(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: orch target use NAME")
		return ExitInvalid
	}
	store, err := target.Default()
	if err == nil {
		err = store.Use(args[0])
	}
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	fmt.Fprintf(stdout, "Using target %s\n", args[0])
	return ExitOK
}

func runTargetImport(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("target import", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var use, jsonOutput bool
	flags.BoolVar(&use, "use", false, "select the imported target")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	reordered := reorderInterspersed(args, map[string]bool{})
	if err := flags.Parse(reordered); err != nil || flags.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: orch target import NAME CONTEXT [--use] [--json]")
		return ExitInvalid
	}
	name, source := flags.Arg(0), flags.Arg(1)
	store, err := target.Default()
	var resolved target.Resolved
	if err == nil {
		resolved, err = store.Import(name, source)
	}
	if err == nil && use {
		err = store.Use(name)
	}
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			Name      string `json:"name"`
			Transport string `json:"transport"`
			Current   bool   `json:"current"`
		}{Name: resolved.Name, Transport: resolved.Transport.Type, Current: use})
	}
	fmt.Fprintf(stdout, "Imported target %s (%s)\n", resolved.Name, resolved.Transport.Type)
	if !use {
		fmt.Fprintf(stdout, "Select it with: orch target use %s\n", resolved.Name)
	}
	return ExitOK
}

func runTargetTransport(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: orch target transport NAME direct|wireguard|ssh [OPTIONS]")
		return ExitInvalid
	}
	name, mode := args[0], args[1]
	flags := flag.NewFlagSet("target transport", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var address, host, destination string
	flags.StringVar(&address, "address", "", "WireGuard-reachable controller host:port")
	flags.StringVar(&host, "host", "", "OpenSSH host or alias")
	flags.StringVar(&destination, "destination", "", "controller host:port from the SSH host")
	if err := flags.Parse(args[2:]); err != nil || flags.NArg() != 0 {
		return ExitInvalid
	}
	transport := target.Transport{Type: mode, Address: address, SSHHost: host, SSHDestination: destination}
	store, err := target.Default()
	if err == nil {
		err = store.SetTransport(name, transport)
	}
	if err != nil {
		writeError(stderr, err, false)
		return ExitInvalid
	}
	fmt.Fprintf(stdout, "Target %s now uses %s transport\n", name, mode)
	return ExitOK
}

func runTargetTest(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("target test", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var jsonOutput bool
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	reordered := reorderInterspersed(args, map[string]bool{})
	if err := flags.Parse(reordered); err != nil || flags.NArg() > 1 {
		fmt.Fprintln(stderr, "usage: orch target test [NAME] [--json]")
		return ExitInvalid
	}
	store, err := target.Default()
	var resolved target.Resolved
	if err == nil && flags.NArg() == 1 {
		resolved, err = store.Get(flags.Arg(0))
	} else if err == nil {
		resolved, err = store.Current()
	}
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	reference, _ := target.Reference(resolved.Name)
	remote, err := controlclient.New(reference)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	testCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cluster, err := remote.GetCluster(testCtx)
	if err != nil {
		writeError(stderr, fmt.Errorf("target %s (%s): %w", resolved.Name, resolved.Transport.Type, err), jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			Name      string `json:"name"`
			Transport string `json:"transport"`
			Cluster   string `json:"cluster"`
		}{Name: resolved.Name, Transport: resolved.Transport.Type, Cluster: cluster.GetName()})
	}
	fmt.Fprintf(stdout, "Connected to %s through target %s (%s)\n", cluster.GetName(), resolved.Name, resolved.Transport.Type)
	return ExitOK
}

func prepareConnectionArgs(args []string) ([]string, error) {
	if len(args) == 0 || !isRemoteCommand(args) {
		return args, nil
	}
	for _, argument := range args[1:] {
		if argument == "--" {
			break
		}
		if argument == "help" || argument == "-h" || argument == "--help" {
			return args, nil
		}
	}
	cleaned := make([]string, 0, len(args)+2)
	var explicitTarget string
	hasContext := false
	for i := 0; i < len(args); i++ {
		argument := args[i]
		if argument == "--" {
			cleaned = append(cleaned, args[i:]...)
			break
		}
		switch {
		case argument == "--target" || argument == "-target":
			if i+1 >= len(args) || explicitTarget != "" {
				return nil, fmt.Errorf("--target requires exactly one name")
			}
			explicitTarget = args[i+1]
			i++
			continue
		case strings.HasPrefix(argument, "--target="):
			if explicitTarget != "" {
				return nil, fmt.Errorf("--target may only be provided once")
			}
			explicitTarget = strings.TrimPrefix(argument, "--target=")
			continue
		case argument == "--context" || argument == "-context" || strings.HasPrefix(argument, "--context=") || strings.HasPrefix(argument, "-context="):
			hasContext = true
		}
		cleaned = append(cleaned, argument)
	}
	if hasContext {
		if explicitTarget != "" {
			return nil, fmt.Errorf("--context and --target are mutually exclusive")
		}
		return cleaned, nil
	}
	name := explicitTarget
	if name == "" {
		name = os.Getenv("ORCH_TARGET")
	}
	store, err := target.Default()
	if err != nil {
		return nil, err
	}
	if name == "" {
		resolved, currentErr := store.Current()
		if currentErr != nil {
			return nil, currentErr
		}
		name = resolved.Name
	} else if _, err := store.Get(name); err != nil {
		return nil, err
	}
	reference, err := target.Reference(name)
	if err != nil {
		return nil, err
	}
	insert := len(cleaned)
	for i, argument := range cleaned {
		if argument == "--" {
			insert = i
			break
		}
	}
	cleaned = append(cleaned, "", "")
	copy(cleaned[insert+2:], cleaned[insert:len(cleaned)-2])
	cleaned[insert], cleaned[insert+1] = "--context", reference
	return cleaned, nil
}

func isRemoteCommand(args []string) bool {
	switch args[0] {
	case "status", "events", "deploy", "nodes", "logs", "exec", "remove", "secret", "registry", "tui":
		return true
	case "app":
		return len(args) > 1 && args[1] == "status"
	case "cron":
		return len(args) > 1 && (args[1] == "runs" || args[1] == "logs")
	case "network":
		return len(args) > 1 && args[1] == "configure"
	case "node":
		return len(args) > 1 && args[1] != "help" && args[1] != "-h" && args[1] != "--help"
	default:
		return false
	}
}

func printTargetHelp(output io.Writer) {
	fmt.Fprintln(output, `orch target manages remembered local cluster connections.

Usage:
  orch target list [--json]
  orch target import NAME CONTEXT [--use] [--json]
  orch target use NAME
  orch target transport NAME direct
  orch target transport NAME wireguard --address HOST:PORT
  orch target transport NAME ssh --host SSH_ALIAS [--destination HOST:PORT]
  orch target test [NAME] [--json]`)
}
