package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	controlclient "change.codes/torsten/better-swarm/internal/client"
	"change.codes/torsten/better-swarm/internal/tui"
	"change.codes/torsten/better-swarm/internal/web"
)

func runWeb(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("web", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var port int
	var noOpen bool
	flags.StringVar(&contextPath, "context", "", "administrator context compatibility path")
	flags.IntVar(&port, "port", 0, "loopback port, 0 picks a free port")
	flags.BoolVar(&noOpen, "no-open", false, "print the URL without opening a browser")
	reordered := reorderInterspersed(args, map[string]bool{"-context": true, "--context": true, "-port": true, "--port": true})
	if err := flags.Parse(reordered); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: orch web [--target NAME] [--port N] [--no-open]")
		return ExitInvalid
	}
	if contextPath == "" || port < 0 || port > 65535 {
		fmt.Fprintln(stderr, "web requires a selected target and --port from 0 through 65535")
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	targetName, transport := remote.Target()
	if transport == "" {
		transport = "direct"
	}
	token, err := web.NewToken()
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	handler, err := web.New(web.ClientProvider{ClientProvider: tui.ClientProvider{Client: remote}}, targetName, transport, token)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		writeError(stderr, fmt.Errorf("listen for web dashboard: %w", err), false)
		return ExitRuntime
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	address := fmt.Sprintf("http://%s/?token=%s", listener.Addr().String(), token)
	fmt.Fprintf(stdout, "orch web is serving %s\nOpen %s (the link works once)\nPress Ctrl+C to stop.\n", displayTarget(targetName), address)
	if !noOpen {
		openBrowser(address)
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		return ExitOK
	case err := <-served:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			writeError(stderr, fmt.Errorf("serve web dashboard: %w", err), false)
			return ExitRuntime
		}
		return ExitOK
	}
}

func displayTarget(name string) string {
	if name == "" {
		return "the selected context"
	}
	return "target " + name
}

// openBrowser is best-effort; the printed URL is the fallback.
func openBrowser(address string) {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", address)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", address)
	default:
		command = exec.Command("xdg-open", address)
	}
	if err := command.Start(); err == nil {
		go func() { _ = command.Wait() }()
	}
}
