package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	controlclient "change.codes/torsten/better-swarm/internal/client"
	"change.codes/torsten/better-swarm/internal/config"
)

func runAppDeployments(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	command := args[0]
	flags := flag.NewFlagSet("app "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath, against, revision string
	var jsonOutput bool
	var limit uint
	var before uint64
	var timeout time.Duration
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	valueFlags := map[string]bool{"--context": true, "-context": true}
	switch command {
	case "history":
		flags.UintVar(&limit, "limit", 20, "history page size (1-50)")
		flags.Uint64Var(&before, "before-generation", 0, "exclusive generation cursor")
		valueFlags["--limit"], valueFlags["--before-generation"] = true, true
	case "diff":
		flags.StringVar(&against, "against", "", "target revision (defaults to current desired state)")
		valueFlags["--against"] = true
	case "watch":
		flags.StringVar(&revision, "revision", "", "revision to watch (defaults to current desired state)")
		flags.DurationVar(&timeout, "timeout", 10*time.Minute, "rollout wait deadline")
		valueFlags["--revision"], valueFlags["--timeout"] = true, true
	}
	if err := flags.Parse(reorderInterspersed(args[1:], valueFlags)); err != nil {
		return ExitInvalid
	}
	count, positional := 1, "APP"
	if command == "diff" {
		count, positional = 2, "APP REVISION"
	}
	if flags.NArg() != count || contextPath == "" || command == "history" && (limit < 1 || limit > 50 || before > math.MaxInt64) || command == "watch" && timeout <= 0 {
		writeError(stderr, fmt.Errorf("app %s requires %s and valid bounds", command, positional), jsonOutput)
		return ExitInvalid
	}
	name := flags.Arg(0)
	if err := config.ValidateApplicationName(name); err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	switch command {
	case "history":
		response, err := remote.ListDeployments(ctx, name, before, uint32(limit))
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		if jsonOutput {
			return writeJSON(stdout, stderr, response)
		}
		for _, d := range response.GetDeployments() {
			markers := ""
			if d.GetCurrent() {
				markers += " current"
			}
			if d.GetActive() {
				markers += " active"
			}
			fmt.Fprintf(stdout, "%d  %s  %s  %s%s\n", d.GetGeneration(), d.GetRevisionId(), d.GetCreatedAt().AsTime().Format(time.RFC3339), d.GetOutcome(), markers)
			if d.GetReason() != "" {
				fmt.Fprintf(stdout, "  %s\n", d.GetReason())
			}
			if d.GetRollbackFromRevisionId() != "" {
				fmt.Fprintf(stdout, "  rollback from %s\n", d.GetRollbackFromRevisionId())
			}
			for _, image := range d.GetImages() {
				fmt.Fprintf(stdout, "  %s %s  %s\n", image.GetKind(), image.GetName(), deploymentImageReference(image))
			}
		}
		if response.GetNextBeforeGeneration() != 0 {
			fmt.Fprintf(stdout, "More: orch app history %s --before-generation %d\n", name, response.GetNextBeforeGeneration())
		}
	case "diff":
		response, err := remote.CompareDeployments(ctx, name, flags.Arg(1), against)
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		if jsonOutput {
			return writeJSON(stdout, stderr, response)
		}
		fmt.Fprintf(stdout, "%s: generation %d -> %d\n", name, response.GetFrom().GetGeneration(), response.GetTo().GetGeneration())
		if len(response.GetChanges()) == 0 {
			fmt.Fprintln(stdout, "No configuration changes.")
		}
		for _, change := range response.GetChanges() {
			fmt.Fprintf(stdout, "%s %s: %s -> %s\n", change.GetKind(), change.GetPath(), change.GetBefore(), change.GetAfter())
		}
		if response.GetTruncated() {
			fmt.Fprintln(stdout, "Comparison truncated to 500 changes.")
		}
	case "watch":
		current, err := remote.GetApplication(ctx, name)
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		generation := current.GetGeneration()
		if revision == "" {
			revision = current.GetRevisionId()
		} else {
			d, err := remote.GetDeployment(ctx, name, revision)
			if err != nil {
				writeError(stderr, err, jsonOutput)
				return ExitRuntime
			}
			generation = d.GetGeneration()
		}
		return waitForDeployment(ctx, remote, name, generation, revision, "", timeout, jsonOutput, stdout, stderr)
	}
	return ExitOK
}

func runRollback(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("rollback", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath, key string
	var expected uint64
	var wait, jsonOutput bool
	var timeout time.Duration
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.StringVar(&key, "idempotency-key", "", "stable retry key (requires --expected-generation)")
	flags.Uint64Var(&expected, "expected-generation", 0, "generation being replaced (defaults to current)")
	flags.BoolVar(&wait, "wait", false, "wait for rollback revision readiness")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	flags.DurationVar(&timeout, "timeout", 10*time.Minute, "rollout wait deadline")
	values := map[string]bool{"--context": true, "-context": true, "--idempotency-key": true, "--expected-generation": true, "--timeout": true}
	if err := flags.Parse(reorderInterspersed(args, values)); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 2 || contextPath == "" || timeout <= 0 || expected > math.MaxInt64 || key != "" && (expected == 0 || !cliIdempotencyKeyPattern.MatchString(key)) {
		writeError(stderr, errors.New("rollback requires APP REVISION; explicit retry keys require --expected-generation; timeout must be positive"), jsonOutput)
		return ExitInvalid
	}
	name, revision := flags.Arg(0), flags.Arg(1)
	if err := config.ValidateApplicationName(name); err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitInvalid
	}
	if strings.TrimSpace(revision) == "" || len(revision) > 128 {
		writeError(stderr, errors.New("revision must contain 1-128 bytes"), jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	if expected == 0 {
		current, err := remote.GetApplication(ctx, name)
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		expected = current.GetGeneration()
	}
	if key == "" {
		key, err = newIdempotencyKey(rand.Reader)
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
	}
	retry := fmt.Sprintf("orch rollback %s %s --expected-generation %d --idempotency-key %s", name, revision, expected, key)
	// Emit the retry identity before sending the mutation, including on lost replies.
	if !jsonOutput {
		fmt.Fprintf(stderr, "Retry with the same target: %s\n", retry)
	}
	response, err := remote.RollbackApplication(ctx, &orchv1alpha1.RollbackApplicationRequest{Application: name, RevisionId: revision, ExpectedGeneration: expected, IdempotencyKey: key})
	if err != nil {
		writeError(stderr, fmt.Errorf("rollback failed; retry on the same target with %s: %w", retry, err), jsonOutput)
		return ExitRuntime
	}
	if response.GetName() != name || response.GetGeneration() != expected+1 || response.GetRevisionId() == "" || !response.GetChanged() || response.GetExecutionStatus() != orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_PENDING {
		writeError(stderr, errors.New("controller returned inconsistent rollback acceptance"), jsonOutput)
		return ExitRuntime
	}
	if wait {
		return waitForDeployment(ctx, remote, name, response.GetGeneration(), response.GetRevisionId(), key, timeout, jsonOutput, stdout, stderr)
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			Deployment         *orchv1alpha1.ApplyApplicationResponse `json:"deployment"`
			IdempotencyKey     string                                 `json:"idempotency_key"`
			ExpectedGeneration uint64                                 `json:"expected_generation"`
		}{response, key, expected})
	}
	fmt.Fprintf(stdout, "Rollback accepted for %s: generation %d (%s), from %s.\nStatus: PENDING\nIdempotency key: %s\n", name, response.GetGeneration(), response.GetRevisionId(), revision, key)
	return ExitOK
}

func waitForDeployment(ctx context.Context, reader controlclient.ApplicationReader, name string, generation uint64, revision, key string, timeout time.Duration, jsonOutput bool, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if !jsonOutput {
		fmt.Fprintf(stdout, "Watching %s generation %d (%s)\n", name, generation, revision)
		if key != "" {
			fmt.Fprintf(stdout, "Idempotency key: %s\n", key)
		}
	}
	previous := ""
	latest, err := controlclient.WaitDeployment(ctx, reader, name, generation, revision, time.Second, func(app *orchv1alpha1.Application) {
		if jsonOutput {
			return
		}
		var progress strings.Builder
		fmt.Fprintf(&progress, "%s  observed=%d active=%d  %s\n", strings.TrimPrefix(app.GetExecutionStatus().String(), "EXECUTION_STATUS_"), app.GetObservedGeneration(), app.GetActiveGeneration(), app.GetExecutionReason())
		for _, a := range app.GetAllocations() {
			fmt.Fprintf(&progress, "  %s #%d on %s: %s / %s  active=%d  %s\n", a.GetService(), a.GetReplica(), a.GetNodeName(), strings.TrimPrefix(a.GetPhase().String(), "ALLOCATION_PHASE_"), strings.TrimPrefix(a.GetHealthStatus().String(), "HEALTH_STATUS_"), a.GetActiveApplicationGeneration(), a.GetReason())
		}
		if value := progress.String(); value != previous {
			fmt.Fprint(stdout, value)
			previous = value
		}
	})
	status := "READY"
	failure := ""
	if err != nil {
		status = "FAILED"
		failure = err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			status = "TIMED_OUT"
		} else if errors.Is(err, context.Canceled) {
			status = "CANCELED"
		}
	}
	if jsonOutput {
		code := writeJSON(stdout, stderr, struct {
			Application     string `json:"application"`
			Generation      uint64 `json:"generation"`
			RevisionID      string `json:"revision_id"`
			Status          string `json:"status"`
			IdempotencyKey  string `json:"idempotency_key,omitempty"`
			ExecutionStatus string `json:"execution_status"`
			Error           string `json:"error,omitempty"`
		}{name, generation, revision, status, key, latest.GetExecutionStatus().String(), failure})
		if code != ExitOK {
			return code
		}
	} else if err == nil {
		fmt.Fprintf(stdout, "Ready: %s generation %d.\n", name, generation)
	}
	if err != nil {
		if !jsonOutput {
			writeError(stderr, fmt.Errorf("%s; watch again with orch app watch %s --revision %s (stopping the watcher does not cancel deployment)", failure, name, revision), false)
		}
		return ExitRuntime
	}
	return ExitOK
}

func deploymentImageReference(image *orchv1alpha1.DeploymentImage) string {
	if image.GetRequestedImage() != "" && image.GetRequestedImage() != image.GetImage() {
		return image.GetRequestedImage() + " -> " + image.GetImage()
	}
	return image.GetImage()
}
