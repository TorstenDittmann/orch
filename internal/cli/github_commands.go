package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	controlclient "github.com/TorstenDittmann/orch/internal/client"
)

func runGitHub(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printGitHubHelp(stdout)
		return ExitOK
	}
	command := args[0]
	flags := flag.NewFlagSet("github "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath, application, branch, environment string
	var jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	valueFlags := map[string]bool{"--context": true, "-context": true}
	positional := 0
	switch command {
	case "trust":
		flags.StringVar(&application, "app", "", "application the repository may deploy")
		flags.StringVar(&branch, "branch", "main", "branch whose runs may deploy")
		flags.StringVar(&environment, "environment", "", "GitHub environment the run must use (optional)")
		valueFlags["--app"], valueFlags["--branch"], valueFlags["--environment"] = true, true, true
		positional = 1
	case "untrust":
		flags.StringVar(&application, "app", "", "application to stop trusting the repository for")
		valueFlags["--app"] = true
		positional = 1
	case "init":
		return runGitHubInit(ctx, args[1:], stdout, stderr)
	case "list":
	default:
		fmt.Fprintf(stderr, "unknown github command %q\n\n", command)
		printGitHubHelp(stderr)
		return ExitInvalid
	}
	if err := flags.Parse(reorderInterspersed(args[1:], valueFlags)); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != positional || contextPath == "" || (positional == 1 && application == "") {
		usage := map[string]string{"trust": "OWNER/REPO --app APP", "untrust": "OWNER/REPO --app APP", "list": "no arguments"}[command]
		writeError(stderr, fmt.Errorf("github %s requires %s", command, usage), jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	switch command {
	case "trust":
		response, err := remote.SetGitHubTrust(ctx, flags.Arg(0), application, branch, environment)
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		address := controlclient.ClusterAddress(response.GetControllerEndpoint(), response.GetCaFingerprint())
		trust := response.GetTrust()
		if jsonOutput {
			return writeJSON(stdout, stderr, struct {
				Repository  string `json:"repository"`
				Application string `json:"application"`
				Branch      string `json:"branch"`
				Environment string `json:"environment,omitempty"`
				Changed     bool   `json:"changed"`
				Cluster     string `json:"cluster"`
			}{trust.GetRepository(), trust.GetApplication(), trust.GetBranch(), trust.GetEnvironment(), response.GetChanged(), address})
		}
		fmt.Fprint(stdout, githubTrustInstructions(trust.GetRepository(), trust.GetApplication(), trust.GetBranch(), trust.GetEnvironment(), address))
	case "list":
		trusts, err := remote.ListGitHubTrusts(ctx)
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		if jsonOutput {
			return writeJSON(stdout, stderr, struct {
				Trusts any `json:"trusts"`
			}{trusts})
		}
		if len(trusts) == 0 {
			fmt.Fprintln(stdout, "No GitHub repositories are trusted. Add one with orch github trust OWNER/REPO --app APP.")
		}
		for _, trust := range trusts {
			line := fmt.Sprintf("%s  app %s  branch %s", trust.GetRepository(), trust.GetApplication(), trust.GetBranch())
			if trust.GetEnvironment() != "" {
				line += "  environment " + trust.GetEnvironment()
			}
			fmt.Fprintln(stdout, line)
		}
	case "untrust":
		changed, err := remote.DeleteGitHubTrust(ctx, flags.Arg(0), application)
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		if jsonOutput {
			return writeJSON(stdout, stderr, struct {
				Changed bool `json:"changed"`
			}{changed})
		}
		if !changed {
			writeError(stderr, errors.New("no such GitHub trust"), false)
			return ExitInvalid
		}
		fmt.Fprintf(stdout, "%s can no longer deploy %s. Credentials already issued expire within 15 minutes.\n", strings.ToLower(flags.Arg(0)), application)
	}
	return ExitOK
}

func githubTrustInstructions(repository, application, branch, environment, address string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Trusted %s to deploy %s from branch %s", repository, application, branch)
	if environment != "" {
		fmt.Fprintf(&out, " in environment %s", environment)
	}
	out.WriteString(".\n\nRun orch github init in the repository to write the workflow, or deploy on\nevery push with .github/workflows/deploy.yml; no secrets are needed:\n\n")
	out.WriteString(githubWorkflow{Application: application, Branch: branch, Environment: environment, Cluster: address}.render())
	return out.String()
}

func printGitHubHelp(output io.Writer) {
	fmt.Fprintln(output, `orch github lets GitHub Actions deploy without stored credentials.

Usage:
  orch github init [--app APP] [--branch BRANCH] [--environment NAME] [--image REPOSITORY] [-f orch.pkl] [--force] [--json]
  orch github trust OWNER/REPO --app APP [--branch main] [--environment NAME] [--json]
  orch github list [--json]
  orch github untrust OWNER/REPO --app APP [--json]

A trusted repository's workflow runs on that branch exchange their GitHub OIDC
token for a 15-minute credential that can deploy, roll back and inspect only APP.

init, run inside the repository, trusts it and writes
.github/workflows/deploy.yml, which builds the image when the manifest's
directory has a Dockerfile and deploys on every push.`)
}
