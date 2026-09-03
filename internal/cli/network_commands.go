package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	controlclient "change.codes/torsten/better-swarm/internal/client"
	"change.codes/torsten/better-swarm/internal/model"
)

func runNetwork(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printNetworkHelp(stderr)
		return ExitInvalid
	}
	switch args[0] {
	case "configure":
		return runNetworkConfigure(ctx, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printNetworkHelp(stdout)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "unknown network command %q\n\n", args[0])
		printNetworkHelp(stderr)
		return ExitInvalid
	}
}

func runNetworkConfigure(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("network configure", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var cidr, contextPath, idempotencyKey string
	var jsonOutput bool
	flags.StringVar(&cidr, "cidr", model.DefaultWorkloadCIDR, "RFC1918 workload CIDR (/16 through /19)")
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.StringVar(&idempotencyKey, "idempotency-key", "", "stable key for safe retries")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 || strings.TrimSpace(contextPath) == "" {
		writeError(stderr, errors.New("network configure requires --context and accepts flags only"), jsonOutput)
		return ExitInvalid
	}
	if _, err := model.ParseWorkloadCIDR(cidr); err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitInvalid
	}
	if idempotencyKey == "" {
		var err error
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
	response, err := remote.ConfigureNetwork(ctx, cidr, idempotencyKey)
	if err != nil {
		writeError(stderr, fmt.Errorf("network configuration failed; retry with --idempotency-key %s: %w", idempotencyKey, err), jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			CIDR           string `json:"cidr"`
			Generation     uint64 `json:"generation"`
			Frozen         bool   `json:"frozen"`
			Changed        bool   `json:"changed"`
			EventSequence  uint64 `json:"event_sequence"`
			IdempotencyKey string `json:"idempotency_key"`
		}{response.GetCidr(), response.GetGeneration(), response.GetFrozen(), response.GetChanged(), response.GetEventSequence(), idempotencyKey})
	}
	fmt.Fprintf(stdout, "Workload network %s (generation %d, frozen %t, changed %t)\nIdempotency key: %s\n",
		response.GetCidr(), response.GetGeneration(), response.GetFrozen(), response.GetChanged(), idempotencyKey)
	return ExitOK
}

func printNetworkHelp(output io.Writer) {
	fmt.Fprintln(output, `orch network configures the one-node workload network.

Usage:
  orch network configure --cidr CIDR --context FILE [--idempotency-key KEY] [--json]`)
}
