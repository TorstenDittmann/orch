package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostCommandsDoNotResolveSelectedTargets(t *testing.T) {
	for _, command := range []string{"doctor", "install"} {
		args := []string{"node", command, "--advertise-address", "10.0.0.11", "--json"}
		if command == "install" {
			args = append(args, "--dry-run", "--join-file", "/does/not/exist")
		}
		if isRemoteCommand(args) {
			t.Fatalf("%s classified as remote", command)
		}
		resolved, err := prepareConnectionArgs(args)
		if err != nil || strings.Join(resolved, " ") != strings.Join(args, " ") {
			t.Fatalf("local args changed: %v %v", resolved, err)
		}
		var out, errs bytes.Buffer
		code := RunContext(context.Background(), args, &out, &errs)
		if code == ExitInvalid || out.Len() == 0 || strings.Contains(errs.String(), "target") {
			t.Fatalf("code=%d out=%s errors=%s", code, out.String(), errs.String())
		}
	}
}

func TestNodeUpgradeDoesNotResolveSelectedTargets(t *testing.T) {
	t.Setenv("ORCH_TARGET", "missing-target")
	args := []string{"node", "upgrade", "--binary", "/does/not/exist", "--dry-run", "--json"}
	if isRemoteCommand(args) {
		t.Fatal("node upgrade classified as remote")
	}
	resolved, err := prepareConnectionArgs(args)
	if err != nil || strings.Join(resolved, " ") != strings.Join(args, " ") {
		t.Fatalf("local args changed: %v %v", resolved, err)
	}
	var out, errs bytes.Buffer
	RunContext(context.Background(), args, &out, &errs)
	if strings.Contains(errs.String(), "target") {
		t.Fatalf("node upgrade resolved a target: %s", errs.String())
	}
}

func TestControllerHostCommandsAreLocalAndDryRunDoesNotPublishOutputs(t *testing.T) {
	root := t.TempDir()
	admin := filepath.Join(root, "admin")
	recovery := filepath.Join(root, "recovery")
	args := []string{"cluster", "install", "--name", "test", "--server", "https://10.0.0.10:9443", "--listen", "0.0.0.0:9443", "--admin-dir", admin, "--recovery-dir", recovery, "--dry-run", "--json"}
	if isRemoteCommand(args) {
		t.Fatal("controller install classified as remote")
	}
	resolved, err := prepareConnectionArgs(args)
	if err != nil || strings.Join(resolved, " ") != strings.Join(args, " ") {
		t.Fatalf("local args changed: %v %v", resolved, err)
	}
	var out, errs bytes.Buffer
	code := RunContext(context.Background(), args, &out, &errs)
	if code == ExitInvalid || out.Len() == 0 || strings.Contains(errs.String(), "target") {
		t.Fatalf("code=%d out=%s errors=%s", code, out.String(), errs.String())
	}
	for _, path := range []string{admin, recovery} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("dry-run published %s", path)
		}
	}
}

func TestControllerHostCommandsRejectInvalidOrRemoteArguments(t *testing.T) {
	commands := [][]string{
		{"cluster", "doctor", "--server", "http://10.0.0.10:9443", "--listen", "0.0.0.0:9443"},
		{"cluster", "install", "--server", "https://10.0.0.10:9443", "--listen", "0.0.0.0:9443"},
		{"cluster", "doctor", "--server", "https://10.0.0.10:9443", "--listen", "0.0.0.0:9443", "--target", "prod"},
	}
	for _, args := range commands {
		var out, errs bytes.Buffer
		if code := RunContext(context.Background(), args, &out, &errs); code != ExitInvalid {
			t.Fatalf("%v code=%d out=%s errors=%s", args, code, out.String(), errs.String())
		}
	}
}
func TestHostCommandsRejectRemoteTarget(t *testing.T) {
	var out, errs bytes.Buffer
	if code := RunContext(context.Background(), []string{"node", "doctor", "--advertise-address", "10.0.0.11", "--target", "prod"}, &out, &errs); code != ExitInvalid {
		t.Fatalf("code=%d %s", code, errs.String())
	}
}
