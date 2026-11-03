package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
)

// nodeUpdatePoll is how often orch node update looks at a node while it
// drains and reboots. It is a variable so tests can shorten it.
var nodeUpdatePoll = 3 * time.Second

// nodeUpdateClient is the part of the controller API orch node update uses.
type nodeUpdateClient interface {
	ListNodes(context.Context) ([]*orchv1alpha1.Node, error)
	UpdateNode(context.Context, *orchv1alpha1.UpdateNodeRequest) (*orchv1alpha1.UpdateNodeResponse, error)
	UpdateNodeOS(context.Context, *orchv1alpha1.UpdateNodeOSRequest) (*orchv1alpha1.UpdateNodeOSResponse, error)
}

type nodeUpdateOptions struct {
	drainTimeout  time.Duration
	rebootTimeout time.Duration
}

type nodeUpdateResult struct {
	Node      string `json:"node"`
	Rebooted  bool   `json:"rebooted"`
	Lifecycle string `json:"lifecycle,omitempty"`
	Skipped   string `json:"skipped,omitempty"`
	Error     string `json:"error,omitempty"`
}

func runNodeUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("node update", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var jsonOutput, all bool
	options := nodeUpdateOptions{}
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	flags.BoolVar(&all, "all", false, "update every node, one at a time")
	flags.DurationVar(&options.drainTimeout, "drain-timeout", 10*time.Minute, "how long to wait for a node's replicas to move")
	flags.DurationVar(&options.rebootTimeout, "reboot-timeout", 15*time.Minute, "how long to wait for a node to come back after its reboot")
	reordered := reorderInterspersed(args, map[string]bool{
		"-context": true, "--context": true, "-drain-timeout": true, "--drain-timeout": true,
		"-reboot-timeout": true, "--reboot-timeout": true,
	})
	if err := flags.Parse(reordered); err != nil {
		return ExitInvalid
	}
	if contextPath == "" {
		writeError(stderr, errors.New("node update requires --context"), jsonOutput)
		return ExitInvalid
	}
	if all == (flags.NArg() != 0) {
		writeError(stderr, errors.New("node update requires NODE... or --all"), jsonOutput)
		return ExitInvalid
	}
	if options.drainTimeout <= 0 || options.rebootTimeout <= 0 {
		writeError(stderr, errors.New("--drain-timeout and --reboot-timeout must be positive"), jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	progress := stdout
	if jsonOutput {
		progress = stderr
	}
	results, err := updateNodes(ctx, remote, flags.Args(), options, progress)
	if jsonOutput {
		if code := writeJSON(stdout, stderr, results); code != ExitOK {
			return code
		}
	}
	if err != nil {
		if !jsonOutput {
			writeError(stderr, err, false)
		}
		return ExitRuntime
	}
	return ExitOK
}

// updateNodes updates the named nodes, or every node when names is empty,
// one at a time, and stops at the first node that fails so a problem with
// the updates does not spread through the cluster. Ingress nodes and the
// controller's host go last.
func updateNodes(ctx context.Context, remote nodeUpdateClient, names []string, options nodeUpdateOptions, progress io.Writer) ([]nodeUpdateResult, error) {
	nodes, err := remote.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]*orchv1alpha1.Node, len(nodes))
	for _, node := range nodes {
		byName[node.GetName()] = node
	}
	var targets []*orchv1alpha1.Node
	if len(names) == 0 {
		targets = nodes
	} else {
		for _, name := range names {
			node, ok := byName[name]
			if !ok {
				return nil, fmt.Errorf("node %q was not found", name)
			}
			if !slices.Contains(targets, node) {
				targets = append(targets, node)
			}
		}
	}
	sort.SliceStable(targets, func(i, j int) bool { return nodeUpdateRank(targets[i]) < nodeUpdateRank(targets[j]) })
	results := make([]nodeUpdateResult, 0, len(targets))
	var skipped []string
	for _, node := range targets {
		result := nodeUpdateResult{Node: node.GetName()}
		switch {
		case node.GetConnectionStatus() != orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_CONNECTED:
			result.Skipped = "not connected"
		case !node.GetOsUpdates():
			result.Skipped = "its agent cannot run OS updates (it needs root, systemd and apt-get or dnf, and an agent from this release)"
		}
		if result.Skipped != "" {
			fmt.Fprintf(progress, "Skipping %s: %s\n", node.GetName(), result.Skipped)
			skipped = append(skipped, node.GetName())
			results = append(results, result)
			continue
		}
		result, err := updateNode(ctx, remote, node, nodes, options, progress)
		results = append(results, result)
		if err != nil {
			return results, fmt.Errorf("update %s: %w", node.GetName(), err)
		}
	}
	if len(skipped) != 0 {
		return results, fmt.Errorf("skipped %s", strings.Join(skipped, ", "))
	}
	return results, nil
}

// nodeUpdateRank orders workers first, then ingress nodes, then the
// controller's host, whose reboot takes the API away for a while.
func nodeUpdateRank(node *orchv1alpha1.Node) int {
	switch {
	case node.GetControllerHost():
		return 2
	case node.GetIngress():
		return 1
	default:
		return 0
	}
}

func updateNode(ctx context.Context, remote nodeUpdateClient, node *orchv1alpha1.Node, nodes []*orchv1alpha1.Node, options nodeUpdateOptions, progress io.Writer) (nodeUpdateResult, error) {
	result := nodeUpdateResult{Node: node.GetName()}
	// The node goes back to how it was. A node that was already draining
	// stays so, since someone is taking it out of service.
	restore := orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_UNSPECIFIED
	switch node.GetLifecycle() {
	case orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_ACTIVE, orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_CORDONED:
		restore = node.GetLifecycle()
	}
	// With no other node to take its replicas, draining would only stop them
	// for the whole upgrade. Cordoning keeps them running until the reboot.
	pause := orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_DRAINING
	if !otherActiveNode(node, nodes) {
		pause = orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_CORDONED
	}
	// Until the controller has the upgrade, a failure or Ctrl-C puts the node
	// back the way it was.
	paused, handedOver := false, false
	defer func() {
		if !paused || handedOver || restore == orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_UNSPECIFIED {
			return
		}
		restoreContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if key, err := newIdempotencyKey(rand.Reader); err == nil {
			if _, err := remote.UpdateNode(restoreContext, &orchv1alpha1.UpdateNodeRequest{Name: node.GetName(), Lifecycle: restore, IdempotencyKey: key}); err == nil {
				fmt.Fprintf(progress, "Returned %s to %s\n", node.GetName(), strings.TrimPrefix(restore.String(), "NODE_LIFECYCLE_"))
			}
		}
	}()
	if node.GetLifecycle() != pause && node.GetLifecycle() != orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_DRAINING {
		key, err := newIdempotencyKey(rand.Reader)
		if err != nil {
			return result, err
		}
		if _, err := remote.UpdateNode(ctx, &orchv1alpha1.UpdateNodeRequest{Name: node.GetName(), Lifecycle: pause, IdempotencyKey: key}); err != nil {
			return result, err
		}
		paused = true
	}
	if pause == orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_DRAINING {
		fmt.Fprintf(progress, "Draining %s\n", node.GetName())
		if err := waitNodeDrained(ctx, remote, node.GetName(), options.drainTimeout, progress); err != nil {
			return result, err
		}
	} else {
		fmt.Fprintf(progress, "Cordoned %s; no other node can take its replicas, so they stop only for the reboot\n", node.GetName())
	}
	fmt.Fprintf(progress, "Upgrading packages on %s\n", node.GetName())
	response, err := remote.UpdateNodeOS(ctx, &orchv1alpha1.UpdateNodeOSRequest{Name: node.GetName(), RestoreLifecycle: restore})
	if err != nil {
		return result, err
	}
	// From here the controller returns the node to service.
	handedOver = true
	if response.GetRebooting() {
		result.Rebooted = true
		fmt.Fprintf(progress, "Rebooting %s\n", node.GetName())
		if err := waitNodeBack(ctx, remote, node.GetName(), response.GetBootId(), restore, options.rebootTimeout); err != nil {
			return result, err
		}
	}
	current, err := findNode(ctx, remote, node.GetName())
	if err != nil {
		return result, err
	}
	result.Lifecycle = strings.TrimPrefix(current.GetLifecycle().String(), "NODE_LIFECYCLE_")
	verb := "Updated"
	if result.Rebooted {
		verb = "Updated and rebooted"
	}
	fmt.Fprintf(progress, "%s %s (lifecycle=%s)\n", verb, node.GetName(), result.Lifecycle)
	if restore == orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_UNSPECIFIED {
		fmt.Fprintf(progress, "%s was draining before the update and stays draining; run orch node uncordon %s to use it again\n", node.GetName(), node.GetName())
	}
	return result, nil
}

func otherActiveNode(node *orchv1alpha1.Node, nodes []*orchv1alpha1.Node) bool {
	for _, other := range nodes {
		if other.GetId() != node.GetId() && other.GetLifecycle() == orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_ACTIVE &&
			other.GetConnectionStatus() == orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_CONNECTED {
			return true
		}
	}
	return false
}

// waitNodeDrained waits until only replicas pinned to a local volume are
// left on the node. Those stop for the reboot and start again after it. When
// the drain does not finish in time the update goes ahead, since the
// remaining replicas would see no worse than that.
func waitNodeDrained(ctx context.Context, remote nodeUpdateClient, name string, timeout time.Duration, progress io.Writer) error {
	deadline := time.Now().Add(timeout)
	for {
		node, err := findNode(ctx, remote, name)
		if err != nil {
			return err
		}
		var waiting []string
		for _, blocker := range node.GetDrainBlockers() {
			if blocker != "local_volume_pinned" {
				waiting = append(waiting, blocker)
			}
		}
		if len(waiting) == 0 {
			if slices.Contains(node.GetDrainBlockers(), "local_volume_pinned") {
				fmt.Fprintf(progress, "Replicas with a local volume stay on %s and restart with it\n", name)
			}
			return nil
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(progress, "%s did not finish draining in %s (%s); continuing\n", name, timeout, strings.Join(waiting, ","))
			return nil
		}
		if err := sleepContext(ctx, nodeUpdatePoll); err != nil {
			return err
		}
	}
}

// waitNodeBack waits until the node's agent runs again under a new boot id
// and the controller has returned it to service. Errors are retried: when
// the controller shares the rebooting host, its API is away for a while.
func waitNodeBack(ctx context.Context, remote nodeUpdateClient, name, bootID string, restore orchv1alpha1.NodeLifecycle, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		if err := sleepContext(ctx, nodeUpdatePoll); err != nil {
			return err
		}
		node, err := findNode(ctx, remote, name)
		switch {
		case err != nil:
			last = err
		case node.GetBootId() == bootID || node.GetConnectionStatus() != orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_CONNECTED:
			last = errors.New("its agent has not reconnected")
		case restore != orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_UNSPECIFIED && node.GetLifecycle() != restore:
			last = fmt.Errorf("it is still %s", node.GetLifecycle())
		default:
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not come back within %s: %w; the controller returns it to service when its agent reconnects", name, timeout, last)
		}
	}
}

func findNode(ctx context.Context, remote nodeUpdateClient, name string) (*orchv1alpha1.Node, error) {
	nodes, err := remote.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, node := range nodes {
		if node.GetName() == name {
			return node, nil
		}
	}
	return nil, fmt.Errorf("node %q was not found", name)
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
