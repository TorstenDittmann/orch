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
	panelApplication = "orch-panel"
	// panelTokenName is the administrator access token the panel uses.
	panelTokenName       = "orch-panel"
	panelTokenLifetime   = 365 * 24 * time.Hour
	panelImageRepository = "ghcr.io/torstendittmann/orch-panel"
	panelDataDir         = "/data"
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
	var noWait bool
	var timeout time.Duration
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.StringVar(&domain, "domain", "", "domain the panel is served on, with automatic HTTPS")
	flags.StringVar(&image, "image", "", "panel image (defaults to the release matching this orch)")
	flags.BoolVar(&noWait, "no-wait", false, "return once the deployment is accepted")
	flags.DurationVar(&timeout, "timeout", 5*time.Minute, "rollout wait deadline")
	reordered := reorderInterspersed(args, map[string]bool{"-context": true, "--context": true, "-domain": true, "--domain": true, "-image": true, "--image": true, "-timeout": true, "--timeout": true})
	if err := flags.Parse(reordered); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: orch panel install --domain DOMAIN [--image REF] [--no-wait] [--target NAME]")
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
	setupToken, err := randomPanelSecret()
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
	for name, value := range map[string]string{"token": token.GetValue(), "setup-token": setupToken} {
		key, err := newIdempotencyKey(rand.Reader)
		if err == nil {
			_, err = remote.SetSecret(ctx, panelApplication, name, []byte(value), key)
		}
		if err != nil {
			writeError(stderr, fmt.Errorf("store the panel's %s secret: %w", name, err), false)
			return ExitRuntime
		}
	}
	key, err := newIdempotencyKey(rand.Reader)
	if err != nil {
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
	fmt.Fprintf(stdout, "\norch panel is deployed as the application %s.\n", panelApplication)
	fmt.Fprintf(stdout, "Point %s at an ingress node, then set the administrator password at:\n\n  https://%s/setup?token=%s\n\n", domain, domain, setupToken)
	fmt.Fprintln(stdout, "The link works once. Run orch panel install again to reset the password or rotate the panel's access token.")
	return ExitOK
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
	if _, err := remote.DeleteAccessToken(ctx, panelTokenName); err != nil {
		writeError(stderr, fmt.Errorf("revoke the panel's access token: %w", err), false)
		return ExitRuntime
	}
	key, err := newIdempotencyKey(rand.Reader)
	if err == nil {
		_, err = remote.RemoveApplication(ctx, panelApplication, key)
	}
	if err != nil {
		writeError(stderr, fmt.Errorf("remove orch panel: %w", err), false)
		return ExitRuntime
	}
	fmt.Fprintf(stdout, "Revoked the panel's access token and removed %s. Its data volume stays until you remove it with orch remove.\n", panelApplication)
	return ExitOK
}

// runPanelServe runs inside the panel's container. It signs in with the
// ORCH_TOKEN administrator token that orch panel install stored.
func runPanelServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("panel serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var listen, dataDir string
	flags.StringVar(&listen, "listen", fmt.Sprintf(":%d", panelPort), "address to serve on")
	flags.StringVar(&dataDir, "data-dir", panelDataDir, "directory for the panel's own state")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: orch panel serve [--listen :8080] [--data-dir /data]")
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
	handler, err := panel.New(panel.Options{Dashboard: dashboard, DataDir: dataDir, SetupToken: os.Getenv("ORCH_PANEL_SETUP_TOKEN")})
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

// panelManifest is the application orch panel install deploys: one replica
// with its password and settings on a backed-up local volume.
func panelManifest(domain, image string) *config.Manifest {
	return &config.Manifest{
		Schema: config.SchemaVersion,
		Name:   panelApplication,
		Services: map[string]config.Service{
			"panel": {
				Image:     image,
				Args:      []string{"panel", "serve", "--listen", fmt.Sprintf(":%d", panelPort), "--data-dir", panelDataDir},
				Replicas:  1,
				Restart:   config.DefaultRestart,
				Resources: config.Resources{CPU: "100m", Memory: "128Mi"},
				HTTP:      &config.HTTP{Port: panelPort, Domains: []string{domain}, HealthPath: "/healthz", HTTPS: true},
				SecretEnv: map[string]string{"ORCH_TOKEN": "token", "ORCH_PANEL_SETUP_TOKEN": "setup-token"},
				Mounts:    []config.Mount{{Source: "data", Target: panelDataDir}},
			},
		},
		Volumes: map[string]config.Volume{
			"data": {Driver: "local", Backup: &config.VolumeBackup{Schedule: "0 3 * * *", Keep: 7}},
		},
		Deploy: config.Deployment{MaxSurge: 0, MaxUnavailable: 1, HealthDeadline: config.DefaultHealthDeadline, AutoRollback: true},
	}
}

func randomPanelSecret() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate panel setup token: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func printPanelHelp(output io.Writer) {
	fmt.Fprintln(output, `orch panel runs the web dashboard on the cluster itself, always on and
behind a password, as the application orch-panel.

Usage:
  orch panel install --domain DOMAIN [--image REF] [--no-wait] [--target NAME]
  orch panel remove [--target NAME]

install creates an administrator access token for the panel, deploys it on
DOMAIN with automatic HTTPS, and prints a one-time link to set the
administrator password. Running it again resets the password and rotates the
token. remove revokes the token and removes the application.`)
}
