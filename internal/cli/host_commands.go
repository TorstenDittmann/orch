package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"change.codes/torsten/better-swarm/internal/host"
)

func runNodeHost(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	command := args[0]
	flags := flag.NewFlagSet("node "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var address, joinFile string
	var jsonOutput, dryRun bool
	var timeout time.Duration
	flags.StringVar(&address, "advertise-address", "", "private IPv4 address assigned to this host")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable results")
	if command == "install" {
		flags.StringVar(&joinFile, "join-file", "", "private join artifact path, or - for stdin")
		flags.BoolVar(&dryRun, "dry-run", false, "inspect and show planned actions without modifying the host or reading join credentials")
		flags.DurationVar(&timeout, "wait-timeout", 5*time.Minute, "maximum wait for authenticated agent readiness")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 || address == "" || command == "install" && timeout <= 0 {
		writeError(stderr, errors.New("requires --advertise-address PRIVATE_IP and a positive --wait-timeout; runs locally without --context or --target"), jsonOutput)
		return ExitInvalid
	}
	if command == "doctor" {
		report := host.Doctor(ctx, address)
		if jsonOutput {
			if code := writeJSON(stdout, stderr, report); code != ExitOK {
				return code
			}
		} else {
			printHostReport(stdout, report)
		}
		if !report.Ready {
			return ExitRuntime
		}
		return ExitOK
	}
	if dryRun {
		plan, err := host.BuildPlan(ctx, address)
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitInvalid
		}
		if jsonOutput {
			if code := writeJSON(stdout, stderr, plan); code != ExitOK {
				return code
			}
		} else {
			printHostReport(stdout, plan.Report)
			fmt.Fprintf(stdout, "\nPackages: %v\n", plan.Packages)
			for _, artifact := range plan.Artifacts {
				fmt.Fprintf(stdout, "%s: %s\n  SHA256 %s\n", artifact.Name, artifact.URL, artifact.SHA256)
			}
			for _, file := range plan.Files {
				fmt.Fprintln(stdout, "Own:", file)
			}
			for _, action := range plan.Actions {
				fmt.Fprintln(stdout, "Action:", action)
			}
		}
		if !plan.Report.Installable {
			return ExitRuntime
		}
		return ExitOK
	}
	result, err := host.Install(ctx, host.InstallOptions{Address: address, JoinFile: joinFile, JoinInput: os.Stdin, WaitTimeout: timeout, Progress: stderr})
	if jsonOutput {
		if code := writeJSON(stdout, stderr, result); code != ExitOK {
			return code
		}
	} else if result.NodeID != "" {
		fmt.Fprintf(stdout, "Node %s: ready=%t changed=%t state=%s\n", result.NodeID, result.Ready, result.Changed, result.DataDir)
	}
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	return ExitOK
}
func printHostReport(w io.Writer, r host.Report) {
	fmt.Fprintf(w, "Local node: %s %s %s (ready=%t installable=%t)\n", r.OS, r.Version, r.Architecture, r.Ready, r.Installable)
	for _, c := range r.Checks {
		fmt.Fprintf(w, "[%s] %s: %s\n", c.State, c.ID, c.Detail)
		if c.Remedy != "" {
			fmt.Fprintf(w, "  %s\n", c.Remedy)
		}
	}
}
