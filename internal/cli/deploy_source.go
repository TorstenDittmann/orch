package cli

import (
	"strings"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
)

// stringList collects a repeatable string flag.
type stringList []string

func (list *stringList) String() string { return strings.Join(*list, ",") }

func (list *stringList) Set(value string) error {
	*list = append(*list, value)
	return nil
}

// ciDeploymentSource describes the GitHub Actions run that is deploying, so
// history shows which commit and run produced each deployment. Outside GitHub
// Actions it returns nil.
func ciDeploymentSource(getenv func(string) string) *orchv1alpha1.DeploymentSource {
	if getenv("GITHUB_ACTIONS") != "true" {
		return nil
	}
	source := &orchv1alpha1.DeploymentSource{
		Repository: getenv("GITHUB_REPOSITORY"),
		Commit:     getenv("GITHUB_SHA"),
		Ref:        getenv("GITHUB_REF_NAME"),
		Actor:      getenv("GITHUB_ACTOR"),
	}
	server, runID := strings.TrimSuffix(getenv("GITHUB_SERVER_URL"), "/"), getenv("GITHUB_RUN_ID")
	if strings.HasPrefix(server, "https://") && source.Repository != "" && runID != "" {
		source.RunUrl = server + "/" + source.Repository + "/actions/runs/" + runID
		if attempt := getenv("GITHUB_RUN_ATTEMPT"); attempt != "" && attempt != "1" {
			source.RunUrl += "/attempts/" + attempt
		}
	}
	return source
}

// deploymentSourceSummary renders a source as one line, such as
// "owner/repo@abc1234 (main) by octocat https://github.com/...".
func deploymentSourceSummary(source *orchv1alpha1.DeploymentSource) string {
	if source == nil {
		return ""
	}
	parts := []string{}
	origin := source.GetRepository()
	if commit := source.GetCommit(); commit != "" {
		if len(commit) > 12 {
			commit = commit[:12]
		}
		origin = strings.TrimPrefix(origin+"@"+commit, "@")
	}
	if origin != "" {
		parts = append(parts, origin)
	}
	if ref := source.GetRef(); ref != "" {
		parts = append(parts, "("+ref+")")
	}
	if actor := source.GetActor(); actor != "" {
		parts = append(parts, "by "+actor)
	}
	if run := source.GetRunUrl(); run != "" {
		parts = append(parts, run)
	}
	return strings.Join(parts, " ")
}
