package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/TorstenDittmann/orch/internal/buildinfo"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/panel"
	"github.com/TorstenDittmann/orch/internal/tui"
	"github.com/TorstenDittmann/orch/internal/web"
)

const (
	// panelApplication is the orch application orch panel install deploys.
	panelApplication = panel.Application
	// panelTokenName is the administrator access token the panel uses.
	panelTokenName       = "orch-panel"
	panelTokenLifetime   = 365 * 24 * time.Hour
	panelImageRepository = "ghcr.io/torstendittmann/orch-panel"
	panelPort            = 8080
)

func runPanel(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printPanelHelp(stdout)
		return ExitOK
	}
	switch args[0] {
	case "install":
		return runPanelInstall(ctx, args[1:], stdout, stderr)
	case "open":
		return runPanelOpen(ctx, args[1:], stdout, stderr)
	case "remove":
		return runPanelRemove(ctx, args[1:], stdout, stderr)
	case "serve":
		return runPanelServe(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown panel command %q\n\n", args[0])
		printPanelHelp(stderr)
		return ExitInvalid
	}
}

func runPanelInstall(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("panel install", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath, domain, image string
	var noWait, noOpen bool
	var timeout time.Duration
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.StringVar(&domain, "domain", "", "domain the panel is served on, with automatic HTTPS")
	flags.StringVar(&image, "image", "", "panel image (defaults to the release matching this orch)")
	flags.BoolVar(&noWait, "no-wait", false, "return once the deployment is accepted")
	flags.BoolVar(&noOpen, "no-open", false, "print the sign-in link without opening a browser")
	flags.DurationVar(&timeout, "timeout", 5*time.Minute, "rollout wait deadline")
	reordered := reorderInterspersed(args, map[string]bool{"-context": true, "--context": true, "-domain": true, "--domain": true, "-image": true, "--image": true, "-timeout": true, "--timeout": true})
	if err := flags.Parse(reordered); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: orch panel install --domain DOMAIN [--image REF] [--no-wait] [--no-open] [--target NAME]")
		return ExitInvalid
	}
	if domain == "" || contextPath == "" {
		writeError(stderr, errors.New("panel install requires --domain and a selected target"), false)
		return ExitInvalid
	}
	if image == "" {
		if strings.HasSuffix(buildinfo.Version, "-dev") {
			writeError(stderr, errors.New("this development build has no matching panel image; pass --image"), false)
			return ExitInvalid
		}
		image = panelImageRepository + ":" + buildinfo.Version
	}
	manifest := panelManifest(domain, image)
	if err := config.Validate(manifest); err != nil {
		writeError(stderr, err, false)
		return ExitInvalid
	}
	canonical, err := config.CanonicalJSON(manifest)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()

	// Installing again replaces the token, so it also rotates it.
	if _, err := remote.DeleteAccessToken(ctx, panelTokenName); err != nil {
		writeError(stderr, fmt.Errorf("replace the panel's access token: %w", err), false)
		return ExitRuntime
	}
	token, err := remote.CreateAdministratorAccessToken(ctx, panelTokenName, panelTokenLifetime)
	if err != nil {
		writeError(stderr, fmt.Errorf("create the panel's access token: %w", err), false)
		return ExitRuntime
	}
	key, err := newIdempotencyKey(rand.Reader)
	if err == nil {
		_, err = remote.SetSecret(ctx, panelApplication, "token", []byte(token.GetValue()), key)
	}
	if err != nil {
		writeError(stderr, fmt.Errorf("store the panel's token secret: %w", err), false)
		return ExitRuntime
	}
	if key, err = newIdempotencyKey(rand.Reader); err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	response, err := remote.ApplyApplication(ctx, canonical, key)
	if err != nil {
		writeError(stderr, fmt.Errorf("deploy orch panel: %w", err), false)
		return ExitRuntime
	}
	if !noWait {
		if code := waitForDeployment(ctx, remote, response.GetName(), response.GetGeneration(), response.GetRevisionId(), "", timeout, false, stdout, stderr); code != ExitOK {
			return code
		}
	}
	fmt.Fprintf(stdout, "\norch panel is deployed as the application %s. Point %s at an ingress node.\n", panelApplication, domain)
	return openPanel(ctx, remote, domain, noOpen, stdout, stderr)
}

func runPanelOpen(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("panel open", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var noOpen bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.BoolVar(&noOpen, "no-open", false, "print the link without opening a browser")
	if err := flags.Parse(reorderInterspersed(args, map[string]bool{"-context": true, "--context": true})); err != nil || flags.NArg() != 0 || contextPath == "" {
		fmt.Fprintln(stderr, "usage: orch panel open [--no-open] [--target NAME]")
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	app, err := remote.GetApplication(ctx, panelApplication)
	if err != nil {
		writeError(stderr, fmt.Errorf("orch panel is not installed; run orch panel install --domain DOMAIN: %w", err), false)
		return ExitRuntime
	}
	domain, err := panelDomain(app.GetCanonicalManifestJson())
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	return openPanel(ctx, remote, domain, noOpen, stdout, stderr)
}

// openPanel creates a one-time sign-in link, prints it and opens it. It
// first removes links that expired unused.
func openPanel(ctx context.Context, remote *controlclient.Client, domain string, noOpen bool, stdout, stderr io.Writer) int {
	tokens, err := remote.ListAccessTokens(ctx)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	now := time.Now()
	for _, token := range tokens {
		if strings.HasPrefix(token.GetName(), panel.LoginTokenPrefix) && !now.Before(token.GetExpiresAt().AsTime()) {
			_, _ = remote.DeleteAccessToken(ctx, token.GetName())
		}
	}
	suffix, err := randomPanelSecret(6)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	name := panel.LoginTokenPrefix + suffix
	token, err := remote.CreateAccessToken(ctx, name, panelApplication, true, panel.LoginTokenLifetime)
	if err != nil {
		writeError(stderr, fmt.Errorf("create a sign-in link: %w", err), false)
		return ExitRuntime
	}
	link := panel.LoginURL(domain, name, token.GetValue())
	fmt.Fprintf(stdout, "Open %s\n(the link works once, within %s)\n", link, panel.LoginTokenLifetime)
	if !noOpen {
		openBrowser(link)
	}
	return ExitOK
}

// panelDomain reads the panel's domain from its deployed manifest.
func panelDomain(canonical string) (string, error) {
	manifest, err := config.ParseCanonicalJSON([]byte(canonical))
	if err != nil {
		return "", fmt.Errorf("read the panel's manifest: %w", err)
	}
	service, ok := manifest.Services["panel"]
	if !ok || service.HTTP == nil || len(service.HTTP.Domains) == 0 {
		return "", errors.New("the panel's manifest has no domain; run orch panel install --domain DOMAIN")
	}
	return service.HTTP.Domains[0], nil
}

func runPanelRemove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("panel remove", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	if err := flags.Parse(reorderInterspersed(args, map[string]bool{"-context": true, "--context": true})); err != nil || flags.NArg() != 0 || contextPath == "" {
		fmt.Fprintln(stderr, "usage: orch panel remove [--target NAME]")
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	tokens, err := remote.ListAccessTokens(ctx)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	for _, token := range tokens {
		if token.GetName() != panelTokenName && !strings.HasPrefix(token.GetName(), panel.LoginTokenPrefix) {
			continue
		}
		if _, err := remote.DeleteAccessToken(ctx, token.GetName()); err != nil {
			writeError(stderr, fmt.Errorf("revoke the panel's access tokens: %w", err), false)
			return ExitRuntime
		}
	}
	key, err := newIdempotencyKey(rand.Reader)
	if err == nil {
		_, err = remote.RemoveApplication(ctx, panelApplication, key)
	}
	if err != nil {
		writeError(stderr, fmt.Errorf("remove orch panel: %w", err), false)
		return ExitRuntime
	}
	fmt.Fprintf(stdout, "Revoked the panel's access tokens and removed %s.\n", panelApplication)
	return ExitOK
}

// runPanelServe runs inside the panel's container. It signs in with the
// ORCH_TOKEN administrator token that orch panel install stored.
func runPanelServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("panel serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var listen string
	flags.StringVar(&listen, "listen", fmt.Sprintf(":%d", panelPort), "address to serve on")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: orch panel serve [--listen :8080]")
		return ExitInvalid
	}
	if os.Getenv("ORCH_TOKEN") == "" {
		writeError(stderr, errors.New("orch panel serve needs an administrator token in ORCH_TOKEN; use orch panel install"), false)
		return ExitInvalid
	}
	remote, err := controlclient.NewAccessToken(ctx, os.Getenv("ORCH_TOKEN"))
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	dashboard, err := web.NewHosted(web.ClientProvider{ClientProvider: tui.ClientProvider{Client: remote}}, "panel")
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	handler, err := panel.New(panel.Options{Dashboard: dashboard, Controller: remote})
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		writeError(stderr, fmt.Errorf("listen for orch panel: %w", err), false)
		return ExitRuntime
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	fmt.Fprintf(stdout, "orch panel is serving on %s\n", listener.Addr())
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		return ExitOK
	case err := <-served:
		writeError(stderr, err, false)
		return ExitRuntime
	}
}

// panelManifest is the application orch panel install deploys. The panel
// keeps no state of its own, so it rolls out without downtime.
func panelManifest(domain, image string) *config.Manifest {
	return &config.Manifest{
		Schema: config.SchemaVersion,
		Name:   panelApplication,
		Services: map[string]config.Service{
			"panel": {
				Image:     image,
				Args:      []string{"panel", "serve", "--listen", fmt.Sprintf(":%d", panelPort)},
				Replicas:  1,
				Restart:   config.DefaultRestart,
				Resources: config.Resources{CPU: "100m", Memory: "128Mi"},
				HTTP:      &config.HTTP{Port: panelPort, Domains: []string{domain}, HealthPath: "/healthz", HTTPS: true},
				SecretEnv: map[string]string{"ORCH_TOKEN": "token"},
			},
		},
		Deploy: config.Deployment{MaxSurge: 1, MaxUnavailable: 0, HealthDeadline: config.DefaultHealthDeadline, AutoRollback: true},
	}
}

func randomPanelSecret(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func printPanelHelp(output io.Writer) {
	fmt.Fprintln(output, `orch panel runs the web dashboard on the cluster itself, always on, as
the application orch-panel. Like orch web, you sign in with a one-time link.

Usage:
  orch panel install --domain DOMAIN [--image REF] [--no-wait] [--no-open] [--target NAME]
  orch panel open [--no-open] [--target NAME]
  orch panel remove [--target NAME]

install creates an administrator access token for the panel, deploys it on
DOMAIN with automatic HTTPS, and opens a sign-in link. Running it again
rotates the token. open prints and opens a new sign-in link; each works once,
within 15 minutes. remove revokes the token and removes the application.`)
}
