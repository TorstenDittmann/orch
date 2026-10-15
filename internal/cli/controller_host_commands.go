package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"change.codes/torsten/better-swarm/internal/host"
)

func runClusterHost(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	command := args[0]
	flags := flag.NewFlagSet("cluster "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var options host.ControllerOptions
	var dryRun, jsonOutput bool
	var wait time.Duration
	flags.StringVar(&options.Server, "server", "", "canonical controller HTTPS URL")
	flags.StringVar(&options.Listen, "listen", "", "controller IP listen address")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if command == "install" {
		flags.StringVar(&options.Name, "name", "", "cluster name")
		flags.StringVar(&options.AdminDir, "admin-dir", "", "new portable administrator directory")
		flags.StringVar(&options.RecoveryDir, "recovery-dir", "", "optional new day-zero recovery directory")
		flags.BoolVar(&dryRun, "dry-run", false, "inspect and show planned actions without modifying the host")
		flags.DurationVar(&wait, "wait-timeout", 2*time.Minute, "maximum wait for authenticated controller readiness")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 || strings.TrimSpace(options.Server) == "" || strings.TrimSpace(options.Listen) == "" || command == "install" && wait <= 0 {
		writeError(stderr, errors.New("requires a valid --server and --listen; install also requires --name, --admin-dir, and a positive --wait-timeout"), jsonOutput)
		return ExitInvalid
	}
	if command == "doctor" {
		plan, err := host.BuildControllerPlan(ctx, options)
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitInvalid
		}
		report := plan.Report
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
	if strings.TrimSpace(options.Name) == "" || strings.TrimSpace(options.AdminDir) == "" {
		writeError(stderr, errors.New("cluster install requires --name and --admin-dir"), jsonOutput)
		return ExitInvalid
	}
	if dryRun {
		plan, err := host.BuildControllerPlan(ctx, options)
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
			for _, path := range plan.Files {
				if path != "" {
					fmt.Fprintln(stdout, "Own:", path)
				}
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
	result, err := host.InstallController(ctx, host.ControllerInstallOptions{ControllerOptions: options, WaitTimeout: wait, Progress: stderr})
	if jsonOutput {
		if code := writeJSON(stdout, stderr, result); code != ExitOK {
			return code
		}
	} else if result.ClusterID != "" {
		fmt.Fprintf(stdout, "Controller %s (%s): ready=%t changed=%t\n", result.Name, result.ClusterID, result.Ready, result.Changed)
		fmt.Fprintln(stdout, "Administrator context:", result.AdminPath)
		if result.RecoveryDir != "" {
			fmt.Fprintln(stdout, "Recovery set:", result.RecoveryDir)
		}
		if result.Ready {
			fmt.Fprintln(stdout, "Copy the private directories off-host, then import the administrator context with orch target import.")
		}
	}
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	return ExitOK
}
