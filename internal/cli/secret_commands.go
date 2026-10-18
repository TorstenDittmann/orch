package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	controlclient "github.com/TorstenDittmann/orch/internal/client"
	"github.com/TorstenDittmann/orch/internal/config"
)

const maxSecretInput = 1 << 20

type commandInputKey struct{}

// withCommandInput makes commands that read a value from stdin read input
// instead; orch mcp uses it because its own stdin carries the protocol.
func withCommandInput(ctx context.Context, input io.Reader) context.Context {
	return context.WithValue(ctx, commandInputKey{}, input)
}

func secretInput(ctx context.Context) io.Reader {
	if input, ok := ctx.Value(commandInputKey{}).(io.Reader); ok {
		return input
	}
	return os.Stdin
}

func runSecret(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: orch secret set|list|delete ...")
		return ExitInvalid
	}
	switch args[0] {
	case "set":
		return runSecretSet(ctx, args[1:], stdout, stderr)
	case "list":
		return runSecretList(ctx, args[1:], stdout, stderr)
	case "delete":
		return runSecretDelete(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown secret command %q\n", args[0])
		return ExitInvalid
	}
}

func runSecretSet(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("secret set", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath, idempotencyKey string
	var fromStdin, jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.StringVar(&idempotencyKey, "idempotency-key", "", "retry-safe request key")
	flags.BoolVar(&fromStdin, "from-stdin", false, "read the write-only secret value from stdin")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	reordered := reorderInterspersed(args, map[string]bool{"-context": true, "--context": true, "-idempotency-key": true, "--idempotency-key": true})
	if err := flags.Parse(reordered); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 2 || contextPath == "" || !fromStdin {
		writeError(stderr, errors.New("secret set requires APP NAME, --from-stdin, and --context"), jsonOutput)
		return ExitInvalid
	}
	if err := validateTwoNames(flags.Arg(0), flags.Arg(1)); err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitInvalid
	}
	value, err := io.ReadAll(io.LimitReader(secretInput(ctx), maxSecretInput+1))
	if err != nil || len(value) == 0 || len(value) > maxSecretInput {
		writeError(stderr, errors.New("secret stdin must contain 1-1048576 bytes"), jsonOutput)
		return ExitInvalid
	}
	defer wipeCLIBytes(value)
	if idempotencyKey == "" {
		idempotencyKey, err = newIdempotencyKey(rand.Reader)
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	result, err := remote.SetSecret(ctx, flags.Arg(0), flags.Arg(1), value, idempotencyKey)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, result)
	}
	fmt.Fprintf(stdout, "Stored secret %s/%s version %s\n", result.GetApplication(), result.GetName(), result.GetVersionId())
	return ExitOK
}

func runSecretList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("secret list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	reordered := reorderInterspersed(args, map[string]bool{"-context": true, "--context": true})
	if err := flags.Parse(reordered); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 1 || contextPath == "" {
		writeError(stderr, errors.New("secret list requires APP and --context"), jsonOutput)
		return ExitInvalid
	}
	if err := config.ValidateApplicationName(flags.Arg(0)); err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	items, err := remote.ListSecrets(ctx, flags.Arg(0))
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, items)
	}
	for _, item := range items {
		updated := "-"
		if item.GetUpdatedAt() != nil && item.GetUpdatedAt().CheckValid() == nil {
			updated = item.GetUpdatedAt().AsTime().Format(time.RFC3339Nano)
		}
		fmt.Fprintf(stdout, "%s  %s  referenced=%t  updated=%s\n", item.GetName(), item.GetCurrentVersionId(), item.GetReferenced(), updated)
	}
	return ExitOK
}

func runSecretDelete(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("secret delete", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath, idempotencyKey string
	var jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.StringVar(&idempotencyKey, "idempotency-key", "", "retry-safe request key")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	reordered := reorderInterspersed(args, map[string]bool{"-context": true, "--context": true, "-idempotency-key": true, "--idempotency-key": true})
	if err := flags.Parse(reordered); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 2 || contextPath == "" {
		writeError(stderr, errors.New("secret delete requires APP NAME and --context"), jsonOutput)
		return ExitInvalid
	}
	if err := validateTwoNames(flags.Arg(0), flags.Arg(1)); err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitInvalid
	}
	if idempotencyKey == "" {
		var err error
		idempotencyKey, err = newIdempotencyKey(rand.Reader)
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	result, err := remote.DeleteSecret(ctx, flags.Arg(0), flags.Arg(1), idempotencyKey)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, result)
	}
	fmt.Fprintf(stdout, "Deleted secret %s/%s\n", result.GetApplication(), result.GetName())
	return ExitOK
}

func runRegistry(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: orch registry login|list|logout ...")
		return ExitInvalid
	}
	switch args[0] {
	case "login":
		return runRegistryLogin(ctx, args[1:], stdout, stderr)
	case "list":
		return runRegistryList(ctx, args[1:], stdout, stderr)
	case "logout":
		return runRegistryLogout(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown registry command %q\n", args[0])
		return ExitInvalid
	}
}

func runRegistryLogin(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("registry login", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath, username, idempotencyKey string
	var passwordStdin, jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.StringVar(&username, "username", "", "registry username")
	flags.StringVar(&idempotencyKey, "idempotency-key", "", "retry-safe request key")
	flags.BoolVar(&passwordStdin, "password-stdin", false, "read the write-only password or token from stdin")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	reordered := reorderInterspersed(args, map[string]bool{"-context": true, "--context": true, "-username": true, "--username": true, "-idempotency-key": true, "--idempotency-key": true})
	if err := flags.Parse(reordered); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 1 || contextPath == "" || username == "" || !passwordStdin {
		writeError(stderr, errors.New("registry login requires HOST, --username, --password-stdin, and --context"), jsonOutput)
		return ExitInvalid
	}
	secret, err := io.ReadAll(io.LimitReader(secretInput(ctx), maxSecretInput+1))
	if err != nil || len(secret) == 0 || len(secret) > maxSecretInput {
		writeError(stderr, errors.New("registry secret stdin must contain 1-1048576 bytes"), jsonOutput)
		return ExitInvalid
	}
	defer wipeCLIBytes(secret)
	if idempotencyKey == "" {
		idempotencyKey, err = newIdempotencyKey(rand.Reader)
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	result, err := remote.LoginRegistry(ctx, flags.Arg(0), username, secret, idempotencyKey)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, result)
	}
	fmt.Fprintf(stdout, "Stored registry credential for %s as %s (version %s)\n", result.GetHost(), result.GetUsername(), result.GetVersionId())
	return ExitOK
}

func runRegistryList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("registry list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 || contextPath == "" {
		writeError(stderr, errors.New("registry list requires --context"), jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	items, err := remote.ListRegistries(ctx)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, items)
	}
	for _, item := range items {
		fmt.Fprintf(stdout, "%s  %s  %s\n", item.GetHost(), item.GetUsername(), item.GetCurrentVersionId())
	}
	return ExitOK
}

func runRegistryLogout(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("registry logout", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath, idempotencyKey string
	var jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.StringVar(&idempotencyKey, "idempotency-key", "", "retry-safe request key")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	reordered := reorderInterspersed(args, map[string]bool{"-context": true, "--context": true, "-idempotency-key": true, "--idempotency-key": true})
	if err := flags.Parse(reordered); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 1 || contextPath == "" {
		writeError(stderr, errors.New("registry logout requires HOST and --context"), jsonOutput)
		return ExitInvalid
	}
	if idempotencyKey == "" {
		var err error
		idempotencyKey, err = newIdempotencyKey(rand.Reader)
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	result, err := remote.LogoutRegistry(ctx, flags.Arg(0), idempotencyKey)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, result)
	}
	fmt.Fprintf(stdout, "Removed registry credential for %s\n", result.GetHost())
	return ExitOK
}

func validateTwoNames(application, name string) error {
	if err := config.ValidateApplicationName(application); err != nil {
		return err
	}
	if err := config.ValidateApplicationName(name); err != nil {
		return fmt.Errorf("secret name: %w", err)
	}
	return nil
}

func wipeCLIBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
