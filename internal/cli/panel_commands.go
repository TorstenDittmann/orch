package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
	panelApplication     = panel.Application
	panelTokenLifetime   = 365 * 24 * time.Hour
	panelImageRepository = "ghcr.io/torstendittmann/orch-panel"
	panelPort            = 8080
	panelDataDir         = "/data"
)

func runPanel(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printPanelHelp(stdout)
		return ExitOK
	}
	switch args[0] {
	case "connect":
		return runPanelConnect(ctx, args[1:], stdout, stderr)
	case "open":
		return runPanelOpen(ctx, args[1:], stdout, stderr)
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

// runPanelConnect connects the selected cluster to a panel: it creates an
// administrator access token for the panel and hands it over with a
// connect code, then signs the browser in.
func runPanelConnect(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("panel connect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath, code, name string
	var noOpen bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.StringVar(&code, "code", "", "connect code from the panel or its logs")
	flags.StringVar(&name, "name", "", "name for the cluster in the panel (defaults to the cluster's name)")
	flags.BoolVar(&noOpen, "no-open", false, "print the sign-in link without opening a browser")
	reordered := reorderInterspersed(args, map[string]bool{"-context": true, "--context": true, "-code": true, "--code": true, "-name": true, "--name": true})
	if err := flags.Parse(reordered); err != nil || flags.NArg() != 1 || contextPath == "" {
		fmt.Fprintln(stderr, "usage: orch panel connect URL --code CODE [--name NAME] [--no-open] [--target NAME]")
		return ExitInvalid
	}
	panelURL, err := parsePanelURL(flags.Arg(0))
	if err != nil {
		writeError(stderr, err, false)
		return ExitInvalid
	}
	if code == "" {
		writeError(stderr, errors.New("panel connect needs --code; copy the command from the panel under Clusters, or from its logs for the first cluster"), false)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	if err := connectPanel(ctx, remote, panelURL, code, name, stdout); err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	return openPanel(ctx, remote, panelURL, noOpen, stdout, stderr)
}

// connectPanel gives the panel at panelURL a new administrator access token
// for the cluster. Connecting again replaces the token, so it also rotates it.
func connectPanel(ctx context.Context, remote *controlclient.Client, panelURL, code, name string, stdout io.Writer) error {
	// Check the code before replacing the token, so a bad code does not cut
	// off a panel that already uses it. The panel accepts the code and then
	// rejects the missing token.
	if _, err := postPanelConnect(ctx, panelURL, code, name, ""); err == nil || errors.Is(err, errPanelCodeRejected) || !errors.Is(err, errPanelTokenRejected) {
		if err == nil {
			err = errors.New("the panel accepted a connect request without a token")
		}
		return fmt.Errorf("connect to orch panel at %s: %w", panelURL, err)
	}
	if _, err := remote.DeleteAccessToken(ctx, panel.TokenName); err != nil {
		return fmt.Errorf("replace the panel's access token: %w", err)
	}
	token, err := remote.CreateAdministratorAccessToken(ctx, panel.TokenName, panelTokenLifetime)
	if err != nil {
		return fmt.Errorf("create the panel's access token: %w", err)
	}
	connected, err := postPanelConnect(ctx, panelURL, code, name, token.GetValue())
	if err != nil {
		_, _ = remote.DeleteAccessToken(ctx, panel.TokenName)
		return fmt.Errorf("connect to orch panel at %s: %w", panelURL, err)
	}
	rememberPanelURL(panelURL)
	fmt.Fprintf(stdout, "Connected this cluster to orch panel at %s as %s.\n", panelURL, connected)
	return nil
}

var (
	// errPanelCodeRejected reports a connect code the panel did not accept.
	errPanelCodeRejected = errors.New("the panel rejected the connect code")
	// errPanelTokenRejected reports a request the panel refused after
	// accepting its code.
	errPanelTokenRejected = errors.New("the panel rejected the request")
)

var panelHTTPClient = &http.Client{Timeout: 30 * time.Second}

func postPanelConnect(ctx context.Context, panelURL, code, name, token string) (string, error) {
	body, err := json.Marshal(map[string]string{"code": code, "name": name, "token": token})
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, panelURL+"/connect", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := panelHTTPClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var result struct {
		Name  string `json:"name"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil || response.StatusCode != http.StatusOK {
		switch {
		case response.StatusCode == http.StatusForbidden:
			return "", fmt.Errorf("%w: %s", errPanelCodeRejected, result.Error)
		case response.StatusCode == http.StatusBadRequest && result.Error != "":
			return "", fmt.Errorf("%w: %s", errPanelTokenRejected, result.Error)
		case result.Error != "":
			return "", errors.New(result.Error)
		}
		return "", fmt.Errorf("the panel answered %s", response.Status)
	}
	return result.Name, nil
}

// parsePanelURL accepts the panel's base URL. Plain HTTP is only allowed to
// this machine, since the connect request carries an administrator token.
func parsePanelURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(value, "/"))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("panel URL %q must look like https://panel.example.com", value)
	}
	if parsed.Scheme == "http" {
		if ip := net.ParseIP(parsed.Hostname()); parsed.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", errors.New("the panel must be reached over HTTPS, except on this machine")
		}
	} else if parsed.Scheme != "https" {
		return "", fmt.Errorf("panel URL %q must look like https://panel.example.com", value)
	}
	return parsed.String(), nil
}

func runPanelOpen(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("panel open", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var noOpen bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.BoolVar(&noOpen, "no-open", false, "print the link without opening a browser")
	if err := flags.Parse(reorderInterspersed(args, map[string]bool{"-context": true, "--context": true})); err != nil || flags.NArg() > 1 || contextPath == "" {
		fmt.Fprintln(stderr, "usage: orch panel open [URL] [--no-open] [--target NAME]")
		return ExitInvalid
	}
	value := flags.Arg(0)
	if value == "" {
		value = rememberedPanelURL()
	}
	if value == "" {
		writeError(stderr, errors.New("panel open needs the panel's URL the first time: orch panel open https://panel.example.com"), false)
		return ExitInvalid
	}
	panelURL, err := parsePanelURL(value)
	if err != nil {
		writeError(stderr, err, false)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	rememberPanelURL(panelURL)
	return openPanel(ctx, remote, panelURL, noOpen, stdout, stderr)
}

// openPanel creates a one-time sign-in link, prints it and opens it. It
// first removes links that expired unused.
func openPanel(ctx context.Context, remote *controlclient.Client, panelURL string, noOpen bool, stdout, stderr io.Writer) int {
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
	link := panel.LoginURL(panelURL, name, token.GetValue())
	fmt.Fprintf(stdout, "Open %s\n(the link works once, within %s)\n", link, panel.LoginTokenLifetime)
	if !noOpen {
		openBrowser(link)
	}
	return ExitOK
}

// panelURLFile remembers the last panel connected to or opened, so orch
// panel open needs no URL afterwards.
func panelURLFile() string {
	root := os.Getenv("XDG_CONFIG_HOME")
	if root == "" {
		var err error
		if root, err = os.UserConfigDir(); err != nil {
			return ""
		}
	}
	return filepath.Join(root, "orch", "panel-url")
}

func rememberPanelURL(value string) {
	if path := panelURLFile(); path != "" && os.MkdirAll(filepath.Dir(path), 0o700) == nil {
		_ = os.WriteFile(path, []byte(value+"\n"), 0o600)
	}
}

func rememberedPanelURL() string {
	path := panelURLFile()
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// runPanelInstall deploys the panel as an application on the selected
// cluster and connects that cluster to it.
func runPanelInstall(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("panel install", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath, domain, image, name string
	var noWait, noOpen bool
	var timeout time.Duration
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.StringVar(&domain, "domain", "", "domain the panel is served on, with automatic HTTPS")
	flags.StringVar(&image, "image", "", "panel image (defaults to the release matching this orch)")
	flags.StringVar(&name, "name", "", "name for this cluster in the panel (defaults to the cluster's name)")
	flags.BoolVar(&noWait, "no-wait", false, "return once the deployment is accepted, without connecting")
	flags.BoolVar(&noOpen, "no-open", false, "print the sign-in link without opening a browser")
	flags.DurationVar(&timeout, "timeout", 5*time.Minute, "rollout and connect deadline")
	reordered := reorderInterspersed(args, map[string]bool{"-context": true, "--context": true, "-domain": true, "--domain": true, "-image": true, "--image": true, "-name": true, "--name": true, "-timeout": true, "--timeout": true})
	if err := flags.Parse(reordered); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: orch panel install --domain DOMAIN [--image REF] [--name NAME] [--no-wait] [--no-open] [--target NAME]")
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

	// The setup code lets this command connect the first cluster.
	setupCode, err := randomPanelSecret(10)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	key, err := newIdempotencyKey(rand.Reader)
	if err == nil {
		_, err = remote.SetSecret(ctx, panelApplication, "setup-code", []byte(setupCode), key)
	}
	if err != nil {
		writeError(stderr, fmt.Errorf("store the panel's setup code: %w", err), false)
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
	panelURL := "https://" + domain
	connectCommand := fmt.Sprintf("orch panel connect %s --code %s", panelURL, setupCode)
	if noWait {
		fmt.Fprintf(stdout, "orch panel is deploying as the application %s. Once %s points at an ingress node, connect this cluster with:\n  %s\n", panelApplication, domain, connectCommand)
		return ExitOK
	}
	if code := waitForDeployment(ctx, remote, response.GetName(), response.GetGeneration(), response.GetRevisionId(), "", timeout, false, stdout, stderr); code != ExitOK {
		return code
	}
	fmt.Fprintf(stdout, "\norch panel is deployed as the application %s.\n", panelApplication)
	// The panel only answers on its domain once DNS and its certificate are
	// in place, so wait for it until the deadline.
	if !waitForPanel(ctx, panelURL, time.Now().Add(timeout)) {
		fmt.Fprintf(stdout, "%s does not answer yet. Once it points at an ingress node, connect this cluster with:\n  %s\n", panelURL, connectCommand)
		return ExitRuntime
	}
	err = connectPanel(ctx, remote, panelURL, setupCode, name, stdout)
	if errors.Is(err, errPanelCodeRejected) {
		// The panel already has clusters, so it was installed before and
		// keeps them; its setup code only connects the first one.
		rememberPanelURL(panelURL)
		fmt.Fprintf(stdout, "orch panel already has its clusters. If this cluster is not one of them, connect it with a code from Clusters in the panel.\n")
	} else if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	return openPanel(ctx, remote, panelURL, noOpen, stdout, stderr)
}

func waitForPanel(ctx context.Context, panelURL string, deadline time.Time) bool {
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, panelURL+"/healthz", nil)
		if err != nil {
			return false
		}
		if response, err := panelHTTPClient.Do(request); err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Second):
		}
	}
}

// runPanelRemove revokes the panel's tokens on the selected cluster and
// removes the panel application if orch panel install deployed it there.
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
		if token.GetName() != panel.TokenName && !strings.HasPrefix(token.GetName(), panel.LoginTokenPrefix) {
			continue
		}
		if _, err := remote.DeleteAccessToken(ctx, token.GetName()); err != nil {
			writeError(stderr, fmt.Errorf("revoke the panel's access tokens: %w", err), false)
			return ExitRuntime
		}
	}
	fmt.Fprintln(stdout, "Revoked the panel's access tokens; the panel can no longer manage this cluster.")
	if _, err := remote.GetApplication(ctx, panelApplication); err != nil {
		return ExitOK
	}
	key, err := newIdempotencyKey(rand.Reader)
	if err == nil {
		_, err = remote.RemoveApplication(ctx, panelApplication, key)
	}
	if err != nil {
		writeError(stderr, fmt.Errorf("remove orch panel: %w", err), false)
		return ExitRuntime
	}
	fmt.Fprintf(stdout, "Removed the application %s and its connected clusters.\n", panelApplication)
	return ExitOK
}

// runPanelServe runs the panel, in its container or anywhere else.
func runPanelServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("panel serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var listen, dataDir, publicURL string
	flags.StringVar(&listen, "listen", fmt.Sprintf(":%d", panelPort), "address to serve on")
	flags.StringVar(&dataDir, "data", envOr("ORCH_PANEL_DATA", "orch-panel-data"), "directory that keeps the connected clusters")
	flags.StringVar(&publicURL, "url", os.Getenv("ORCH_PANEL_URL"), "URL the panel is reached at, for the setup command it logs")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: orch panel serve [--listen :8080] [--data DIR] [--url URL]")
		return ExitInvalid
	}
	handler, err := panel.New(panel.Options{
		DataDir: dataDir, Connect: connectPanelCluster, SetupCode: os.Getenv("ORCH_PANEL_SETUP_CODE"), URL: publicURL, Log: stdout,
	})
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	defer handler.Close()
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

// connectPanelCluster signs the panel in to a cluster with an
// administrator access token, and refuses any other token.
func connectPanelCluster(ctx context.Context, token string) (*panel.Connection, error) {
	remote, err := controlclient.NewAccessToken(ctx, token)
	if err != nil {
		return nil, err
	}
	cluster, _, err := remote.GetClusterStatus(ctx)
	if err == nil {
		// Only administrators can list access tokens.
		_, err = remote.ListAccessTokens(ctx)
	}
	if err != nil {
		remote.CloseIdleConnections()
		return nil, fmt.Errorf("the token must be an administrator token: %w", err)
	}
	dashboard, err := web.NewHosted(web.ClientProvider{ClientProvider: tui.ClientProvider{Client: remote}}, cluster.GetName())
	if err != nil {
		remote.CloseIdleConnections()
		return nil, err
	}
	return &panel.Connection{Name: cluster.GetName(), Controller: remote, Dashboard: dashboard, Close: remote.CloseIdleConnections}, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// panelManifest is the application orch panel install deploys. The
// connected clusters live on a local volume, so one replica runs at a time.
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
				Env:       map[string]string{"ORCH_PANEL_DATA": panelDataDir, "ORCH_PANEL_URL": "https://" + domain},
				SecretEnv: map[string]string{"ORCH_PANEL_SETUP_CODE": "setup-code"},
				Mounts:    []config.Mount{{Source: "data", Target: panelDataDir}},
			},
		},
		Volumes: map[string]config.Volume{"data": {Driver: "local"}},
		Deploy:  config.Deployment{MaxSurge: 0, MaxUnavailable: 1, HealthDeadline: config.DefaultHealthDeadline, AutoRollback: true},
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
	fmt.Fprintln(output, `orch panel is a self-hosted dashboard that manages any number of orch
clusters. Like orch web, you sign in with a one-time link.

Usage:
  orch panel connect URL --code CODE [--name NAME] [--no-open] [--target NAME]
  orch panel open [URL] [--no-open] [--target NAME]
  orch panel install --domain DOMAIN [--image REF] [--name NAME] [--no-wait] [--no-open] [--target NAME]
  orch panel remove [--target NAME]
  orch panel serve [--listen :8080] [--data DIR] [--url URL]

serve runs the panel anywhere, for example in the orch-panel container. On
first start it logs the connect command for the first cluster. connect gives
the panel an administrator token for the selected cluster, using a code from
the panel's logs or from Clusters in the panel, and opens a sign-in link.
open prints and opens a new sign-in link; each works once, within 15 minutes.
install deploys the panel on the selected cluster at DOMAIN with automatic
HTTPS and connects that cluster. remove revokes the panel's tokens on the
selected cluster, and removes the panel if install deployed it there.`)
}
