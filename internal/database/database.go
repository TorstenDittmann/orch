// Package database reads the databases an application declares and restores
// them from backups. The CLI's orch db commands and the orch web dashboard
// share it so both restore a database the same way.
package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
	"github.com/TorstenDittmann/orch/internal/config"
)

const (
	readyTimeout = 5 * time.Minute
	readyEvery   = 2 * time.Second
	loadTimeout  = 300
)

// Database is one database of an application's current manifest.
type Database struct {
	Application    string `json:"application"`
	Database       string `json:"database"`
	Engine         string `json:"engine"`
	Version        string `json:"version"`
	URLSecret      string `json:"url_secret"`
	BackupVolume   string `json:"backup_volume,omitempty"`
	BackupSchedule string `json:"backup_schedule,omitempty"`
	BackupKeep     int    `json:"backup_keep,omitempty"`
}

// List reads the databases of an application's current manifest, sorted by
// name.
func List(ctx context.Context, remote *controlclient.Client, application string) ([]Database, error) {
	app, err := remote.GetApplication(ctx, application)
	if err != nil {
		return nil, err
	}
	var manifest config.Manifest
	if err := json.Unmarshal([]byte(app.GetCanonicalManifestJson()), &manifest); err != nil {
		return nil, fmt.Errorf("read the manifest of %s: %w", application, err)
	}
	var databases []Database
	for name, service := range manifest.Services {
		if service.Database == nil {
			continue
		}
		item := Database{
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

// Find returns one database of an application, or an error naming the
// databases it has.
func Find(ctx context.Context, remote *controlclient.Client, application, name string) (Database, error) {
	databases, err := List(ctx, remote, application)
	if err != nil {
		return Database{}, err
	}
	var names []string
	for _, database := range databases {
		if database.Database == name {
			return database, nil
		}
		names = append(names, database.Database)
	}
	if len(names) == 0 {
		return Database{}, fmt.Errorf("application %s declares no databases", application)
	}
	return Database{}, fmt.Errorf("application %s has no database %q (it has %s)", application, name, strings.Join(names, ", "))
}

// Volume returns the volume whose backups hold the database.
func (database Database) Volume() (string, error) {
	if database.BackupVolume == "" {
		return "", fmt.Errorf("backups are off for database %s; add `backup = new DatabaseBackup {}` to it in orch.pkl to turn them on", database.Database)
	}
	return database.BackupVolume, nil
}

// Restore swaps a backup into the database's backup volume, which restarts
// the database, and for dump-based engines waits for it and loads the dump.
// It returns the backup that keeps the volume's previous contents, if any.
// Progress lines go to progress.
func Restore(ctx context.Context, remote *controlclient.Client, database Database, backup string, progress io.Writer) (*orchv1alpha1.VolumeBackupInfo, error) {
	volume, err := database.Volume()
	if err != nil {
		return nil, err
	}
	application, name := database.Application, database.Database
	previous, err := remote.RestoreVolume(ctx, application, volume, backup)
	if err != nil {
		return nil, err
	}
	load := config.DatabaseLoadCommand(name, database.Engine)
	if load == nil {
		return previous, nil
	}
	fmt.Fprintln(progress, "Waiting for the database to accept connections.")
	if err := waitForDatabase(ctx, remote, application, name, config.DatabaseReadyCommand(name, database.Engine)); err != nil {
		return previous, fmt.Errorf("the backup was swapped in, but the database did not come back: %w; load it later with: orch exec %s %s -- %s",
			err, application, name, shellQuote(load))
	}
	fmt.Fprintln(progress, "Loading the dump.")
	if output, err := runCommand(ctx, remote, application, name, load, loadTimeout); err != nil {
		return previous, fmt.Errorf("loading the dump failed: %w%s", err, commandOutput(output))
	}
	return previous, nil
}

// KeepsPreviousData reports whether the backup Restore returns holds the
// database's previous data, so restoring it undoes the restore. For dump
// engines it holds only the previous dump.
func (database Database) KeepsPreviousData() bool {
	return config.DatabaseLoadCommand(database.Database, database.Engine) == nil
}

func waitForDatabase(ctx context.Context, remote *controlclient.Client, application, name string, ready []string) error {
	deadline := time.Now().Add(readyTimeout)
	var last error
	for {
		if ready == nil {
			return nil
		}
		output, err := runCommand(ctx, remote, application, name, ready, 30)
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
		case <-time.After(readyEvery):
		}
	}
}

// runCommand runs a command in the database's replica and returns its
// combined output.
func runCommand(ctx context.Context, remote *controlclient.Client, application, name string, command []string, timeout uint32) ([]byte, error) {
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
