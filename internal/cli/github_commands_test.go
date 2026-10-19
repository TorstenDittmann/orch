package cli

import (
	"reflect"
	"strings"
	"testing"
)

func TestOrchClusterSignsInFromGitHubActions(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ORCH_TARGET", "")
	t.Setenv("ORCH_CLUSTER", "https://203.0.113.10:9443#sha256:abc")
	got, err := prepareConnectionArgs([]string{"deploy", "--wait"})
	want := []string{"deploy", "--wait", "--context", "github-actions:https://203.0.113.10:9443#sha256:abc"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("prepareConnectionArgs() = %q, %v; want %q", got, err, want)
	}
}

func TestGitHubTrustInstructionsAreAWorkingWorkflow(t *testing.T) {
	out := githubTrustInstructions("acme/hello", "hello", "main", "production", "https://203.0.113.10:9443#sha256:abc")
	for _, want := range []string{"id-token: write", "environment: production", "branches: [main]", "uses: TorstenDittmann/orch@", "cluster: https://203.0.113.10:9443#sha256:abc", "app: hello"} {
		if !strings.Contains(out, want) {
			t.Errorf("instructions lack %q:\n%s", want, out)
		}
	}
}
