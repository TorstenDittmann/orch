package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/distribution/reference"

	controlclient "github.com/TorstenDittmann/orch/internal/client"
	"github.com/TorstenDittmann/orch/internal/config"
)

const githubWorkflowPath = ".github/workflows/deploy.yml"

// githubWorkflow is the deploy workflow orch github init writes and orch
// github trust prints.
type githubWorkflow struct {
	Application string
	Branch      string
	Environment string
	Cluster     string
	// Image is the repository the workflow builds and pushes; empty deploys
	// the manifest's images as they are.
	Image string
	// Context and File are relative to the repository root.
	Context string
	File    string
	// RunsOn and Platforms match the build to the cluster's node
	// architectures; empty means a native ubuntu-latest (amd64) build.
	RunsOn    string
	Platforms string
}

func (w githubWorkflow) render() string {
	var out strings.Builder
	fmt.Fprintf(&out, "name: deploy\non:\n  push:\n    branches: [%s]\n  workflow_dispatch:\n", w.Branch)
	out.WriteString("permissions:\n  contents: read\n")
	if strings.HasPrefix(strings.ToLower(w.Image), "ghcr.io/") {
		out.WriteString("  packages: write\n")
	}
	out.WriteString("  id-token: write\n")
	fmt.Fprintf(&out, "concurrency:\n  group: deploy-%s\n  cancel-in-progress: false\n", w.Application)
	runsOn := w.RunsOn
	if runsOn == "" {
		runsOn = "ubuntu-latest"
	}
	fmt.Fprintf(&out, "jobs:\n  deploy:\n    runs-on: %s\n", runsOn)
	if w.Environment != "" {
		fmt.Fprintf(&out, "    environment: %s\n", w.Environment)
	}
	out.WriteString("    steps:\n      - uses: actions/checkout@v4\n")
	fmt.Fprintf(&out, "      - uses: TorstenDittmann/orch@%s\n        with:\n", githubActionRef(Version))
	fmt.Fprintf(&out, "          cluster: %s\n          app: %s\n", w.Cluster, w.Application)
	if w.Image != "" {
		fmt.Fprintf(&out, "          image: %s\n", w.Image)
		if w.Context != "" && w.Context != "." {
			fmt.Fprintf(&out, "          context: %s\n", w.Context)
		}
		if w.Platforms != "" {
			fmt.Fprintf(&out, "          platforms: %s\n", w.Platforms)
		}
	}
	if w.File != "" && w.File != "orch.pkl" {
		fmt.Fprintf(&out, "          file: %s\n", w.File)
	}
	return out.String()
}

// githubActionRef pins the action to this CLI's release, so the workflow
// installs the orch it was written for. Development builds use main.
func githubActionRef(version string) string {
	if version == "" || strings.Contains(version, "-") {
		return "main"
	}
	return "v" + strings.TrimPrefix(version, "v")
}

func runGitHubInit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("github init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath, application, branch, environment, image, file, repository string
	var force, jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.StringVar(&application, "app", "", "application to deploy (default: the manifest's name)")
	flags.StringVar(&branch, "branch", "", "branch whose pushes deploy (default: the repository's default branch)")
	flags.StringVar(&environment, "environment", "", "GitHub environment the deploy job uses (optional)")
	flags.StringVar(&image, "image", "", "image repository to build and push (default: the manifest's image when a Dockerfile sits next to it)")
	flags.StringVar(&file, "file", "orch.pkl", "Pkl manifest path")
	flags.StringVar(&file, "f", "orch.pkl", "Pkl manifest path")
	flags.StringVar(&repository, "repo", "", "GitHub repository OWNER/REPO (default: from the origin remote)")
	flags.BoolVar(&force, "force", false, "replace an existing "+githubWorkflowPath)
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	valueFlags := map[string]bool{"--context": true, "-context": true, "--app": true, "--branch": true, "--environment": true, "--image": true, "--file": true, "-f": true, "--repo": true}
	if err := flags.Parse(reorderInterspersed(args, valueFlags)); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 || contextPath == "" {
		writeError(stderr, errors.New("github init takes no arguments and needs a target"), jsonOutput)
		return ExitInvalid
	}
	fail := func(err error) int {
		writeError(stderr, err, jsonOutput)
		return ExitInvalid
	}

	root, err := gitOutput(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return fail(errors.New("run orch github init inside the repository's git checkout"))
	}
	if repository == "" {
		remote, err := gitOutput(ctx, "remote", "get-url", "origin")
		if err != nil {
			return fail(errors.New("the checkout has no origin remote; pass --repo OWNER/REPO"))
		}
		if repository = githubRepositoryFromRemote(remote); repository == "" {
			return fail(fmt.Errorf("origin %s is not a GitHub repository; pass --repo OWNER/REPO", remote))
		}
	}
	if branch == "" {
		branch = defaultGitBranch(ctx)
	}

	manifestPath, err := filepath.Abs(file)
	if err != nil {
		return fail(err)
	}
	manifest, loadErr := config.Load(ctx, manifestPath, config.LoadOptions{})
	if application == "" {
		if loadErr != nil {
			return fail(fmt.Errorf("read the application name from %s: %w; or pass --app", file, loadErr))
		}
		application = manifest.Name
	}
	if image == "" && loadErr == nil {
		image = buildableImage(manifest, filepath.Dir(manifestPath))
	}
	workflow := githubWorkflow{Application: application, Branch: branch, Environment: environment, Image: strings.ToLower(image)}
	if workflow.File, err = repositoryPath(root, manifestPath); err != nil {
		return fail(err)
	}
	if workflow.Context, err = repositoryPath(root, filepath.Dir(manifestPath)); err != nil {
		return fail(err)
	}

	target := filepath.Join(root, filepath.FromSlash(githubWorkflowPath))
	existing, readErr := os.ReadFile(target)
	if readErr == nil && !force {
		return fail(fmt.Errorf("%s already exists; pass --force to replace it", githubWorkflowPath))
	}

	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	response, err := remote.SetGitHubTrust(ctx, repository, application, branch, environment)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	workflow.Cluster = controlclient.ClusterAddress(response.GetControllerEndpoint(), response.GetCaFingerprint())
	if workflow.Image != "" {
		if nodes, err := remote.ListNodes(ctx); err == nil {
			architectures := make([]string, 0, len(nodes))
			for _, node := range nodes {
				architectures = append(architectures, node.GetArchitecture())
			}
			workflow.RunsOn, workflow.Platforms = buildTarget(architectures)
		}
	}
	content := []byte(workflow.render())
	if readErr != nil || !bytes.Equal(existing, content) {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		if err := os.WriteFile(target, content, 0o644); err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
	}

	trust := response.GetTrust()
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			Repository  string `json:"repository"`
			Application string `json:"application"`
			Branch      string `json:"branch"`
			Environment string `json:"environment,omitempty"`
			Image       string `json:"image,omitempty"`
			Cluster     string `json:"cluster"`
			Workflow    string `json:"workflow"`
		}{trust.GetRepository(), trust.GetApplication(), trust.GetBranch(), trust.GetEnvironment(), workflow.Image, workflow.Cluster, target})
	}
	fmt.Fprintf(stdout, "Trusted %s to deploy %s from branch %s.\n", trust.GetRepository(), trust.GetApplication(), trust.GetBranch())
	if workflow.Image != "" {
		fmt.Fprintf(stdout, "Wrote %s: every push to %s builds %s and deploys it.\n", githubWorkflowPath, branch, workflow.Image)
	} else {
		fmt.Fprintf(stdout, "Wrote %s: every push to %s deploys %s as written.\n", githubWorkflowPath, branch, workflow.File)
		fmt.Fprintln(stdout, "Pass --image REPOSITORY to build and push an image first.")
	}
	fmt.Fprintln(stdout, "\nCommit and push it to deploy. No secrets are needed.")
	if strings.HasPrefix(workflow.Image, "ghcr.io/") {
		fmt.Fprintln(stdout, "If the package is private, let the cluster pull it once with:")
		fmt.Fprintln(stdout, "  orch registry login ghcr.io --username USER --password-stdin")
	}
	return ExitOK
}

// buildTarget picks where the workflow builds so every node can run the image:
// an arm64 runner for an all-arm64 cluster, and a two-platform image when
// amd64 and arm64 nodes are mixed. Otherwise the default amd64 runner fits.
func buildTarget(architectures []string) (runsOn, platforms string) {
	var amd64, arm64 bool
	for _, architecture := range architectures {
		amd64 = amd64 || architecture == "amd64"
		arm64 = arm64 || architecture == "arm64"
	}
	switch {
	case arm64 && amd64:
		return "", "linux/amd64,linux/arm64"
	case arm64:
		return "ubuntu-24.04-arm", ""
	}
	return "", ""
}

func gitOutput(ctx context.Context, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, "git", args...).Output()
	return strings.TrimSpace(string(output)), err
}

var githubRemotePattern = regexp.MustCompile(`^(?:https://(?:[^@/]+@)?github\.com/|ssh://git@github\.com/|git@github\.com:)([A-Za-z0-9-]+/[A-Za-z0-9._-]+?)(?:\.git)?/?$`)

// githubRepositoryFromRemote returns OWNER/REPO for a GitHub remote URL.
func githubRepositoryFromRemote(remote string) string {
	match := githubRemotePattern.FindStringSubmatch(strings.TrimSpace(remote))
	if match == nil {
		return ""
	}
	return strings.ToLower(match[1])
}

func defaultGitBranch(ctx context.Context) string {
	head, err := gitOutput(ctx, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err == nil {
		if branch, ok := strings.CutPrefix(head, "origin/"); ok && branch != "" {
			return branch
		}
	}
	return "main"
}

// buildableImage is the one repository the manifest's workloads use, when
// a Dockerfile next to the manifest can build it.
func buildableImage(manifest *config.Manifest, dir string) string {
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil {
		return ""
	}
	repositories := map[string]bool{}
	add := func(image string) {
		if named, err := reference.ParseNormalizedNamed(image); err == nil {
			repositories[named.Name()] = true
		}
	}
	for _, service := range manifest.Services {
		add(service.Image)
	}
	for _, job := range manifest.CronJobs {
		add(job.Image)
	}
	if len(repositories) != 1 {
		return ""
	}
	for repository := range repositories {
		return repository
	}
	return ""
}

func repositoryPath(root, path string) (string, error) {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	} else if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		path = filepath.Join(dir, filepath.Base(path))
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the repository", path)
	}
	return filepath.ToSlash(relative), nil
}
