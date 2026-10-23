package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
)

func runNotify(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printNotifyHelp(stdout)
		return ExitOK
	}
	command := args[0]
	flags := flag.NewFlagSet("notify "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath, name, events string
	var jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	valueFlags := map[string]bool{"--context": true, "-context": true}
	minArgs, maxArgs := 0, 0
	switch command {
	case "add":
		flags.StringVar(&name, "name", "", "name of the notification (default: the kind)")
		flags.StringVar(&events, "on", "", "comma-separated events to send (default: the cluster events)")
		valueFlags["--name"], valueFlags["-name"], valueFlags["--on"], valueFlags["-on"] = true, true, true, true
		minArgs, maxArgs = 1, 2
	case "remove":
		minArgs, maxArgs = 1, 1
	case "test":
		maxArgs = 1
	case "list":
	default:
		fmt.Fprintf(stderr, "unknown notify command %q\n\n", command)
		printNotifyHelp(stderr)
		return ExitInvalid
	}
	if err := flags.Parse(reorderInterspersed(args[1:], valueFlags)); err != nil {
		return ExitInvalid
	}
	if flags.NArg() < minArgs || flags.NArg() > maxArgs || contextPath == "" {
		usage := map[string]string{"add": "slack|discord|webhook [URL]", "remove": "NAME", "test": "at most one NAME", "list": "no arguments"}[command]
		writeError(stderr, fmt.Errorf("notify %s requires %s", command, usage), jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	switch command {
	case "add":
		url := flags.Arg(1)
		if url == "" {
			// Reading the URL from stdin keeps it out of shell history.
			value, err := io.ReadAll(io.LimitReader(secretInput(ctx), 4097))
			if err != nil {
				writeError(stderr, err, jsonOutput)
				return ExitInvalid
			}
			url = strings.TrimSpace(string(value))
		}
		var on []string
		for _, event := range strings.Split(events, ",") {
			if event = strings.TrimSpace(event); event != "" {
				on = append(on, event)
			}
		}
		notification, err := remote.SetNotification(ctx, name, flags.Arg(0), url, on)
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		// Send a test right away so a wrong URL shows up now, not during
		// an outage.
		results, err := remote.TestNotification(ctx, notification.GetName())
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		testError := ""
		if len(results) == 1 {
			testError = results[0].GetError()
		}
		if jsonOutput {
			return writeJSON(stdout, stderr, struct {
				Notification any    `json:"notification"`
				TestError    string `json:"test_error,omitempty"`
			}{notificationJSON(notification), testError})
		}
		fmt.Fprintf(stdout, "Added notification %s for %s.\n", notification.GetName(), strings.Join(notification.GetEvents(), ", "))
		if testError != "" {
			fmt.Fprintf(stdout, "The test message failed: %s\nCheck the URL and add it again.\n", testError)
			return ExitRuntime
		}
		fmt.Fprintln(stdout, "Sent a test message.")
	case "list":
		notifications, err := remote.ListNotifications(ctx)
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		if jsonOutput {
			items := make([]any, 0, len(notifications))
			for _, notification := range notifications {
				items = append(items, notificationJSON(notification))
			}
			return writeJSON(stdout, stderr, struct {
				Notifications []any `json:"notifications"`
			}{items})
		}
		if len(notifications) == 0 {
			fmt.Fprintln(stdout, "No cluster notifications. Add one with orch notify add slack URL.")
		}
		for _, notification := range notifications {
			fmt.Fprintf(stdout, "%s  %s  %s\n", notification.GetName(), notification.GetKind(), strings.Join(notification.GetEvents(), ","))
		}
	case "test":
		results, err := remote.TestNotification(ctx, flags.Arg(0))
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
		failed := false
		if jsonOutput {
			type result struct {
				Name  string `json:"name"`
				Error string `json:"error,omitempty"`
			}
			items := make([]result, 0, len(results))
			for _, item := range results {
				items = append(items, result{item.GetName(), item.GetError()})
				failed = failed || item.GetError() != ""
			}
			if code := writeJSON(stdout, stderr, struct {
				Results []result `json:"results"`
			}{items}); code != ExitOK {
				return code
			}
		} else {
			if len(results) == 0 {
				fmt.Fprintln(stdout, "No cluster notifications. Add one with orch notify add slack URL.")
			}
			for _, item := range results {
				if item.GetError() != "" {
					failed = true
					fmt.Fprintf(stdout, "%s: failed: %s\n", item.GetName(), item.GetError())
				} else {
					fmt.Fprintf(stdout, "%s: sent\n", item.GetName())
				}
			}
		}
		if failed {
			return ExitRuntime
		}
	case "remove":
		changed, err := remote.DeleteNotification(ctx, flags.Arg(0))
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
			writeError(stderr, errors.New("no such notification"), false)
			return ExitInvalid
		}
		fmt.Fprintf(stdout, "Removed notification %s.\n", flags.Arg(0))
	}
	return ExitOK
}

func notificationJSON(notification *orchv1alpha1.Notification) any {
	return struct {
		Name   string   `json:"name"`
		Kind   string   `json:"kind"`
		Events []string `json:"events"`
	}{notification.GetName(), notification.GetKind(), notification.GetEvents()}
}

func printNotifyHelp(output io.Writer) {
	fmt.Fprintln(output, `orch notify tells you when something is wrong with the cluster itself.

Usage:
  orch notify add slack|discord|webhook [URL] [--name NAME] [--on EVENT,...] [--json]
  orch notify list [--json]
  orch notify test [NAME] [--json]
  orch notify remove NAME [--json]

add stores the webhook URL encrypted on the controller and sends a test
message. Without URL it is read from stdin, which keeps it out of shell
history. NAME defaults to the kind; adding the same name again replaces it.

Cluster events, sent by default:
  node.lost             a node has had no heartbeat for 3 minutes
  node.disk_low         a node's disk is 90% full
  certificate.expiring  a controller, CA, agent or administrator certificate
                        expires within 30 days
  backup.stale          the newest controller backup is older than 2 days

--on can also name application events (deploy.failed, deploy.rolled_back,
deploy.succeeded, backup.failed, cron.failed, replica.unhealthy), which are
then sent for every application.`)
}
