package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	agentctl "github.com/TorstenDittmann/orch/internal/agent"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/control"
	"github.com/TorstenDittmann/orch/internal/discovery"
	"github.com/TorstenDittmann/orch/internal/ingress"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

// fakeOSUpdater stands in for apt-get and the reboot. A reboot restarts the
// agent, which then runs with a new boot id like after a real one.
type fakeOSUpdater struct {
	mu        sync.Mutex
	pending   bool
	fail      error
	upgrades  int
	reboots   int
	restartFn func()
}

func (f *fakeOSUpdater) Available() bool { return true }

func (f *fakeOSUpdater) Upgrade(context.Context) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upgrades++
	if f.fail != nil {
		return []byte("E: broken package"), f.fail
	}
	return []byte("1 upgraded, 0 newly installed"), nil
}

func (f *fakeOSUpdater) RebootRequired(context.Context) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending
}

func (f *fakeOSUpdater) Reboot(context.Context) error {
	f.mu.Lock()
	f.reboots++
	f.pending = false
	restart := f.restartFn
	f.mu.Unlock()
	go func() {
		time.Sleep(200 * time.Millisecond)
		restart()
	}()
	return nil
}

func TestNodeUpdateUpgradesRebootsAndReturnsTheNodeThroughControllerAndAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback controller/agent")
	}
	previousPoll := nodeUpdatePoll
	nodeUpdatePoll = 50 * time.Millisecond
	defer func() { nodeUpdatePoll = previousPoll }()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir, path := filepath.Join(root, "controller"), filepath.Join(root, "context.json")
	if _, err := clusterctl.Bootstrap(ctx, clusterctl.BootstrapOptions{Name: "test", Server: "https://" + address, DataDir: dir, ContextPath: path, Now: time.Now, Random: rand.Reader}); err != nil {
		t.Fatal(err)
	}
	server, err := control.Start(ctx, control.Options{DataDir: dir, Listen: address, BackupKeep: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown(context.Background())
	rewriteCLIContextServer(t, path, "https://"+server.Addr().String())
	agentDir := filepath.Join(root, "agent")
	if _, err := agentctl.Enroll(ctx, agentctl.EnrollOptions{Name: "node-one", ContextPath: path, DataDir: agentDir, Now: time.Now, Random: rand.Reader}); err != nil {
		t.Fatal(err)
	}
	updater := &fakeOSUpdater{pending: true}

	var agentMu sync.Mutex
	var stopAgent context.CancelFunc
	var done chan error
	start := func() {
		// Serve closes its runtime, so each boot gets a fresh one.
		runtime := orchruntime.NewFake()
		runtime.Capabilities.TotalCPUMillis = 4000
		runtime.Capabilities.TotalMemoryBytes = 8 << 30
		agentCtx, stop := context.WithCancel(ctx)
		finished := make(chan error, 1)
		go func() {
			finished <- agentctl.Serve(agentCtx, agentctl.ServeOptions{DataDir: agentDir, Snapshotter: "overlayfs", Runtime: runtime,
				Discovery: discovery.NewFake(), Ingress: &ingress.Fake{}, Now: time.Now, Random: rand.Reader, OSUpdater: updater})
		}()
		stopAgent, done = stop, finished
	}
	stop := func() {
		stopAgent()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("agent did not stop")
		}
	}
	updater.restartFn = func() {
		agentMu.Lock()
		defer agentMu.Unlock()
		stop()
		start()
	}
	agentMu.Lock()
	start()
	agentMu.Unlock()
	defer func() {
		agentMu.Lock()
		defer agentMu.Unlock()
		stop()
	}()

	remote, err := controlclient.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.CloseIdleConnections()
	node := func() *orchv1alpha1.Node {
		t.Helper()
		found, err := findNode(ctx, remote, "node-one")
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	for current := node(); current.GetConnectionStatus() != orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_CONNECTED ||
		!current.GetOsUpdates() || !current.GetRebootRequired(); current = node() {
		if err := sleepContext(ctx, 30*time.Millisecond); err != nil {
			t.Fatal("node did not connect and report a pending reboot")
		}
	}
	bootID := node().GetBootId()

	var usageOut, usageErr bytes.Buffer
	if code := RunContext(ctx, []string{"node", "usage", "node-one", "--json", "--context", path}, &usageOut, &usageErr); code != ExitOK {
		t.Fatalf("node usage code=%d err=%s", code, usageErr.String())
	}
	var usage struct {
		Node struct {
			Name           string `json:"name"`
			TotalCPUMillis uint64 `json:"total_cpu_millis"`
		} `json:"node"`
		StepSeconds uint32            `json:"step_seconds"`
		Samples     []json.RawMessage `json:"samples"`
	}
	if err := json.Unmarshal(usageOut.Bytes(), &usage); err != nil || usage.Node.Name != "node-one" || usage.Node.TotalCPUMillis != 4000 || usage.StepSeconds == 0 || usage.Samples == nil {
		t.Fatalf("node usage = %s (%v)", usageOut.String(), err)
	}

	var out, errOut bytes.Buffer
	if code := RunContext(ctx, []string{"node", "update", "--all", "--reboot-timeout", "30s", "--context", path}, &out, &errOut); code != ExitOK {
		t.Fatalf("node update code=%d out=%s err=%s", code, out.String(), errOut.String())
	}
	for _, want := range []string{"Cordoned node-one", "Upgrading packages on node-one", "Rebooting node-one", "Updated and rebooted node-one (lifecycle=ACTIVE)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	after := node()
	if after.GetBootId() == bootID || after.GetLifecycle() != orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_ACTIVE || after.GetRebootRequired() {
		t.Fatalf("after update: boot=%q (before %q) lifecycle=%s reboot_required=%t", after.GetBootId(), bootID, after.GetLifecycle(), after.GetRebootRequired())
	}

	// Nothing to reboot for: the node goes straight back to service.
	out.Reset()
	if code := RunContext(ctx, []string{"node", "update", "node-one", "--context", path}, &out, &errOut); code != ExitOK ||
		!strings.Contains(out.String(), "Updated node-one (lifecycle=ACTIVE)") {
		t.Fatalf("second update code=%d out=%s err=%s", code, out.String(), errOut.String())
	}

	// A failed upgrade is reported and leaves the node in service.
	updater.mu.Lock()
	updater.fail = errors.New("exit status 100")
	updater.mu.Unlock()
	out.Reset()
	errOut.Reset()
	if code := RunContext(ctx, []string{"node", "update", "node-one", "--context", path}, &out, &errOut); code != ExitRuntime ||
		!strings.Contains(errOut.String(), "broken package") {
		t.Fatalf("failed update code=%d out=%s err=%s", code, out.String(), errOut.String())
	}
	if lifecycle := node().GetLifecycle(); lifecycle != orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_ACTIVE {
		t.Fatalf("lifecycle after failed update = %s", lifecycle)
	}
	updater.mu.Lock()
	defer updater.mu.Unlock()
	if updater.upgrades != 3 || updater.reboots != 1 {
		t.Fatalf("upgrades=%d reboots=%d", updater.upgrades, updater.reboots)
	}
}

// fakeNodeUpdateClient drives updateNodes without a controller: a drain
// finishes on the next look, and an OS update answers right away.
type fakeNodeUpdateClient struct {
	nodes   []*orchv1alpha1.Node
	calls   []string
	failOS  map[string]error
	reboots map[string]bool
}

func (f *fakeNodeUpdateClient) ListNodes(context.Context) ([]*orchv1alpha1.Node, error) {
	return f.nodes, nil
}

func (f *fakeNodeUpdateClient) node(name string) *orchv1alpha1.Node {
	for _, node := range f.nodes {
		if node.GetName() == name {
			return node
		}
	}
	return nil
}

func (f *fakeNodeUpdateClient) UpdateNode(_ context.Context, request *orchv1alpha1.UpdateNodeRequest) (*orchv1alpha1.UpdateNodeResponse, error) {
	f.calls = append(f.calls, request.GetName()+" "+strings.TrimPrefix(request.GetLifecycle().String(), "NODE_LIFECYCLE_"))
	node := f.node(request.GetName())
	node.Lifecycle = request.GetLifecycle()
	return &orchv1alpha1.UpdateNodeResponse{Node: node, Changed: true}, nil
}

func (f *fakeNodeUpdateClient) UpdateNodeOS(_ context.Context, request *orchv1alpha1.UpdateNodeOSRequest) (*orchv1alpha1.UpdateNodeOSResponse, error) {
	f.calls = append(f.calls, request.GetName()+" os")
	if err := f.failOS[request.GetName()]; err != nil {
		return nil, err
	}
	node := f.node(request.GetName())
	if request.GetRestoreLifecycle() != orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_UNSPECIFIED {
		node.Lifecycle = request.GetRestoreLifecycle()
	}
	old := node.GetBootId()
	if f.reboots[request.GetName()] {
		node.BootId = old + "-rebooted"
	}
	return &orchv1alpha1.UpdateNodeOSResponse{Rebooting: f.reboots[request.GetName()], BootId: old}, nil
}

func testUpdateNode(name string, lifecycle orchv1alpha1.NodeLifecycle, ingress, controller bool) *orchv1alpha1.Node {
	return &orchv1alpha1.Node{Id: "id-" + name, Name: name, Lifecycle: lifecycle, Ingress: ingress, ControllerHost: controller,
		ConnectionStatus: orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_CONNECTED, OsUpdates: true, BootId: "boot-" + name}
}

func TestUpdateNodesOrdersNodesAndStopsAtTheFirstFailure(t *testing.T) {
	previousPoll := nodeUpdatePoll
	nodeUpdatePoll = time.Millisecond
	defer func() { nodeUpdatePoll = previousPoll }()
	active, cordoned := orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_ACTIVE, orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_CORDONED
	remote := &fakeNodeUpdateClient{
		nodes: []*orchv1alpha1.Node{
			testUpdateNode("control", active, true, true),
			testUpdateNode("edge", active, true, false),
			testUpdateNode("worker-b", cordoned, false, false),
			testUpdateNode("worker-a", active, false, false),
		},
		reboots: map[string]bool{"worker-a": true},
	}
	var progress bytes.Buffer
	results, err := updateNodes(context.Background(), remote, nil, nodeUpdateOptions{drainTimeout: time.Second, rebootTimeout: time.Second}, &progress)
	if err != nil {
		t.Fatalf("updateNodes: %v\n%s", err, progress.String())
	}
	want := []string{
		"worker-b DRAINING", "worker-b os",
		"worker-a DRAINING", "worker-a os",
		"edge DRAINING", "edge os",
		"control DRAINING", "control os",
	}
	if strings.Join(remote.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q", remote.calls)
	}
	if len(results) != 4 || !results[1].Rebooted || results[0].Lifecycle != "CORDONED" || results[3].Lifecycle != "ACTIVE" {
		t.Fatalf("results = %+v", results)
	}

	// A failure stops the run, and the node goes back to how it was.
	remote.calls = nil
	remote.failOS = map[string]error{"worker-a": errors.New("dpkg was interrupted")}
	progress.Reset()
	if _, err := updateNodes(context.Background(), remote, nil, nodeUpdateOptions{drainTimeout: time.Second, rebootTimeout: time.Second}, &progress); err == nil ||
		!strings.Contains(err.Error(), "dpkg was interrupted") {
		t.Fatalf("err = %v", err)
	}
	want = []string{"worker-b DRAINING", "worker-b os", "worker-a DRAINING", "worker-a os", "worker-a ACTIVE"}
	if strings.Join(remote.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls after failure = %q", remote.calls)
	}

	// Nodes that cannot be updated are skipped and reported.
	remote.calls, remote.failOS = nil, nil
	remote.node("edge").ConnectionStatus = orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_UNREACHABLE
	if _, err := updateNodes(context.Background(), remote, []string{"edge"}, nodeUpdateOptions{drainTimeout: time.Second, rebootTimeout: time.Second}, &progress); err == nil ||
		!strings.Contains(err.Error(), "skipped edge") || len(remote.calls) != 0 {
		t.Fatalf("err = %v calls = %q", err, remote.calls)
	}
}
