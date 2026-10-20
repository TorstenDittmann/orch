package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
)

func runToken(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printTokenHelp(stdout)
		return ExitOK
	}
	command := args[0]
	flags := flag.NewFlagSet("token "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath, application string
	var readOnly, administrator, jsonOutput bool
	var lifetime time.Duration
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	valueFlags := map[string]bool{"--context": true, "-context": true}
	maxArgs := 0
	switch command {
	case "create":
		flags.StringVar(&application, "app", "", "the one application the token may use")
		flags.BoolVar(&readOnly, "read-only", false, "allow only reading the application, not deploying it")
		flags.BoolVar(&administrator, "admin", false, "allow everything an administrator can do, on every application")
		flags.DurationVar(&lifetime, "ttl", 30*24*time.Hour, "how long the token works, up to 8760h")
		valueFlags["--app"], valueFlags["-app"], valueFlags["--ttl"], valueFlags["-ttl"] = true, true, true, true
		maxArgs = 1
	case "revoke":
		maxArgs = 1
	case "list":
	default:
		fmt.Fprintf(stderr, "unknown token command %q\n\n", command)
		printTokenHelp(stderr)
		return ExitInvalid
	}
	if err := flags.Parse(reorderInterspersed(args[1:], valueFlags)); err != nil {
		return ExitInvalid
	}
	usage := map[string]string{"create": "--app APP or --admin", "revoke": "NAME", "list": "no arguments"}[command]
	if flags.NArg() > maxArgs || contextPath == "" || (command == "create" && (application == "") == !administrator) || (command == "revoke" && flags.NArg() != 1) {
		writeError(stderr, fmt.Errorf("token %s requires %s", command, usage), jsonOutput)
		return ExitInvalid
	}
	if administrator && readOnly {
		writeError(stderr, errors.New("--admin and --read-only cannot be combined"), jsonOutput)
		return ExitInvalid
	}
	if command == "create" && (lifetime < time.Minute || lifetime > 365*24*time.Hour) {
		writeError(stderr, errors.New("--ttl must be between 1m and 8760h"), jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	switch command {
	case "create":
		name := flags.Arg(0)
		if name == "" && administrator {
			name = "admin"
		} else if name == "" {
			// One token per application and access level needs no name.
			name = application
			if readOnly {
				name += "-read-only"
			}
		}
		var response *orchv1alpha1.CreateAccessTokenResponse
		if administrator {
			response, err = remote.CreateAdministratorAccessToken(ctx, name, lifetime)
		} else {
			response, err = remote.CreateAccessToken(ctx, name, application, readOnly, lifetime)
		}
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		token := response.GetToken()
		if jsonOutput {
			return writeJSON(stdout, stderr, struct {
				Name          string    `json:"name"`
				Application   string    `json:"application"`
				ReadOnly      bool      `json:"read_only"`
				Administrator bool      `json:"administrator"`
				ExpiresAt     time.Time `json:"expires_at"`
				Token         string    `json:"token"`
			}{token.GetName(), token.GetApplication(), token.GetReadOnly(), token.GetAdministrator(), token.GetExpiresAt().AsTime(), response.GetValue()})
		}
		access := "deploy, roll back and inspect " + token.GetApplication()
		if token.GetAdministrator() {
			access = "do everything an administrator can"
		} else if token.GetReadOnly() {
			access = "inspect " + token.GetApplication()
		}
		fmt.Fprintf(stderr, "Created token %s. It can %s until %s.\n", token.GetName(), access, token.GetExpiresAt().AsTime().Format("2006-01-02"))
		fmt.Fprintf(stderr, "It is shown only once. Give it to an agent or script as ORCH_TOKEN, for example:\n\n")
		fmt.Fprintf(stderr, "  claude mcp add orch -e ORCH_TOKEN=TOKEN -- orch mcp\n\n")
		fmt.Fprintf(stderr, "Revoke it with: orch token revoke %s\n\n", token.GetName())
		fmt.Fprintln(stdout, response.GetValue())
	case "list":
		tokens, err := remote.ListAccessTokens(ctx)
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		if jsonOutput {
			return writeJSON(stdout, stderr, struct {
				Tokens any `json:"tokens"`
			}{tokens})
		}
		if len(tokens) == 0 {
			fmt.Fprintln(stdout, "No access tokens. Create one with orch token create --app APP.")
		}
		now := time.Now()
		for _, token := range tokens {
			scope, access := "app "+token.GetApplication(), "deploy"
			if token.GetAdministrator() {
				scope, access = "all apps", "administrator"
			} else if token.GetReadOnly() {
				access = "read-only"
			}
			state := "expires " + token.GetExpiresAt().AsTime().Format("2006-01-02")
			if !now.Before(token.GetExpiresAt().AsTime()) {
				state = "expired"
			}
			used := "never used"
			if token.GetLastUsedAt() != nil {
				used = "last used " + token.GetLastUsedAt().AsTime().Format(time.RFC3339)
			}
			fmt.Fprintf(stdout, "%s  %s  %s  %s  %s\n", token.GetName(), scope, access, state, used)
		}
	case "revoke":
		changed, err := remote.DeleteAccessToken(ctx, flags.Arg(0))
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
			writeError(stderr, fmt.Errorf("no access token named %s", flags.Arg(0)), false)
			return ExitInvalid
		}
		fmt.Fprintf(stdout, "Revoked %s. Credentials already issued with it expire within 15 minutes.\n", flags.Arg(0))
	}
	return ExitOK
}

func printTokenHelp(output io.Writer) {
	fmt.Fprintln(output, `orch token gives agents and scripts limited access to one application.

Usage:
  orch token create [NAME] --app APP [--read-only] [--ttl 720h] [--json]
  orch token create [NAME] --admin [--ttl 720h] [--json]
  orch token list [--json]
  orch token revoke NAME [--json]

create prints a token for ORCH_TOKEN. With it, orch commands and orch mcp
can deploy, roll back and inspect APP, or with --read-only only inspect it,
and nothing else on the cluster. The token names the cluster and pins its
CA, so it needs no target. NAME defaults to APP (APP-read-only with
--read-only).

With --admin instead of --app, the token can do everything an
administrator can, for tools such as a web panel that manage the whole
cluster. NAME defaults to admin.`)
}
