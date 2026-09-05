package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	controlclient "change.codes/torsten/better-swarm/internal/client"
)

func runNode(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: orch node label|unlabel|cordon|uncordon|drain|ingress|remove NODE --context FILE")
		return ExitInvalid
	}
	if args[0] == "install" || args[0] == "doctor" {
		return runNodeHost(ctx, args, stdout, stderr)
	}
	if args[0] == "join" {
		return runNodeJoin(ctx, args[1:], stdout, stderr)
	}
	command := args[0]
	flags := flag.NewFlagSet("node "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath, idempotencyKey string
	var jsonOutput, enable, disable, lost, confirmFenced bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.StringVar(&idempotencyKey, "idempotency-key", "", "stable key for safe retries")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if command == "ingress" {
		flags.BoolVar(&enable, "enable", false, "enable ingress on this node")
		flags.BoolVar(&disable, "disable", false, "disable ingress on this node")
	}
	if command == "remove" {
		flags.BoolVar(&lost, "lost", false, "remove an unreachable, externally fenced node")
		flags.BoolVar(&confirmFenced, "confirm-fenced", false, "confirm that a lost node cannot run workloads")
	}
	reordered := reorderInterspersed(args[1:], map[string]bool{
		"-context": true, "--context": true, "-idempotency-key": true, "--idempotency-key": true,
	})
	if err := flags.Parse(reordered); err != nil {
		return ExitInvalid
	}
	if contextPath == "" {
		writeError(stderr, errors.New("node operations require --context"), jsonOutput)
		return ExitInvalid
	}
	if idempotencyKey == "" {
		var err error
		idempotencyKey, err = newIdempotencyKey(rand.Reader)
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
	}
	if !cliIdempotencyKeyPattern.MatchString(idempotencyKey) {
		writeError(stderr, errors.New("invalid --idempotency-key"), jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()

	if command == "remove" {
		if flags.NArg() != 1 || lost != confirmFenced {
			writeError(stderr, errors.New("node remove requires NODE; --lost and --confirm-fenced must be used together"), jsonOutput)
			return ExitInvalid
		}
		response, err := remote.RemoveNode(ctx, &orchv1alpha1.RemoveNodeRequest{
			Name: flags.Arg(0), Lost: lost, ConfirmFenced: confirmFenced, IdempotencyKey: idempotencyKey,
		})
		if err != nil {
			writeError(stderr, fmt.Errorf("node removal failed; retry with --idempotency-key %s: %w", idempotencyKey, err), jsonOutput)
			return ExitRuntime
		}
		if jsonOutput {
			return writeJSON(stdout, stderr, response)
		}
		fmt.Fprintf(stdout, "Removed node %s (changed=%t)\n", response.GetName(), response.GetChanged())
		return ExitOK
	}

	request := &orchv1alpha1.UpdateNodeRequest{IdempotencyKey: idempotencyKey}
	switch command {
	case "label":
		if flags.NArg() < 2 {
			writeError(stderr, errors.New("node label requires NODE KEY=VALUE..."), jsonOutput)
			return ExitInvalid
		}
		request.Name, request.Labels = flags.Arg(0), make(map[string]string, flags.NArg()-1)
		for _, pair := range flags.Args()[1:] {
			key, value, ok := strings.Cut(pair, "=")
			if !ok || key == "" || value == "" {
				writeError(stderr, fmt.Errorf("invalid node label %q; expected KEY=VALUE", pair), jsonOutput)
				return ExitInvalid
			}
			request.Labels[key] = value
		}
	case "unlabel":
		if flags.NArg() < 2 {
			writeError(stderr, errors.New("node unlabel requires NODE KEY..."), jsonOutput)
			return ExitInvalid
		}
		request.Name, request.RemoveLabels = flags.Arg(0), append([]string(nil), flags.Args()[1:]...)
	case "cordon", "uncordon", "drain":
		if flags.NArg() != 1 {
			writeError(stderr, fmt.Errorf("node %s requires NODE", command), jsonOutput)
			return ExitInvalid
		}
		request.Name = flags.Arg(0)
		request.Lifecycle = orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_ACTIVE
		if command == "cordon" {
			request.Lifecycle = orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_CORDONED
		} else if command == "drain" {
			request.Lifecycle = orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_DRAINING
		}
	case "ingress":
		if flags.NArg() != 1 || enable == disable {
			writeError(stderr, errors.New("node ingress requires NODE and exactly one of --enable or --disable"), jsonOutput)
			return ExitInvalid
		}
		request.Name, request.SetIngress, request.Ingress = flags.Arg(0), true, enable
	default:
		writeError(stderr, fmt.Errorf("unknown node command %q", command), jsonOutput)
		return ExitInvalid
	}
	response, err := remote.UpdateNode(ctx, request)
	if err != nil {
		writeError(stderr, fmt.Errorf("node update failed; retry with --idempotency-key %s: %w", idempotencyKey, err), jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, response)
	}
	fmt.Fprintf(stdout, "Updated node %s (lifecycle=%s ingress=%t changed=%t)\n",
		response.GetNode().GetName(), response.GetNode().GetLifecycle(), response.GetNode().GetIngress(), response.GetChanged())
	return ExitOK
}
