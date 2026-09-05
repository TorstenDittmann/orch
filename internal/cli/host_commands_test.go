package cli

import (
	"bytes"
	"context"
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
func TestHostCommandsRejectRemoteTarget(t *testing.T) {
	var out, errs bytes.Buffer
	if code := RunContext(context.Background(), []string{"node", "doctor", "--advertise-address", "10.0.0.11", "--target", "prod"}, &out, &errs); code != ExitInvalid {
		t.Fatalf("code=%d %s", code, errs.String())
	}
}
