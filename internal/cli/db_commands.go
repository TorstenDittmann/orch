package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/database"
)

func printDBHelp(output io.Writer) {
	fmt.Fprintln(output, `orch db works with the databases an application declares in orch.pkl.

Usage:
  orch db list [APP] [--target NAME] [--json]
  orch db backups APP DATABASE [--target NAME] [--json]
  orch db backup APP DATABASE [--target NAME] [--json]
  orch db restore APP DATABASE [BACKUP] [--target NAME] [--json]

Services and cron jobs reach a database with connect in orch.pkl, which reads
the NAME-url secret orch creates for it.
Restore uses the newest backup unless BACKUP is given. It swaps in the backup,
restarts the database, and for dump-based engines loads the dump into it. The
database's previous backup volume contents are kept as a backup first.`)
}

func runDB(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printDBHelp(stderr)
		return ExitInvalid
	}
	switch args[0] {
	case "list":
		return runDBList(ctx, args[1:], stdout, stderr)
	case "backups":
		return runDBBackups(ctx, args[1:], stdout, stderr)
	case "backup":
		return runDBBackup(ctx, args[1:], stdout, stderr)
	case "restore":
		return runDBRestore(ctx, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printDBHelp(stdout)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "unknown db command %q\n\n", args[0])
		printDBHelp(stderr)
		return ExitInvalid
	}
}

type dbOutput = database.Database

func validateApplicationDatabase(application, database string) error {
	if err := config.ValidateApplicationName(application); err != nil {
		return fmt.Errorf("invalid application name: %w", err)
	}
	if err := config.ValidateApplicationName(database); err != nil {
		return fmt.Errorf("invalid database name: %w", err)
	}
	return nil
}

func runDBList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	parsed, ok := parseVolumeFlags("db list", args, stderr, false)
	if !ok {
		return ExitInvalid
	}
	if parsed.flags.NArg() > 1 || parsed.contextPath == "" {
		fmt.Fprintln(stderr, "db list accepts an optional APP and requires --context")
		return ExitInvalid
	}
	remote, err := controlclient.New(parsed.contextPath)
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	var applications []string
	if application := parsed.flags.Arg(0); application != "" {
		if err := config.ValidateApplicationName(application); err != nil {
			writeError(stderr, fmt.Errorf("invalid application name: %w", err), parsed.jsonOutput)
			return ExitInvalid
		}
		applications = []string{application}
	} else {
		summaries, err := remote.ListApplications(ctx)
		if err != nil {
			writeError(stderr, err, parsed.jsonOutput)
			return ExitRuntime
		}
		for _, summary := range summaries {
			applications = append(applications, summary.GetName())
		}
	}
	databases := []dbOutput{}
	for _, application := range applications {
		found, err := database.List(ctx, remote, application)
		if err != nil {
			writeError(stderr, err, parsed.jsonOutput)
			return ExitRuntime
		}
		databases = append(databases, found...)
	}
	if parsed.jsonOutput {
		return writeJSON(stdout, stderr, databases)
	}
	if len(databases) == 0 {
		fmt.Fprintln(stdout, "No databases.")
		return ExitOK
	}
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "APP\tDATABASE\tENGINE\tURL SECRET\tBACKUPS")
	for _, database := range databases {
		backups := "off"
		if database.BackupVolume != "" {
			backups = fmt.Sprintf("%q keep %d", database.BackupSchedule, database.BackupKeep)
		}
		fmt.Fprintf(table, "%s\t%s\t%s %s\t%s\t%s\n", database.Application, database.Database,
			database.Engine, database.Version, database.URLSecret, backups)
	}
	if err := table.Flush(); err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	return ExitOK
}

// dbVolumeArgs resolves APP DATABASE to the database's backup volume and
// returns the arguments for the matching volume command.
func dbVolumeArgs(ctx context.Context, name string, args []string, stderr io.Writer) ([]string, int) {
	parsed, ok := parseVolumeFlags("db "+name, args, stderr, false)
	if !ok {
		return nil, ExitInvalid
	}
	if parsed.flags.NArg() != 2 || parsed.contextPath == "" {
		fmt.Fprintf(stderr, "db %s requires APP DATABASE and --context\n", name)
		return nil, ExitInvalid
	}
	application, databaseName := parsed.flags.Arg(0), parsed.flags.Arg(1)
	if err := validateApplicationDatabase(application, databaseName); err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return nil, ExitInvalid
	}
	remote, err := controlclient.New(parsed.contextPath)
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return nil, ExitRuntime
	}
	defer remote.CloseIdleConnections()
	found, err := database.Find(ctx, remote, application, databaseName)
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return nil, ExitRuntime
	}
	volume, err := found.Volume()
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return nil, ExitInvalid
	}
	result := []string{application, volume, "--context", parsed.contextPath}
	if parsed.jsonOutput {
		result = append(result, "--json")
	}
	return result, ExitOK
}

func runDBBackups(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	volumeArgs, code := dbVolumeArgs(ctx, "backups", args, stderr)
	if code != ExitOK {
		return code
	}
	return runVolumeBackups(ctx, volumeArgs, stdout, stderr)
}

func runDBBackup(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	volumeArgs, code := dbVolumeArgs(ctx, "backup", args, stderr)
	if code != ExitOK {
		return code
	}
	return runVolumeBackup(ctx, volumeArgs, stdout, stderr)
}

func runDBRestore(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	parsed, ok := parseVolumeFlags("db restore", args, stderr, false)
	if !ok {
		return ExitInvalid
	}
	if parsed.flags.NArg() < 2 || parsed.flags.NArg() > 3 || parsed.contextPath == "" {
		fmt.Fprintln(stderr, "db restore requires APP DATABASE, an optional BACKUP, and --context")
		return ExitInvalid
	}
	application, name, backupName := parsed.flags.Arg(0), parsed.flags.Arg(1), parsed.flags.Arg(2)
	if err := validateApplicationDatabase(application, name); err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(parsed.contextPath)
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	found, err := database.Find(ctx, remote, application, name)
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitRuntime
	}
	volume, err := found.Volume()
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitInvalid
	}
	if backupName == "" {
		volumes, err := remote.ListVolumes(ctx, application, volume)
		if err == nil && len(volumes) != 1 {
			err = fmt.Errorf("database %s has no backups yet", name)
		}
		if err == nil && len(volumes[0].GetBackups()) == 0 && volumes[0].GetBucketError() == "" {
			err = fmt.Errorf("database %s has no backups yet", name)
		}
		if err != nil {
			writeError(stderr, err, parsed.jsonOutput)
			return ExitRuntime
		}
		if backupName, err = latestScheduledBackup(volumes[0]); err != nil {
			writeError(stderr, err, parsed.jsonOutput)
			return ExitRuntime
		}
	}
	progress := stderr
	if parsed.jsonOutput {
		progress = io.Discard
	}
	fmt.Fprintf(progress, "Restoring database %s/%s from %s; it restarts.\n", application, name, backupName)
	previous, err := database.Restore(ctx, remote, found, backupName, progress)
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
	fmt.Fprintf(stdout, "Restored database %s/%s from %s.\n", application, name, backupName)
	if previous.GetName() != "" && found.KeepsPreviousData() {
		fmt.Fprintf(stdout, "The previous data is kept as %s. Undo with: orch db restore %s %s %s\n", previous.GetName(), application, name, previous.GetName())
	}
	return ExitOK
}

// latestScheduledBackup picks the backup a restore without a name takes: the
// newest, preferring scheduled and manual ones over the copies a restore
// keeps of the data it replaced. When the volume moved off a lost node and
// the newest backup since was not taken after a restore, it may hold the new
// node's empty database, so the choice is left to the user. So it is when the bucket,
// which keeps the lost node's backups, could not be listed.
func latestScheduledBackup(volume *orchv1alpha1.VolumeSummary) (string, error) {
	backups := volume.GetBackups()
	if volume.GetBucketError() != "" {
		return "", fmt.Errorf("the bucket's backups could not be listed (%s); name the backup to restore", volume.GetBucketError())
	}
	if claimed := volume.GetClaimedAt(); claimed != nil && claimed.CheckValid() == nil {
		movedAt := claimed.AsTime().Truncate(time.Second)
		var before, newest *orchv1alpha1.VolumeBackupInfo
		var restoredAt time.Time
		for _, backup := range backups {
			created := backup.GetCreatedAt().AsTime()
			switch {
			case strings.HasSuffix(backup.GetName(), "-before-restore"):
				if !created.Before(movedAt) && created.After(restoredAt) {
					restoredAt = created
				}
			case newest == nil:
				newest = backup
				if created.Before(movedAt) {
					before = backup
				}
			case before == nil && created.Before(movedAt):
				before = backup
			}
		}
		// The newest is trusted when it is from before the move, or was taken
		// after a restore since the move put data back.
		if before != nil && newest != before && (restoredAt.IsZero() || !newest.GetCreatedAt().AsTime().After(restoredAt)) {
			return "", fmt.Errorf("the volume moved to node %s at %s; name the backup to restore: %s is the newest from before the move, %s the newest since",
				volume.GetNode(), claimed.AsTime().UTC().Format(time.RFC3339), before.GetName(), newest.GetName())
		}
	}
	for _, backup := range backups {
		if !strings.HasSuffix(backup.GetName(), "-before-restore") {
			return backup.GetName(), nil
		}
	}
	return backups[0].GetName(), nil
}
