package cli

import (
	"testing"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
)

func TestCIDeploymentSourceReadsGitHubActions(t *testing.T) {
	env := map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "acme/hello", "GITHUB_SHA": "0123456789abcdef0123456789abcdef01234567",
		"GITHUB_REF_NAME": "main", "GITHUB_ACTOR": "octocat", "GITHUB_SERVER_URL": "https://github.com",
		"GITHUB_RUN_ID": "42", "GITHUB_RUN_ATTEMPT": "2",
	}
	source := ciDeploymentSource(func(key string) string { return env[key] })
	if source.GetRunUrl() != "https://github.com/acme/hello/actions/runs/42/attempts/2" || source.GetCommit() != env["GITHUB_SHA"] {
		t.Fatalf("source = %+v", source)
	}
	if got, want := deploymentSourceSummary(source), "acme/hello@0123456789ab (main) by octocat https://github.com/acme/hello/actions/runs/42/attempts/2"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if ciDeploymentSource(func(string) string { return "" }) != nil {
		t.Fatal("source recorded outside GitHub Actions")
	}
	if deploymentSourceSummary((*orchv1alpha1.DeploymentSource)(nil)) != "" {
		t.Fatal("nil source has a summary")
	}
}
