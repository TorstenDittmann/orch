package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
	"github.com/TorstenDittmann/orch/internal/config"
)

func printVolumeHelp(output io.Writer) {
	fmt.Fprintln(output, `orch volume works with an application's local volumes and their backups.

Usage:
  orch volume list [APP] [--target NAME] [--json]
  orch volume backups APP VOLUME [--target NAME] [--json]
  orch volume backup APP VOLUME [--target NAME] [--json]
  orch volume restore APP VOLUME BACKUP [--target NAME] [--json]
  orch volume download APP VOLUME BACKUP [--output FILE|-] [--target NAME]

Backups are archives kept on the node that holds the volume, and copied to
the bucket when orch backup destination is set. Restoring stops the service
that mounts the volume, swaps in the backup, and starts it again; the
volume's previous contents are kept as a backup first. A backup that is only
in the bucket is downloaded to the node first.`)
}

func runVolume(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printVolumeHelp(stderr)
		return ExitInvalid
	}
	switch args[0] {
	case "list":
		return runVolumeList(ctx, args[1:], stdout, stderr)
	case "backups":
		return runVolumeBackups(ctx, args[1:], stdout, stderr)
	case "backup":
		return runVolumeBackup(ctx, args[1:], stdout, stderr)
	case "restore":
		return runVolumeRestore(ctx, args[1:], stdout, stderr)
	case "download":
		return runVolumeDownload(ctx, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printVolumeHelp(stdout)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "unknown volume command %q\n\n", args[0])
		printVolumeHelp(stderr)
		return ExitInvalid
	}
}

type volumeFlags struct {
	flags       *flag.FlagSet
	contextPath string
	jsonOutput  bool
	output      string
}

func parseVolumeFlags(name string, args []string, stderr io.Writer, withOutput bool) (*volumeFlags, bool) {
	parsed := &volumeFlags{flags: flag.NewFlagSet(name, flag.ContinueOnError)}
	parsed.flags.SetOutput(stderr)
	parsed.flags.StringVar(&parsed.contextPath, "context", "", "administrator context path")
	valueFlags := map[string]bool{"-context": true, "--context": true}
	if withOutput {
		parsed.flags.StringVar(&parsed.output, "output", "", "file to write, or - for standard output")
		parsed.flags.StringVar(&parsed.output, "o", "", "file to write, or - for standard output")
		valueFlags["-output"], valueFlags["--output"], valueFlags["-o"] = true, true, true
	} else {
		parsed.flags.BoolVar(&parsed.jsonOutput, "json", false, "emit machine-readable JSON")
	}
	if err := parsed.flags.Parse(reorderInterspersed(args, valueFlags)); err != nil {
		return nil, false
	}
	return parsed, true
}

func validateApplicationVolume(application, volume string) error {
	if err := config.ValidateApplicationName(application); err != nil {
		return fmt.Errorf("invalid application name: %w", err)
	}
	if err := config.ValidateApplicationName(volume); err != nil {
		return fmt.Errorf("invalid volume name: %w", err)
	}
	return nil
}

type volumeBackupOutput struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	SizeBytes uint64    `json:"size_bytes"`
	OnNode    bool      `json:"on_node"`
	InBucket  bool      `json:"in_bucket"`
}

type volumeOutput struct {
	Application    string               `json:"application"`
	Volume         string               `json:"volume"`
	Service        string               `json:"service,omitempty"`
	Node           string               `json:"node,omitempty"`
	BackupSchedule string               `json:"backup_schedule,omitempty"`
	BackupKeep     uint32               `json:"backup_keep,omitempty"`
	Reachable      bool                 `json:"reachable"`
	BackupCount    uint32               `json:"backup_count"`
	Backups        []volumeBackupOutput `json:"backups"`
	BackupFailedAt *time.Time           `json:"backup_failed_at,omitempty"`
	BackupFailure  string               `json:"backup_failure,omitempty"`
	BucketError    string               `json:"bucket_error,omitempty"`
}

func volumeBackupJSON(backup *orchv1alpha1.VolumeBackupInfo) volumeBackupOutput {
	return volumeBackupOutput{Name: backup.GetName(), CreatedAt: backup.GetCreatedAt().AsTime().UTC(), SizeBytes: backup.GetSizeBytes(),
		OnNode: backup.GetOnNode(), InBucket: backup.GetInBucket()}
}

func volumeJSON(volume *orchv1alpha1.VolumeSummary) volumeOutput {
	result := volumeOutput{
		Application: volume.GetApplication(), Volume: volume.GetVolume(), Service: volume.GetService(), Node: volume.GetNode(),
		BackupSchedule: volume.GetBackupSchedule(), BackupKeep: volume.GetBackupKeep(), Reachable: volume.GetReachable(),
		BackupCount: volume.GetBackupCount(), Backups: []volumeBackupOutput{}, BackupFailure: volume.GetBackupFailure(),
		BucketError: volume.GetBucketError(),
	}
	for _, backup := range volume.GetBackups() {
		result.Backups = append(result.Backups, volumeBackupJSON(backup))
	}
	if value := volume.GetBackupFailedAt(); value != nil && value.CheckValid() == nil {
		failedAt := value.AsTime().UTC()
		result.BackupFailedAt = &failedAt
	}
	return result
}

func runVolumeList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	parsed, ok := parseVolumeFlags("volume list", args, stderr, false)
	if !ok {
		return ExitInvalid
	}
	if parsed.flags.NArg() > 1 || parsed.contextPath == "" {
		fmt.Fprintln(stderr, "volume list accepts an optional APP and requires --context")
		return ExitInvalid
	}
	application := parsed.flags.Arg(0)
	if application != "" {
		if err := config.ValidateApplicationName(application); err != nil {
			writeError(stderr, fmt.Errorf("invalid application name: %w", err), parsed.jsonOutput)
			return ExitInvalid
		}
	}
	remote, err := controlclient.New(parsed.contextPath)
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	volumes, err := remote.ListVolumes(ctx, application, "")
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitRuntime
	}
	if parsed.jsonOutput {
		result := make([]volumeOutput, 0, len(volumes))
		for _, volume := range volumes {
			result = append(result, volumeJSON(volume))
		}
		return writeJSON(stdout, stderr, result)
	}
	if len(volumes) == 0 {
		fmt.Fprintln(stdout, "No volumes.")
		return ExitOK
	}
	now := time.Now()
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "APP\tVOLUME\tSERVICE\tNODE\tBACKUPS")
	for _, volume := range volumes {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", volume.GetApplication(), volume.GetVolume(),
			dash(volume.GetService()), dash(volume.GetNode()), volumeBackupSummary(volume, now))
	}
	if err := table.Flush(); err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	return ExitOK
}

// volumeBackupSummary says how a volume's backups are doing in a few words.
func volumeBackupSummary(volume *orchv1alpha1.VolumeSummary, now time.Time) string {
	var parts []string
	if volume.GetBackupSchedule() != "" {
		parts = append(parts, fmt.Sprintf("%q keep %d", volume.GetBackupSchedule(), volume.GetBackupKeep()))
	} else {
		parts = append(parts, "not scheduled")
	}
	switch {
	case volume.GetNode() == "":
		parts = append(parts, "not on a node yet")
	case !volume.GetReachable() && len(volume.GetBackups()) == 0:
		parts = append(parts, "node unreachable")
	case len(volume.GetBackups()) == 0:
		parts = append(parts, "none yet")
	default:
		latest := volume.GetBackups()[0]
		age := formatAge(now.Sub(latest.GetCreatedAt().AsTime()))
		if age != "just now" {
			age += " ago"
		}
		parts = append(parts, fmt.Sprintf("%d, latest %s (%s)", volume.GetBackupCount(), latest.GetName(), age))
		if !volume.GetReachable() {
			parts = append(parts, "node unreachable")
		}
	}
	if volume.GetBackupFailure() != "" {
		parts = append(parts, "LAST BACKUP FAILED: "+volume.GetBackupFailure())
	}
	if volume.GetBucketError() != "" {
		parts = append(parts, "bucket not listed: "+volume.GetBucketError())
	}
	return strings.Join(parts, ", ")
}

func runVolumeBackups(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	parsed, ok := parseVolumeFlags("volume backups", args, stderr, false)
	if !ok {
		return ExitInvalid
	}
	if parsed.flags.NArg() != 2 || parsed.contextPath == "" {
		fmt.Fprintln(stderr, "volume backups requires APP VOLUME and --context")
		return ExitInvalid
	}
	application, name := parsed.flags.Arg(0), parsed.flags.Arg(1)
	if err := validateApplicationVolume(application, name); err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(parsed.contextPath)
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	volumes, err := remote.ListVolumes(ctx, application, name)
	if err == nil && len(volumes) != 1 {
		err = fmt.Errorf("volume %q was not found", name)
	}
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitRuntime
	}
	volume := volumes[0]
	if parsed.jsonOutput {
		return writeJSON(stdout, stderr, volumeJSON(volume))
	}
	if volume.GetNode() != "" && !volume.GetReachable() {
		if len(volume.GetBackups()) == 0 {
			writeError(stderr, fmt.Errorf("node %q that holds volume %q is not reachable", volume.GetNode(), name), false)
			return ExitRuntime
		}
		fmt.Fprintf(stdout, "Node %s is not reachable; listing the backups in the bucket.\n\n", volume.GetNode())
	}
	if volume.GetBackupFailure() != "" {
		fmt.Fprintf(stdout, "Last scheduled backup failed at %s: %s\n\n",
			volume.GetBackupFailedAt().AsTime().UTC().Format(time.RFC3339), volume.GetBackupFailure())
	}
	if volume.GetBucketError() != "" {
		fmt.Fprintf(stdout, "Could not list the copies in the bucket: %s\n\n", volume.GetBucketError())
	}
	if len(volume.GetBackups()) == 0 {
		fmt.Fprintln(stdout, "No backups yet. Take one with: orch volume backup", application, name)
		return ExitOK
	}
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "BACKUP\tCREATED\tSIZE\tSTORED")
	for _, backup := range volume.GetBackups() {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", backup.GetName(), backup.GetCreatedAt().AsTime().UTC().Format(time.RFC3339),
			formatSize(backup.GetSizeBytes()), backupLocation(backup))
	}
	if err := table.Flush(); err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	return ExitOK
}

// backupLocation says where copies of a backup are.
func backupLocation(backup *orchv1alpha1.VolumeBackupInfo) string {
	switch {
	case backup.GetOnNode() && backup.GetInBucket():
		return "node, bucket"
	case backup.GetInBucket():
		return "bucket"
	default:
		return "node"
	}
}

func runVolumeBackup(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	parsed, ok := parseVolumeFlags("volume backup", args, stderr, false)
	if !ok {
		return ExitInvalid
	}
	if parsed.flags.NArg() != 2 || parsed.contextPath == "" {
		fmt.Fprintln(stderr, "volume backup requires APP VOLUME and --context")
		return ExitInvalid
	}
	application, name := parsed.flags.Arg(0), parsed.flags.Arg(1)
	if err := validateApplicationVolume(application, name); err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(parsed.contextPath)
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	backup, err := remote.BackupVolume(ctx, application, name)
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitRuntime
	}
	if parsed.jsonOutput {
		return writeJSON(stdout, stderr, volumeBackupJSON(backup))
	}
	fmt.Fprintf(stdout, "Backed up %s/%s as %s (%s).\n", application, name, backup.GetName(), formatSize(backup.GetSizeBytes()))
	return ExitOK
}

func runVolumeRestore(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	parsed, ok := parseVolumeFlags("volume restore", args, stderr, false)
	if !ok {
		return ExitInvalid
	}
	if parsed.flags.NArg() != 3 || parsed.contextPath == "" {
		fmt.Fprintln(stderr, "volume restore requires APP VOLUME BACKUP and --context")
		return ExitInvalid
	}
	application, name, backupName := parsed.flags.Arg(0), parsed.flags.Arg(1), parsed.flags.Arg(2)
	if err := validateApplicationVolume(application, name); err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(parsed.contextPath)
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	if !parsed.jsonOutput {
		fmt.Fprintf(stderr, "Restoring %s/%s from %s; the service that mounts it restarts.\n", application, name, backupName)
	}
	previous, err := remote.RestoreVolume(ctx, application, name, backupName)
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitRuntime
	}
	if parsed.jsonOutput {
		result := struct {
			Restored string              `json:"restored"`
			Previous *volumeBackupOutput `json:"previous,omitempty"`
		}{Restored: backupName}
		if previous.GetName() != "" {
			value := volumeBackupJSON(previous)
			result.Previous = &value
		}
		return writeJSON(stdout, stderr, result)
	}
	fmt.Fprintf(stdout, "Restored %s/%s from %s.\n", application, name, backupName)
	if previous.GetName() != "" {
		fmt.Fprintf(stdout, "The previous contents are kept as %s. Undo with: orch volume restore %s %s %s\n",
			previous.GetName(), application, name, previous.GetName())
	}
	return ExitOK
}

func runVolumeDownload(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	parsed, ok := parseVolumeFlags("volume download", args, stderr, true)
	if !ok {
		return ExitInvalid
	}
	if parsed.flags.NArg() != 3 || parsed.contextPath == "" {
		fmt.Fprintln(stderr, "volume download requires APP VOLUME BACKUP and --context")
		return ExitInvalid
	}
	application, name, backupName := parsed.flags.Arg(0), parsed.flags.Arg(1), parsed.flags.Arg(2)
	if err := validateApplicationVolume(application, name); err != nil {
		writeError(stderr, err, false)
		return ExitInvalid
	}
	output := parsed.output
	if output == "" {
		output = application + "-" + name + "-" + backupName + ".tar.gz"
	}
	remote, err := controlclient.New(parsed.contextPath)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	stream, err := remote.DownloadVolumeBackup(ctx, application, name, backupName)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	defer stream.Close()
	var destination io.Writer = stdout
	var file *os.File
	var partial string
	if output != "-" {
		if _, err := os.Lstat(output); err == nil {
			writeError(stderr, fmt.Errorf("%s already exists", output), false)
			return ExitInvalid
		}
		partial = output + ".partial"
		if file, err = os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600); err != nil {
			writeError(stderr, err, false)
			return ExitRuntime
		}
		defer func() {
			if file != nil {
				_ = file.Close()
				_ = os.Remove(partial)
			}
		}()
		destination = file
	}
	var written, size uint64
	for stream.Receive() {
		frame := stream.Msg()
		size = frame.GetSizeBytes()
		if _, err := destination.Write(frame.GetData()); err != nil {
			writeError(stderr, err, false)
			return ExitRuntime
		}
		written += uint64(len(frame.GetData()))
	}
	if err := stream.Err(); err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	if written != size {
		writeError(stderr, errors.New("download ended early"), false)
		return ExitRuntime
	}
	if file == nil {
		return ExitOK
	}
	if err := file.Sync(); err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	if err := file.Close(); err != nil {
		file = nil
		_ = os.Remove(partial)
		writeError(stderr, err, false)
		return ExitRuntime
	}
	file = nil
	if err := os.Rename(partial, output); err != nil {
		_ = os.Remove(partial)
		writeError(stderr, err, false)
		return ExitRuntime
	}
	absolute, _ := filepath.Abs(output)
	fmt.Fprintf(stderr, "Wrote %s (%s).\n", absolute, formatSize(written))
	return ExitOK
}

// runRestart restarts a service, or every service of an application, one
// replica at a time.
func runRestart(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("restart", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var replica uint
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.UintVar(&replica, "replica", 0, "restart only this replica")
	if err := flags.Parse(reorderInterspersed(args, map[string]bool{"-context": true, "--context": true, "-replica": true, "--replica": true})); err != nil {
		return ExitInvalid
	}
	if flags.NArg() < 1 || flags.NArg() > 2 || contextPath == "" || replica > math.MaxUint32 {
		fmt.Fprintln(stderr, "restart requires APP [SERVICE] and --context")
		return ExitInvalid
	}
	application, service := flags.Arg(0), flags.Arg(1)
	if err := config.ValidateApplicationName(application); err != nil {
		writeError(stderr, fmt.Errorf("invalid application name: %w", err), false)
		return ExitInvalid
	}
	if service != "" {
		if err := validateApplicationService(application, service); err != nil {
			writeError(stderr, err, false)
			return ExitInvalid
		}
	} else if replica != 0 {
		fmt.Fprintln(stderr, "restart --replica requires a SERVICE")
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	target := application
	if service != "" {
		target += "/" + service
	}
	fmt.Fprintf(stderr, "Restarting %s one replica at a time...\n", target)
	if _, err := remote.RestartService(ctx, application, service, uint32(replica)); err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	fmt.Fprintf(stdout, "Restarted %s.\n", target)
	return ExitOK
}

func dash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func formatSize(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value, exponent := float64(bytes)/unit, 0
	for value >= unit && exponent < 4 {
		value /= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", value, "KMGTP"[exponent])
}

func formatAge(age time.Duration) string {
	switch {
	case age < time.Minute:
		return "just now"
	case age < time.Hour:
		return fmt.Sprintf("%dm", int(age/time.Minute))
	case age < 48*time.Hour:
		return fmt.Sprintf("%dh", int(age/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(age/(24*time.Hour)))
	}
}
