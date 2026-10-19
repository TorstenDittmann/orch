package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/TorstenDittmann/orch/internal/host"
)

func runHostUpgrade(ctx context.Context, kind string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(kind+" upgrade", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var binary string
	var dryRun, jsonOutput bool
	var wait time.Duration
	flags.StringVar(&binary, "binary", "", "trusted local candidate orch executable (default: this orch)")
	flags.BoolVar(&dryRun, "dry-run", false, "validate and show the upgrade without changing the host")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	flags.DurationVar(&wait, "wait-timeout", 5*time.Minute, "maximum wait for service readiness")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 || wait <= 0 {
		writeError(stderr, errors.New("upgrade takes only flags and a positive --wait-timeout"), jsonOutput)
		return ExitInvalid
	}
	if strings.TrimSpace(binary) == "" {
		// The orch running this command is the candidate, so installing a new
		// orch and running its upgrade is enough.
		executable, err := os.Executable()
		if err != nil {
			writeError(stderr, fmt.Errorf("locate this orch binary; pass --binary FILE: %w", err), jsonOutput)
			return ExitRuntime
		}
		binary = executable
	}
	options := host.UpgradeOptions{Binary: binary, DryRun: dryRun, WaitTimeout: wait, Progress: stderr}
	var result host.UpgradeResult
	var err error
	if kind == "cluster" {
		result, err = host.UpgradeController(ctx, options)
	} else {
		result, err = host.UpgradeNode(ctx, options)
	}
	if jsonOutput {
		if code := writeJSON(stdout, stderr, result); code != ExitOK {
			return code
		}
	} else if result.Candidate.Version != "" {
		fmt.Fprintf(stdout, "%s upgrade: compatible=%t ready=%t changed=%t phase=%s\n", result.Kind, result.Compatible, result.Ready, result.Changed, result.Phase)
		fmt.Fprintf(stdout, "Candidate: %s (%s) SHA256 %s\n", result.Candidate.Version, result.Candidate.BuildID, result.CandidateHash)
		for _, action := range result.Actions {
			fmt.Fprintln(stdout, "Action:", action)
		}
		if result.RecoveryPath != "" {
			fmt.Fprintln(stdout, "Recovery:", result.RecoveryPath)
		}
		if kind == "cluster" && err == nil && result.Ready {
			fmt.Fprintf(stdout, "Workers upgrade themselves to %s one at a time. Follow them with: orch nodes\n", result.Candidate.Version)
		}
	}
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	return ExitOK
}
