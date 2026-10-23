package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
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
