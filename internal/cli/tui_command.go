package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	controlclient "change.codes/torsten/better-swarm/internal/client"
	"change.codes/torsten/better-swarm/internal/tui"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"
)

func runTUI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("tui", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var refresh time.Duration
	flags.StringVar(&contextPath, "context", "", "administrator context compatibility path")
	flags.DurationVar(&refresh, "refresh", 2*time.Second, "refresh interval")
	reordered := reorderInterspersed(args, map[string]bool{"-context": true, "--context": true, "-refresh": true, "--refresh": true})
	if err := flags.Parse(reordered); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: orch tui [--target NAME] [--refresh 2s]")
		return ExitInvalid
	}
	if contextPath == "" || refresh < time.Second || refresh > time.Minute {
		fmt.Fprintln(stderr, "tui requires a selected target and --refresh from 1s through 1m")
		return ExitInvalid
	}
	output, ok := stdout.(*os.File)
	if !ok || !term.IsTerminal(output.Fd()) || !term.IsTerminal(os.Stdin.Fd()) {
		fmt.Fprintln(stderr, "tui requires an interactive terminal; use the regular CLI commands with --json for automation")
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
	model := tui.New(ctx, tui.ClientProvider{Client: remote}, targetName, transport, refresh)
	program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(os.Stdin), tea.WithOutput(output))
	if _, err := program.Run(); err != nil && !errors.Is(err, tea.ErrInterrupted) && !errors.Is(err, context.Canceled) {
		writeError(stderr, fmt.Errorf("run operator TUI: %w", err), false)
		return ExitRuntime
	}
	return ExitOK
}
