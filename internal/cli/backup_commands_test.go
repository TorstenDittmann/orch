package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestBackupCredentialsFromEnvironmentOrInput(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "env-id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "env-secret")
	id, secret, err := backupCredentials(withCommandInput(context.Background(), strings.NewReader("")), &bytes.Buffer{})
	if err != nil || id != "env-id" || secret != "env-secret" {
		t.Fatalf("backupCredentials() from env = %q, %q, %v", id, secret, err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	id, secret, err = backupCredentials(withCommandInput(context.Background(), strings.NewReader("piped-id\npiped-secret\n")), &bytes.Buffer{})
	if err != nil || id != "piped-id" || secret != "piped-secret" {
		t.Fatalf("backupCredentials() from input = %q, %q, %v", id, secret, err)
	}
	if _, _, err := backupCredentials(withCommandInput(context.Background(), strings.NewReader("")), &bytes.Buffer{}); err == nil {
		t.Fatal("backupCredentials() accepted empty input")
	}
}

func TestBackupCommandRejectsUnknownActions(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runBackup(context.Background(), []string{"destination", "upload"}, &stdout, &stderr); code != ExitInvalid {
		t.Fatalf("code = %d", code)
	}
	if code := runBackup(context.Background(), []string{"destination", "set", "--context", "x"}, &stdout, &stderr); code != ExitInvalid {
		t.Fatalf("set without a URL code = %d", code)
	}
}

func TestBackupDestinationActionAllowsLeadingFlags(t *testing.T) {
	for _, test := range []struct {
		args   []string
		action string
		rest   string
	}{
		{[]string{"set", "s3://b/p", "--context", "c"}, "set", "s3://b/p --context c"},
		{[]string{"--context", "set", "--json", "set", "s3://b/p"}, "set", "--context set --json s3://b/p"},
		{[]string{"--endpoint", "https://x", "remove"}, "remove", "--endpoint https://x"},
		{[]string{"--context", "c", "--json"}, "show", "--context c --json"},
	} {
		action, rest := backupDestinationAction(test.args)
		if action != test.action || strings.Join(rest, " ") != test.rest {
			t.Fatalf("backupDestinationAction(%q) = %q, %q", test.args, action, rest)
		}
	}
}

func TestLatestScheduledBackupAsksAfterAMove(t *testing.T) {
	at := func(name string) *orchv1alpha1.VolumeBackupInfo {
		created, _ := time.Parse("20060102T150405Z", name[:16])
		return &orchv1alpha1.VolumeBackupInfo{Name: name, CreatedAt: timestamppb.New(created)}
	}
	moved := &orchv1alpha1.VolumeSummary{Node: "node-2", ClaimedAt: timestamppb.New(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)),
		Backups: []*orchv1alpha1.VolumeBackupInfo{at("20261019T030000Z"), at("20261018T130000Z-before-restore"), at("20261017T030000Z")}}
	if _, err := latestScheduledBackup(moved); err == nil || !strings.Contains(err.Error(), "20261017T030000Z") {
		t.Fatalf("latestScheduledBackup() after a move = %v", err)
	}
	moved.Backups = moved.Backups[2:]
	if name, err := latestScheduledBackup(moved); err != nil || name != "20261017T030000Z" {
		t.Fatalf("latestScheduledBackup() with only older backups = %q, %v", name, err)
	}
	settled := &orchv1alpha1.VolumeSummary{ClaimedAt: timestamppb.New(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)),
		Backups: []*orchv1alpha1.VolumeBackupInfo{at("20261019T030000Z-before-restore"), at("20261018T030000Z")}}
	if name, err := latestScheduledBackup(settled); err != nil || name != "20261018T030000Z" {
		t.Fatalf("latestScheduledBackup() = %q, %v", name, err)
	}
}
