package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
	"github.com/TorstenDittmann/orch/internal/config"
)

const (
	dbReadyTimeout = 5 * time.Minute
	dbReadyEvery   = 2 * time.Second
	dbLoadTimeout  = 300
)

func printDBHelp(output io.Writer) {
	fmt.Fprintln(output, `orch db works with the databases an application declares in orch.pkl.

Usage:
  orch db list [APP] [--target NAME] [--json]
  orch db backups APP DATABASE [--target NAME] [--json]
  orch db backup APP DATABASE [--target NAME] [--json]
  orch db restore APP DATABASE [BACKUP] [--target NAME] [--json]

Applications connect with the NAME-url secret orch creates for each database.
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

type dbOutput struct {
	Application    string `json:"application"`
	Database       string `json:"database"`
	Engine         string `json:"engine"`
	Version        string `json:"version"`
	URLSecret      string `json:"url_secret"`
	BackupVolume   string `json:"backup_volume,omitempty"`
	BackupSchedule string `json:"backup_schedule,omitempty"`
	BackupKeep     int    `json:"backup_keep,omitempty"`
}

// applicationDatabases reads the databases of an application's current
// manifest, sorted by name.
func applicationDatabases(ctx context.Context, remote *controlclient.Client, application string) ([]dbOutput, error) {
	app, err := remote.GetApplication(ctx, application)
	if err != nil {
		return nil, err
	}
	var manifest config.Manifest
	if err := json.Unmarshal([]byte(app.GetCanonicalManifestJson()), &manifest); err != nil {
		return nil, fmt.Errorf("read the manifest of %s: %w", application, err)
	}
	var databases []dbOutput
	for name, service := range manifest.Services {
		if service.Database == nil {
			continue
		}
		item := dbOutput{
			Application: application, Database: name, Engine: service.Database.Engine, Version: service.Database.Version,
			URLSecret: name + "-url",
		}
		volume := config.DatabaseBackupVolume(name, service.Database.Engine)
		if backup := manifest.Volumes[volume].Backup; backup != nil {
			item.BackupVolume, item.BackupSchedule, item.BackupKeep = volume, backup.Schedule, backup.Keep
		}
		databases = append(databases, item)
	}
	sort.Slice(databases, func(left, right int) bool { return databases[left].Database < databases[right].Database })
	return databases, nil
}

// findDatabase returns one database of an application, or an error naming
// the databases it has.
func findDatabase(ctx context.Context, remote *controlclient.Client, application, name string) (dbOutput, error) {
	databases, err := applicationDatabases(ctx, remote, application)
	if err != nil {
		return dbOutput{}, err
	}
	var names []string
	for _, database := range databases {
		if database.Database == name {
			return database, nil
		}
		names = append(names, database.Database)
	}
	if len(names) == 0 {
		return dbOutput{}, fmt.Errorf("application %s declares no databases", application)
	}
	return dbOutput{}, fmt.Errorf("application %s has no database %q (it has %s)", application, name, strings.Join(names, ", "))
}

func backupVolumeOf(database dbOutput) (string, error) {
	if database.BackupVolume == "" {
		return "", fmt.Errorf("backups are off for database %s; add `backup = new DatabaseBackup {}` to it in orch.pkl to turn them on", database.Database)
	}
	return database.BackupVolume, nil
}

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
		found, err := applicationDatabases(ctx, remote, application)
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
	database, err := findDatabase(ctx, remote, application, databaseName)
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return nil, ExitRuntime
	}
	volume, err := backupVolumeOf(database)
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
	database, err := findDatabase(ctx, remote, application, name)
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitRuntime
	}
	volume, err := backupVolumeOf(database)
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitInvalid
	}
	if backupName == "" {
		volumes, err := remote.ListVolumes(ctx, application, volume)
		if err == nil && (len(volumes) != 1 || len(volumes[0].GetBackups()) == 0) {
			err = fmt.Errorf("database %s has no backups yet", name)
		}
		if err != nil {
			writeError(stderr, err, parsed.jsonOutput)
			return ExitRuntime
		}
		backupName = latestScheduledBackup(volumes[0].GetBackups())
	}
	progress := stderr
	if parsed.jsonOutput {
		progress = io.Discard
	}
	fmt.Fprintf(progress, "Restoring database %s/%s from %s; it restarts.\n", application, name, backupName)
	previous, err := remote.RestoreVolume(ctx, application, volume, backupName)
	if err != nil {
		writeError(stderr, err, parsed.jsonOutput)
		return ExitRuntime
	}
	if load := config.DatabaseLoadCommand(name, database.Engine); load != nil {
		fmt.Fprintln(progress, "Waiting for the database to accept connections.")
		if err := waitForDatabase(ctx, remote, application, name, config.DatabaseReadyCommand(name, database.Engine)); err != nil {
			writeError(stderr, fmt.Errorf("the backup was swapped in, but the database did not come back: %w; load it later with: orch exec %s %s -- %s",
				err, application, name, shellQuote(load)), parsed.jsonOutput)
			return ExitRuntime
		}
		fmt.Fprintln(progress, "Loading the dump.")
		if output, err := runDatabaseCommand(ctx, remote, application, name, load, dbLoadTimeout); err != nil {
			writeError(stderr, fmt.Errorf("loading the dump failed: %w%s", err, commandOutput(output)), parsed.jsonOutput)
			return ExitRuntime
		}
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
	if previous.GetName() != "" && config.DatabaseLoadCommand(name, database.Engine) == nil {
		fmt.Fprintf(stdout, "The previous data is kept as %s. Undo with: orch db restore %s %s %s\n", previous.GetName(), application, name, previous.GetName())
	}
	return ExitOK
}

// latestScheduledBackup picks the newest backup, preferring scheduled and
// manual ones over the copies a restore keeps of the data it replaced.
func latestScheduledBackup(backups []*orchv1alpha1.VolumeBackupInfo) string {
	for _, backup := range backups {
		if !strings.HasSuffix(backup.GetName(), "-before-restore") {
			return backup.GetName()
		}
	}
	return backups[0].GetName()
}

func waitForDatabase(ctx context.Context, remote *controlclient.Client, application, name string, ready []string) error {
	deadline := time.Now().Add(dbReadyTimeout)
	var last error
	for {
		if ready == nil {
			return nil
		}
		output, err := runDatabaseCommand(ctx, remote, application, name, ready, 30)
		if err == nil {
			return nil
		}
		last = fmt.Errorf("%w%s", err, commandOutput(output))
		if time.Now().After(deadline) {
			return last
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dbReadyEvery):
		}
	}
}

// runDatabaseCommand runs a command in the database's replica and returns
// its combined output.
func runDatabaseCommand(ctx context.Context, remote *controlclient.Client, application, name string, command []string, timeout uint32) ([]byte, error) {
	stream, err := remote.Exec(ctx, application, name, 1, command, timeout)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	var output []byte
	exited, exitCode := false, int32(0)
	for stream.Receive() {
		frame := stream.Msg()
		output = append(output, frame.GetData()...)
		if len(output) > 4096 {
			output = output[len(output)-4096:]
		}
		if frame.GetExited() {
			exited, exitCode = true, frame.GetExitCode()
		}
	}
	if err := stream.Err(); err != nil {
		return output, err
	}
	if !exited {
		return output, errors.New("the command ended without an exit status")
	}
	if exitCode != 0 {
		return output, fmt.Errorf("the command exited with status %d", exitCode)
	}
	return output, nil
}

func commandOutput(output []byte) string {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return ""
	}
	return ": " + text
}

func shellQuote(command []string) string {
	quoted := make([]string, len(command))
	for index, argument := range command {
		if argument != "" && strings.Trim(argument, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=") == "" {
			quoted[index] = argument
			continue
		}
		quoted[index] = "'" + strings.ReplaceAll(argument, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}
