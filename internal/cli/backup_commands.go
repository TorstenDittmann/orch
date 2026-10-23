package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
	"github.com/charmbracelet/x/term"
)

func printBackupHelp(output io.Writer) {
	fmt.Fprintln(output, `orch backup copies every backup off its node to an S3 bucket.

Usage:
  orch backup destination [--target NAME] [--json]
  orch backup destination set s3://BUCKET[/PREFIX] [--endpoint URL] [--region REGION] [--target NAME] [--json]
  orch backup destination remove [--target NAME] [--json]

Once a destination is set, volume and database backups and the controller's
daily backups are copied to the bucket within 15 minutes, the bucket keeps as
many as the node does, and orch volume restore and orch db restore fetch a
backup from the bucket when its node no longer has it.

Any S3-compatible service works. Leave out --endpoint for AWS; otherwise pass
the service's endpoint, such as https://ACCOUNT.r2.cloudflarestorage.com.
The access key comes from AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY when
they are set, and is asked for otherwise. It is stored encrypted on the
controller and never sent to workers.`)
}

func runBackup(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		if len(args) == 0 {
			printBackupHelp(stderr)
			return ExitInvalid
		}
		printBackupHelp(stdout)
		return ExitOK
	}
	if args[0] != "destination" {
		fmt.Fprintf(stderr, "unknown backup command %q\n\n", args[0])
		printBackupHelp(stderr)
		return ExitInvalid
	}
	action, rest := backupDestinationAction(args[1:])
	switch action {
	case "show":
		return runBackupDestinationShow(ctx, rest, stdout, stderr)
	case "set":
		return runBackupDestinationSet(ctx, rest, stdout, stderr)
	case "remove":
		return runBackupDestinationRemove(ctx, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown backup destination command %q\n\n", action)
		printBackupHelp(stderr)
		return ExitInvalid
	}
}

// backupDestinationAction finds the subcommand among the arguments, so flags
// may come before it, and returns the remaining arguments.
func backupDestinationAction(args []string) (string, []string) {
	valueFlags := map[string]bool{"-context": true, "--context": true, "-endpoint": true, "--endpoint": true, "-region": true, "--region": true}
	for index := 0; index < len(args); index++ {
		if strings.HasPrefix(args[index], "-") {
			if valueFlags[args[index]] {
				index++
			}
			continue
		}
		rest := append(append([]string(nil), args[:index]...), args[index+1:]...)
		return args[index], rest
	}
	return "show", args
}

type backupDestinationOutput struct {
	URL            string     `json:"url,omitempty"`
	Endpoint       string     `json:"endpoint,omitempty"`
	Region         string     `json:"region,omitempty"`
	AccessKeyID    string     `json:"access_key_id,omitempty"`
	UploadedAt     *time.Time `json:"uploaded_at,omitempty"`
	UploadFailedAt *time.Time `json:"upload_failed_at,omitempty"`
	UploadFailure  string     `json:"upload_failure,omitempty"`
}

func backupDestinationJSON(destination *orchv1alpha1.BackupDestination) backupDestinationOutput {
	return backupDestinationOutput{URL: destination.GetUrl(), Endpoint: destination.GetEndpoint(), Region: destination.GetRegion(),
		AccessKeyID: destination.GetAccessKeyId()}
}

func runBackupDestinationShow(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	parsed, ok := parseVolumeFlags("backup destination", args, stderr, false)
	if !ok {
		return ExitInvalid
	}
	if parsed.flags.NArg() != 0 || parsed.contextPath == "" {
		fmt.Fprintln(stderr, "backup destination accepts no arguments and requires --context")
		return ExitInvalid
	}
	remote, err := controlclient.New(parsed.contextPath)
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	response, err := remote.GetBackupDestination(ctx)
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitRuntime
	}
	destination := response.GetDestination()
	if parsed.jsonOutput {
		if destination == nil {
			return writeJSON(stdout, stderr, nil)
		}
		result := backupDestinationJSON(destination)
		result.UploadFailure = response.GetUploadFailure()
		if value := response.GetUploadedAt(); value != nil && value.CheckValid() == nil {
			at := value.AsTime().UTC()
			result.UploadedAt = &at
		}
		if value := response.GetUploadFailedAt(); value != nil && value.CheckValid() == nil {
			at := value.AsTime().UTC()
			result.UploadFailedAt = &at
		}
		return writeJSON(stdout, stderr, result)
	}
	if destination == nil {
		fmt.Fprintln(stdout, "Backups stay on their nodes. Copy them to a bucket with: orch backup destination set s3://BUCKET/PREFIX")
		return ExitOK
	}
	fmt.Fprintf(stdout, "Destination: %s\n", destination.GetUrl())
	if destination.GetEndpoint() != "" {
		fmt.Fprintf(stdout, "Endpoint:    %s\n", destination.GetEndpoint())
	}
	fmt.Fprintf(stdout, "Region:      %s\nAccess key:  %s\n", destination.GetRegion(), destination.GetAccessKeyId())
	switch {
	case response.GetUploadFailure() != "":
		fmt.Fprintf(stdout, "Last copy FAILED at %s: %s\n", response.GetUploadFailedAt().AsTime().UTC().Format(time.RFC3339), response.GetUploadFailure())
	case response.GetUploadedAt() != nil:
		fmt.Fprintf(stdout, "Last copy:   %s (every scheduled backup is in the bucket)\n", response.GetUploadedAt().AsTime().UTC().Format(time.RFC3339))
	default:
		fmt.Fprintln(stdout, "Last copy:   not yet")
	}
	return ExitOK
}

func runBackupDestinationSet(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("backup destination set", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath, endpoint, region string
	var jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.StringVar(&endpoint, "endpoint", "", "S3 endpoint URL; leave out for AWS")
	flags.StringVar(&region, "region", "", "signing region; detected when left out")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	valueFlags := map[string]bool{"-context": true, "--context": true, "-endpoint": true, "--endpoint": true, "-region": true, "--region": true}
	if err := flags.Parse(reorderInterspersed(args, valueFlags)); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 1 || contextPath == "" {
		writeError(stderr, errors.New("backup destination set requires s3://BUCKET[/PREFIX] and --context"), jsonOutput)
		return ExitInvalid
	}
	if endpoint == "" {
		endpoint = firstEnv("AWS_ENDPOINT_URL_S3", "AWS_ENDPOINT_URL")
	}
	accessKeyID, secretAccessKey, err := backupCredentials(ctx, stderr)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	destination, err := remote.SetBackupDestination(ctx, &orchv1alpha1.SetBackupDestinationRequest{
		Url: flags.Arg(0), Endpoint: endpoint, Region: region, AccessKeyId: accessKeyID, SecretAccessKey: secretAccessKey,
	})
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, backupDestinationJSON(destination))
	}
	fmt.Fprintf(stdout, "Backups now copy to %s (region %s).\nExisting backups upload in the background; check with: orch backup destination\n",
		destination.GetUrl(), destination.GetRegion())
	return ExitOK
}

// backupCredentials reads the bucket's access key from the standard AWS
// variables, or asks for it.
func backupCredentials(ctx context.Context, stderr io.Writer) (string, string, error) {
	accessKeyID, secretAccessKey := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY")
	if accessKeyID != "" && secretAccessKey != "" {
		return accessKeyID, secretAccessKey, nil
	}
	input := secretInput(ctx)
	file, isFile := input.(*os.File)
	interactive := isFile && term.IsTerminal(file.Fd())
	reader := bufio.NewReader(input)
	if accessKeyID == "" {
		if interactive {
			fmt.Fprint(stderr, "Access key ID: ")
		}
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return "", "", errors.New("set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, or enter the access key when asked")
		}
		accessKeyID = strings.TrimSpace(line)
	}
	if secretAccessKey == "" {
		if interactive {
			fmt.Fprint(stderr, "Secret access key: ")
			secret, err := term.ReadPassword(file.Fd())
			fmt.Fprintln(stderr)
			if err != nil {
				return "", "", err
			}
			secretAccessKey = strings.TrimSpace(string(secret))
		} else {
			line, _ := reader.ReadString('\n')
			secretAccessKey = strings.TrimSpace(line)
		}
	}
	if accessKeyID == "" || secretAccessKey == "" {
		return "", "", errors.New("an access key ID and secret access key are required")
	}
	return accessKeyID, secretAccessKey, nil
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

func runBackupDestinationRemove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	parsed, ok := parseVolumeFlags("backup destination remove", args, stderr, false)
	if !ok {
		return ExitInvalid
	}
	if parsed.flags.NArg() != 0 || parsed.contextPath == "" {
		fmt.Fprintln(stderr, "backup destination remove accepts no arguments and requires --context")
		return ExitInvalid
	}
	remote, err := controlclient.New(parsed.contextPath)
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	changed, err := remote.RemoveBackupDestination(ctx)
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitRuntime
	}
	if parsed.jsonOutput {
		return writeJSON(stdout, stderr, struct {
			Changed bool `json:"changed"`
		}{changed})
	}
	if changed {
		fmt.Fprintln(stdout, "Backups stay on their nodes again. What is already in the bucket stays there.")
	} else {
		fmt.Fprintln(stdout, "No backup destination was set.")
	}
	return ExitOK
}
