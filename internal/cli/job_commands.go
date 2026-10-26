package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
)

func runJob(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "job requires run, list, show, logs, cancel, retry, or wait")
		return ExitInvalid
	}
	action := args[0]
	args = args[1:]
	var command []string
	for i, arg := range args {
		if arg == "--" {
			command = args[i+1:]
			args = args[:i]
			break
		}
	}
	flags := flag.NewFlagSet("job "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var path, key, revision, before string
	var limit, tail uint
	var timeout, waitTimeout time.Duration
	var wait, jsonOutput bool
	flags.StringVar(&path, "context", "", "administrator context path")
	flags.StringVar(&key, "idempotency-key", "", "stable key for run or retry")
	flags.StringVar(&revision, "revision", "", "require this current application revision")
	flags.StringVar(&before, "before", "", "exclusive job history cursor")
	flags.UintVar(&limit, "limit", 20, "maximum jobs (1-100)")
	flags.UintVar(&tail, "tail", 100, "log lines per stream (1-10000)")
	flags.DurationVar(&timeout, "timeout", 15*time.Minute, "process timeout or wait deadline")
	flags.DurationVar(&waitTimeout, "wait-timeout", 20*time.Minute, "maximum time to wait for run or cancellation")
	flags.BoolVar(&wait, "wait", false, "wait for the exact run to finish")
	flags.BoolVar(&jsonOutput, "json", false, "machine-readable output")
	values := map[string]bool{}
	for _, name := range []string{"context", "idempotency-key", "revision", "before", "limit", "tail", "timeout", "wait-timeout"} {
		values["--"+name] = true
		values["-"+name] = true
	}
	if err := flags.Parse(reorderInterspersed(args, values)); err != nil {
		return ExitInvalid
	}
	if path == "" || limit < 1 || limit > 100 || tail < 1 || tail > 10000 || timeout < 10*time.Second || timeout > 24*time.Hour || waitTimeout < time.Second || waitTimeout > 24*time.Hour {
		fmt.Fprintln(stderr, "job requires a target/context and bounded limit, tail, and timeout (10s-24h)")
		return ExitInvalid
	}
	expected := 1
	if action == "run" {
		expected = 2
	}
	if action == "list" && flags.NArg() == 0 {
		expected = 0
	}
	if flags.NArg() != expected || (action == "run" && len(command) == 0) || (action != "run" && len(command) > 0) {
		fmt.Fprintln(stderr, "usage: job run APP SERVICE [options] -- COMMAND [ARG...]; job list [APP]; job show|logs|cancel|retry|wait ID")
		return ExitInvalid
	}
	remote, err := controlclient.New(path)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	if action == "list" {
		response, err := remote.ListJobs(ctx, flags.Arg(0), before, uint32(limit))
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		if jsonOutput {
			return writeJSON(stdout, stderr, response)
		}
		for _, job := range response.GetJobs() {
			printJob(stdout, job)
		}
		if response.GetNextBeforeId() != "" {
			fmt.Fprintf(stdout, "Next page: --before %s\n", response.GetNextBeforeId())
		}
		return ExitOK
	}
	var job *orchv1alpha1.OneOffJob
	if action == "run" || action == "retry" {
		if key == "" {
			var random [16]byte
			if _, err := rand.Read(random[:]); err != nil {
				writeError(stderr, err, jsonOutput)
				return ExitRuntime
			}
			key = hex.EncodeToString(random[:])
		}
		// Print the retry key before the request, including uncertain responses.
		fmt.Fprintf(stderr, "Idempotency key: %s\n", key)
	}
	switch action {
	case "run":
		job, err = remote.RunJob(ctx, &orchv1alpha1.RunJobRequest{Application: flags.Arg(0), Service: flags.Arg(1), ExpectedRevisionId: revision, Command: command[:1], Args: command[1:], TimeoutSeconds: uint32(timeout / time.Second), IdempotencyKey: key})
	case "retry":
		job, err = remote.RetryJob(ctx, flags.Arg(0), key)
	case "cancel":
		job, err = remote.CancelJob(ctx, flags.Arg(0))
	case "show", "wait", "logs":
		job, err = remote.GetJob(ctx, flags.Arg(0))
	default:
		fmt.Fprintln(stderr, "unknown job action")
		return ExitInvalid
	}
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if action == "logs" {
		stream, err := remote.TailCronLogs(ctx, job.GetApplication(), job.GetStatus().GetJob(), job.GetId(), uint32(tail))
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		defer stream.Close()
		for stream.Receive() {
			frame := stream.Msg()
			out := stdout
			if frame.GetStream() == orchv1alpha1.LogStream_LOG_STREAM_STDERR {
				out = stderr
			}
			if _, err := writeWorkloadOutput(out, frame.GetData()); err != nil {
				writeError(stderr, err, false)
				return ExitRuntime
			}
		}
		if err := stream.Err(); err != nil {
			writeError(stderr, err, false)
			return ExitRuntime
		}
		return ExitOK
	}
	if wait || action == "wait" {
		if action == "wait" {
			waitTimeout = timeout
		}
		waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
		defer cancel()
		job, err = remote.WaitJob(waitCtx, job.GetId())
	}
	if job != nil {
		if jsonOutput {
			if code := writeJSON(stdout, stderr, job); code != ExitOK {
				return code
			}
		} else {
			printJob(stdout, job)
		}
	}
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	return ExitOK
}
func printJob(out io.Writer, job *orchv1alpha1.OneOffJob) {
	status := job.GetStatus()
	suffix := ""
	if job.GetCancelRequested() && status.GetFinishedAt() == nil {
		suffix = " cancellation requested"
	}
	fmt.Fprintf(out, "%s  %s/%s  %s  %s%s\n", job.GetId(), job.GetApplication(), job.GetService(), status.GetState(), status.GetReason(), suffix)
	fmt.Fprintf(out, "  revision=%s image=%s node=%s kind=%s\n", job.GetRevisionId(), job.GetImage(), job.GetNodeId(), job.GetKind())
	if status.GetHasExitCode() {
		fmt.Fprintf(out, "  exit_code=%d\n", status.GetExitCode())
	}
}
