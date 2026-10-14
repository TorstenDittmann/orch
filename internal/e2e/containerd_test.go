//go:build linux

package e2e

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	agentctl "change.codes/torsten/better-swarm/internal/agent"
	controlclient "change.codes/torsten/better-swarm/internal/client"
	clusterctl "change.codes/torsten/better-swarm/internal/cluster"
	"change.codes/torsten/better-swarm/internal/config"
	"change.codes/torsten/better-swarm/internal/control"
	"change.codes/torsten/better-swarm/internal/discovery"
	"change.codes/torsten/better-swarm/internal/ingress"
	"change.codes/torsten/better-swarm/internal/network"
	orchruntime "change.codes/torsten/better-swarm/internal/runtime"
	containerdruntime "change.codes/torsten/better-swarm/internal/runtime/containerd"
	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/miekg/dns"
	"google.golang.org/protobuf/proto"
)

const (
	e2eApplication   = "runtime-e2e"
	e2eIngressDomain = "runtime-e2e.orch.invalid"
	// e2eNodePortApplication publishes node-local ports after the main
	// application has been removed.
	e2eNodePortApplication = "node-ports-e2e"
	managedLabel           = "io.orch.managed"
	clusterLabel           = "io.orch.cluster"
	nodeLabel              = "io.orch.node"
	allocationLabel        = "io.orch.allocation"
	specLabel              = "io.orch.spec-digest"
)

func TestContainerdSingleNodeLifecycle(t *testing.T) {
	if os.Getenv("ORCH_CONTAINERD_E2E") != "1" {
		t.Skip("set ORCH_CONTAINERD_E2E=1 and use scripts/containerd-e2e.sh on a dedicated Linux host")
	}
	if os.Geteuid() != 0 {
		t.Fatal("the gated containerd lifecycle test requires root")
	}
	socket := requiredEnvironment(t, "ORCH_CONTAINERD_SOCKET")
	snapshotter := requiredEnvironment(t, "ORCH_CONTAINERD_SNAPSHOTTER")
	cniBinDir := requiredEnvironment(t, "ORCH_E2E_CNI_BIN_DIR")
	archivePath := requiredRegularFile(t, "ORCH_E2E_IMAGE_ARCHIVE", false)
	imageReference := requiredEnvironment(t, "ORCH_E2E_IMAGE_REFERENCE")
	orchBinary := requiredRegularFile(t, "ORCH_E2E_ORCH_BINARY", true)
	caddyBinary := requiredRegularFile(t, "ORCH_CADDY_BINARY", true)
	caddyUser := requiredEnvironment(t, "ORCH_CADDY_USER")
	ingressDataDir := requiredEnvironment(t, "ORCH_E2E_INGRESS_DATA_DIR")
	workloadCIDR := requiredEnvironment(t, "ORCH_E2E_WORKLOAD_CIDR")
	outboundURL := requiredEnvironment(t, "ORCH_E2E_OUTBOUND_URL")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	address := reserveLoopbackAddress(t)
	controllerDir := filepath.Join(root, "controller")
	contextPath := filepath.Join(root, "operator", "context.json")
	bootstrap, err := clusterctl.Bootstrap(ctx, clusterctl.BootstrapOptions{
		Name: "runtime-e2e", Server: "https://" + address, DataDir: controllerDir,
		ContextPath: contextPath, Now: time.Now, Random: rand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	namespace := "orch-" + bootstrap.Cluster.ID
	if path := os.Getenv("ORCH_E2E_NAMESPACE_FILE"); path != "" {
		if err := os.WriteFile(path, []byte(namespace+"\n"), 0o600); err != nil {
			t.Fatalf("write cleanup namespace: %v", err)
		}
	}

	raw, err := containerdclient.New(socket,
		containerdclient.WithDefaultNamespace(namespace),
		containerdclient.WithDefaultPlatform(platforms.DefaultStrict()),
		containerdclient.WithTimeout(15*time.Second),
	)
	if err != nil {
		t.Fatalf("connect to containerd: %v", err)
	}
	if err := raw.NamespaceService().Create(ctx, namespace, map[string]string{
		managedLabel: "true", clusterLabel: bootstrap.Cluster.ID,
	}); err != nil && !errdefs.IsAlreadyExists(err) {
		_ = raw.Close()
		t.Fatalf("create containerd namespace: %v", err)
	}
	if err := importFixture(ctx, raw, archivePath, imageReference); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	foreignID := "foreign-e2e-object"
	if _, err := raw.NewContainer(ctx, foreignID, containerdclient.WithContainerLabels(map[string]string{
		"io.example.owner": "foreign",
	})); err != nil {
		_ = raw.Close()
		t.Fatalf("create foreign ownership sentinel: %v", err)
	}

	var server *control.Server
	var remote *controlclient.Client
	var running *agentProcess
	t.Cleanup(func() {
		if running != nil {
			running.stop(t)
		}
		if remote != nil {
			remote.CloseIdleConnections()
		}
		if server != nil {
			shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := server.Shutdown(shutdownContext); err != nil {
				t.Errorf("shutdown controller: %v", err)
			}
			shutdownCancel()
		}
		cleanupContainerdNamespace(t, raw, namespace)
		if err := raw.Close(); err != nil {
			t.Errorf("close containerd client: %v", err)
		}
	})

	server, err = control.Start(ctx, control.Options{DataDir: controllerDir, Listen: address})
	if err != nil {
		t.Fatal(err)
	}
	remote, err = controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	configured, err := remote.ConfigureNetwork(ctx, workloadCIDR, "containerd-e2e-network-one")
	if err != nil || !configured.GetChanged() || configured.GetCidr() != workloadCIDR || configured.GetGeneration() != 2 {
		t.Fatalf("configure network = %#v, %v", configured, err)
	}
	agentDir := filepath.Join(root, "agent")
	t.Cleanup(func() {
		if err := copyNetworkCleanupState(agentDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("preserve network cleanup state: %v", err)
		}
	})
	enrolled, err := agentctl.Enroll(ctx, agentctl.EnrollOptions{
		Name: "node-1", ContextPath: contextPath, DataDir: agentDir,
		Now: time.Now, Random: rand.Reader,
	})
	if err != nil || !enrolled.Created {
		t.Fatalf("enroll agent = %#v, %v", enrolled, err)
	}
	running = startAgent(t, ctx, agentDir, socket, snapshotter, cniBinDir, orchBinary,
		caddyBinary, caddyUser, ingressDataDir, enrolled.Config)
	waitForNode(t, ctx, remote, running, orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_CONNECTED)

	created, err := remote.ApplyApplication(ctx, runtimeManifest(t, imageReference, "worker-v1"), "containerd-e2e-deploy-one")
	if err != nil {
		t.Fatal(err)
	}
	if !created.GetChanged() || created.GetGeneration() != 1 {
		t.Fatalf("initial apply = %#v", created)
	}
	application := waitForApplication(t, ctx, remote, running, 1, orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_HEALTHY)
	writeNetworkCleanupState(t, agentDir)
	foreignCNIState := createForeignCNIState(t, agentDir)
	initialCaddyPID := readCaddyPID(t, ingressDataDir)
	if len(application.GetAllocations()) != 3 {
		t.Fatalf("running allocations = %d, want 3", len(application.GetAllocations()))
	}
	initialPIDs := applicationPIDs(t, ctx, raw, application)

	worker := allocation(t, application, "worker", 1)
	stableOne := allocation(t, application, "stable", 1)
	stableTwo := allocation(t, application, "stable", 2)
	if stableOne.GetHealthStatus() != orchv1alpha1.HealthStatus_HEALTH_STATUS_READY ||
		stableTwo.GetHealthStatus() != orchv1alpha1.HealthStatus_HEALTH_STATUS_READY ||
		stableOne.GetAddress() == "" || stableTwo.GetAddress() == "" || stableOne.GetAddress() == stableTwo.GetAddress() {
		t.Fatalf("healthy stable allocations = %#v, %#v", stableOne, stableTwo)
	}
	assertContainerContract(t, ctx, raw, bootstrap.Cluster.ID, enrolled.Config.NodeID, agentDir, worker, "worker-v1", snapshotter)
	waitForLogs(t, ctx, remote, running, "worker-v1")
	stdout, stderr, exitCode := execute(t, ctx, remote, e2eApplication, "worker", 1, []string{"/orch-fixture", "exec"})
	if stdout != "exec stdout token=worker-v1\n" || stderr != "exec stderr token=worker-v1\n" || exitCode != 0 {
		t.Fatalf("exec output = stdout %q stderr %q exit %d", stdout, stderr, exitCode)
	}
	stdout, stderr, exitCode = execute(t, ctx, remote, e2eApplication, "worker", 1, []string{"/orch-fixture", "network"})
	if stdout != "interfaces=eth0,lo\n" || stderr != "" || exitCode != 0 {
		t.Fatalf("network namespace output = stdout %q stderr %q exit %d", stdout, stderr, exitCode)
	}
	stdout, stderr, exitCode = execute(t, ctx, remote, e2eApplication, "worker", 1,
		[]string{"/orch-fixture", "resolve", "worker." + e2eApplication + ".svc.internal"})
	if stdout != "addresses="+worker.GetAddress()+"\n" || stderr != "" || exitCode != 0 {
		t.Fatalf("service DNS output = stdout %q stderr %q exit %d", stdout, stderr, exitCode)
	}
	stdout, stderr, exitCode = execute(t, ctx, remote, e2eApplication, "worker", 1,
		[]string{"/orch-fixture", "resolve", "stable"})
	stableAddresses := []string{stableOne.GetAddress(), stableTwo.GetAddress()}
	sort.Strings(stableAddresses)
	assertIngressBackends(t, e2eIngressDomain, stableAddresses, "stable-v1")
	if stdout != "addresses="+strings.Join(stableAddresses, ",")+"\n" || stderr != "" || exitCode != 0 {
		t.Fatalf("short service DNS output = stdout %q stderr %q exit %d", stdout, stderr, exitCode)
	}
	stdout, stderr, exitCode = execute(t, ctx, remote, e2eApplication, "worker", 1,
		[]string{"/orch-fixture", "fetch", outboundURL})
	if stdout != "status=200 body=token=outbound\n" || stderr != "" || exitCode != 0 {
		t.Fatalf("outbound masquerade output = stdout %q stderr %q exit %d", stdout, stderr, exitCode)
	}
	stdout, stderr, exitCode = execute(t, ctx, remote, e2eApplication, "worker", 1,
		[]string{"/orch-fixture", "resolve", "stable." + e2eApplication + ".svc.internal"})
	if stdout != "addresses="+strings.Join(stableAddresses, ",")+"\n" || stderr != "" || exitCode != 0 {
		t.Fatalf("multi-endpoint workload DNS output = stdout %q stderr %q exit %d", stdout, stderr, exitCode)
	}
	nodes, err := remote.ListNodes(ctx)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("list node for DNS assertion = %#v, %v", nodes, err)
	}
	assertDNSRecords(t, nodes[0].GetNodeSubnet(), "stable."+e2eApplication+".svc.internal.", stableAddresses)
	stdout, stderr, exitCode = execute(t, ctx, remote, e2eApplication, "worker", 1, []string{"/orch-fixture", "resources"})
	if stdout != "cpu.max=25000 100000\nmemory.max=67108864\nmemory.swap.max=0\n" || stderr != "" || exitCode != 0 {
		t.Fatalf("live cgroup limits = stdout %q stderr %q exit %d", stdout, stderr, exitCode)
	}
	stdout, _, exitCode = execute(t, ctx, remote, e2eApplication, "stable", 2, []string{"/orch-fixture", "memory", "128"})
	if exitCode == 0 || strings.Contains(stdout, "allocated=128MiB") {
		t.Fatalf("memory allocation escaped the 32 MiB task cgroup: stdout %q exit %d", stdout, exitCode)
	}
	waitForTaskRunning(t, ctx, raw, orchruntime.ObjectID(stableTwo.GetId(), stableTwo.GetSpecDigest()))
	assertForeignResource(t, ctx, raw, foreignID)

	beforeControllerRestart := applicationPIDs(t, ctx, raw, application)
	shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := server.Shutdown(shutdownContext); err != nil {
		shutdownCancel()
		t.Fatal(err)
	}
	shutdownCancel()
	server = nil
	assertPIDs(t, ctx, raw, application, beforeControllerRestart)
	assertDNSRecords(t, nodes[0].GetNodeSubnet(), "stable."+e2eApplication+".svc.internal.", stableAddresses)
	assertIngressBackends(t, e2eIngressDomain, stableAddresses, "stable-v1")
	server, err = control.Start(ctx, control.Options{DataDir: controllerDir, Listen: address})
	if err != nil {
		t.Fatal(err)
	}
	remote.CloseIdleConnections()
	remote, err = controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	waitForNode(t, ctx, remote, running, orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_CONNECTED)
	application = waitForApplication(t, ctx, remote, running, 1, orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_HEALTHY)
	assertPIDs(t, ctx, raw, application, beforeControllerRestart)
	if pid := readCaddyPID(t, ingressDataDir); pid != initialCaddyPID {
		t.Fatalf("controller restart changed Caddy PID: %d -> %d", initialCaddyPID, pid)
	}

	stable := allocation(t, application, "stable", 1)
	stableID := orchruntime.ObjectID(stable.GetId(), stable.GetSpecDigest())
	oldStablePID, err := taskPID(ctx, raw, stableID)
	if err != nil {
		t.Fatal(err)
	}
	stableContainer, err := raw.LoadContainer(ctx, stableID)
	if err != nil {
		t.Fatal(err)
	}
	stableTask, err := stableContainer.Task(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	wait, err := stableTask.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stableTask.Kill(ctx, syscall.SIGKILL, containerdclient.WithKillAll); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-wait:
	}
	application = waitForRestart(t, ctx, remote, running, raw, "stable", 1, oldStablePID)

	beforeAgentRestart := applicationPIDs(t, ctx, raw, application)
	running.stop(t)
	assertIngressBackends(t, e2eIngressDomain, stableAddresses, "stable-v1")
	running = nil
	waitForNode(t, ctx, remote, nil, orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_DISCONNECTED)
	assertPIDs(t, ctx, raw, application, beforeAgentRestart)
	running = startAgent(t, ctx, agentDir, socket, snapshotter, cniBinDir, orchBinary,
		caddyBinary, caddyUser, ingressDataDir, enrolled.Config)
	waitForNode(t, ctx, remote, running, orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_CONNECTED)
	application = waitForApplication(t, ctx, remote, running, 1, orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_HEALTHY)
	assertPIDs(t, ctx, raw, application, beforeAgentRestart)
	if pid := readCaddyPID(t, ingressDataDir); pid != initialCaddyPID {
		t.Fatalf("agent restart did not adopt Caddy: %d -> %d", initialCaddyPID, pid)
	}
	worker = allocation(t, application, "worker", 1)
	currentStableAddresses := []string{
		allocation(t, application, "stable", 1).GetAddress(),
		allocation(t, application, "stable", 2).GetAddress(),
	}
	sort.Strings(currentStableAddresses)
	stdout, stderr, exitCode = execute(t, ctx, remote, e2eApplication, "worker", 1,
		[]string{"/orch-fixture", "resolve", "stable"})
	if stdout != "addresses="+strings.Join(currentStableAddresses, ",")+"\n" || stderr != "" || exitCode != 0 {
		t.Fatalf("DNS after agent restart = stdout %q stderr %q exit %d", stdout, stderr, exitCode)
	}

	stablePIDs := servicePIDs(t, ctx, raw, application, "stable")
	oldWorkerID := orchruntime.ObjectID(worker.GetId(), worker.GetSpecDigest())
	updated, err := remote.ApplyApplication(ctx, runtimeManifest(t, imageReference, "worker-v2"), "containerd-e2e-deploy-two")
	if err != nil {
		t.Fatal(err)
	}
	if !updated.GetChanged() || updated.GetGeneration() != 2 {
		t.Fatalf("updated apply = %#v", updated)
	}
	application = waitForApplication(t, ctx, remote, running, 2, orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_HEALTHY)
	assertServicePIDs(t, ctx, raw, application, "stable", stablePIDs)
	worker = allocation(t, application, "worker", 1)
	newWorkerID := orchruntime.ObjectID(worker.GetId(), worker.GetSpecDigest())
	if newWorkerID == oldWorkerID {
		t.Fatalf("worker runtime object did not change after a runtime-affecting update: %s", newWorkerID)
	}
	if _, err := raw.LoadContainer(ctx, oldWorkerID); !errdefs.IsNotFound(err) {
		t.Fatalf("obsolete worker container still exists: %v", err)
	}
	assertContainerContract(t, ctx, raw, bootstrap.Cluster.ID, enrolled.Config.NodeID, agentDir, worker, "worker-v2", snapshotter)
	waitForLogs(t, ctx, remote, running, "worker-v2")
	stdout, stderr, exitCode = execute(t, ctx, remote, e2eApplication, "worker", 1, []string{"/orch-fixture", "exec"})
	if stdout != "exec stdout token=worker-v2\n" || stderr != "exec stderr token=worker-v2\n" || exitCode != 0 {
		t.Fatalf("updated exec output = stdout %q stderr %q exit %d", stdout, stderr, exitCode)
	}
	volumePath := filepath.Join(agentDir, agentctl.VolumesDirectory, e2eApplication, "state", "starts")
	waitForFile(t, ctx, volumePath, "worker-v1\nworker-v2\n")

	trafficReady, stopTraffic, trafficDone := startContinuousServiceTraffic(ctx)
	select {
	case <-trafficReady:
	case <-time.After(15 * time.Second):
		stopTraffic()
		t.Fatal("continuous service traffic did not start")
	}
	healthyUpdate, err := remote.ApplyApplication(ctx,
		runtimeManifestWithStableRevision(t, imageReference, "worker-v2", "stable-v2", false),
		"containerd-e2e-deploy-three")
	if err != nil {
		stopTraffic()
		t.Fatal(err)
	}
	if !healthyUpdate.GetChanged() || healthyUpdate.GetGeneration() != 3 {
		stopTraffic()
		t.Fatalf("healthy routed apply = %#v", healthyUpdate)
	}
	application = waitForApplication(t, ctx, remote, running, 3,
		orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_HEALTHY)
	stopTraffic()
	traffic := <-trafficDone
	if traffic.err != nil || traffic.requests < 2 {
		t.Fatalf("continuous service traffic = %d requests, %v", traffic.requests, traffic.err)
	}
	healthyStablePIDs := servicePIDs(t, ctx, raw, application, "stable")
	healthyStableAddresses := serviceAddresses(t, application, "stable")
	assertIngressBackends(t, e2eIngressDomain, healthyStableAddresses, "stable-v2")
	if reflect.DeepEqual(healthyStablePIDs, stablePIDs) {
		t.Fatalf("healthy stable revision was not rolled out: %#v", healthyStablePIDs)
	}

	rolledBack, err := remote.ApplyApplication(ctx,
		runtimeManifestWithStableRevision(t, imageReference, "worker-v2", "stable-bad", true),
		"containerd-e2e-deploy-four")
	if err != nil {
		t.Fatal(err)
	}
	if !rolledBack.GetChanged() || rolledBack.GetGeneration() != 4 {
		t.Fatalf("unhealthy apply = %#v", rolledBack)
	}
	application = waitForApplication(t, ctx, remote, running, 4,
		orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_ROLLED_BACK)
	for _, item := range application.GetAllocations() {
		if item.GetActiveApplicationGeneration() != 3 || item.GetActiveRevisionId() == "" ||
			item.GetActiveSpecDigest() == "" {
			t.Fatalf("rollback did not restore generation three: %#v", item)
		}
		if item.GetActiveSpecDigest() != item.GetSpecDigest() {
			candidateID := orchruntime.ObjectID(item.GetId(), item.GetSpecDigest())
			if _, err := raw.LoadContainer(ctx, candidateID); !errdefs.IsNotFound(err) {
				t.Fatalf("failed candidate container %s remains after rollback: %v", candidateID, err)
			}
		}
	}
	assertActiveServicePIDs(t, ctx, raw, application, "stable", healthyStablePIDs)
	assertIngressBackends(t, e2eIngressDomain, serviceAddresses(t, application, "stable"), "stable-v2")

	removed, err := remote.RemoveApplication(ctx, e2eApplication, "containerd-e2e-remove-one")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := remote.RemoveApplication(ctx, e2eApplication, "containerd-e2e-remove-one")
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(removed, replayed) {
		t.Fatalf("idempotent remove replay differs: first %#v replay %#v", removed, replayed)
	}
	application = waitForApplication(t, ctx, remote, running, 4, orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_REMOVED)
	if !application.GetDeleted() {
		t.Fatal("removed application did not retain its tombstoned history")
	}
	unchanged, err := remote.RemoveApplication(ctx, e2eApplication, "containerd-e2e-remove-two")
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.GetChanged() {
		t.Fatalf("second logical removal unexpectedly changed state: %#v", unchanged)
	}
	waitForFile(t, ctx, volumePath, "worker-v1\nworker-v2\n")
	assertNoManagedResources(t, ctx, raw, bootstrap.Cluster.ID, snapshotter, filepath.Join(agentDir, agentctl.LogsDirectory))
	assertForeignResource(t, ctx, raw, foreignID)
	assertIngressWithdrawn(t, e2eIngressDomain)
	assertDNSRcode(t, nodes[0].GetNodeSubnet(), "stable."+e2eApplication+".svc.internal.", dns.RcodeNameError)
	if pid := readCaddyPID(t, ingressDataDir); pid != initialCaddyPID {
		t.Fatalf("application removal changed Caddy PID: %d -> %d", initialCaddyPID, pid)
	}
	if contents, err := os.ReadFile(foreignCNIState); err != nil || string(contents) != foreignCNIContents {
		t.Fatalf("foreign CNI state was changed or removed: %q, %v", contents, err)
	}

	if len(initialPIDs) != 3 {
		t.Fatalf("initial task count = %d, want 3", len(initialPIDs))
	}

	assertNodePortPublishing(t, ctx, remote, running, imageReference)
	assertNoManagedResources(t, ctx, raw, bootstrap.Cluster.ID, snapshotter, filepath.Join(agentDir, agentctl.LogsDirectory))
	assertForeignResource(t, ctx, raw, foreignID)
}

// assertNodePortPublishing deploys a service with node-local TCP and UDP
// ports and reaches both from a separate network namespace through the host
// address. The workload must see the client's own address, not the node's.
func assertNodePortPublishing(t *testing.T, ctx context.Context, remote *controlclient.Client, process *agentProcess, imageReference string) {
	t.Helper()
	namespace := requiredEnvironment(t, "ORCH_E2E_EXTERNAL_NAMESPACE")
	hostAddress := requiredEnvironment(t, "ORCH_E2E_EXTERNAL_HOST_ADDRESS")
	clientAddress := requiredEnvironment(t, "ORCH_E2E_EXTERNAL_CLIENT_ADDRESS")
	fixture := requiredRegularFile(t, "ORCH_E2E_FIXTURE_BINARY", true)
	tcpPort, udpPort := 18787, 18790
	manifest := &config.Manifest{
		Schema: config.SchemaVersion, Name: e2eNodePortApplication,
		Services: map[string]config.Service{"game": {
			Image: imageReference, Command: []string{"/orch-fixture"}, Args: []string{"serve", "game"},
			Replicas: 1, Restart: "always", Resources: config.Resources{CPU: "125m", Memory: "32Mi"},
			Env: map[string]string{"ORCH_E2E_TOKEN": "game", "ORCH_E2E_HTTP_PORT": "8787", "ORCH_E2E_UDP_PORT": "8790"},
			TCP: &config.TCP{Port: 8787, NodePort: &tcpPort},
			UDP: &config.UDP{Port: 8790, NodePort: udpPort},
		}},
		Deploy: config.Deployment{MaxSurge: 0, MaxUnavailable: 1, HealthDeadline: "30s", AutoRollback: true},
	}
	if err := config.Validate(manifest); err != nil {
		t.Fatal(err)
	}
	canonical, err := config.CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.ApplyApplication(ctx, canonical, "containerd-e2e-node-ports"); err != nil {
		t.Fatal(err)
	}
	waitForNamedApplication(t, ctx, remote, process, e2eNodePortApplication, 1, orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_HEALTHY)
	external := func(args ...string) (string, error) {
		output, err := exec.CommandContext(ctx, "ip", append([]string{"netns", "exec", namespace, fixture}, args...)...).Output()
		return string(output), err
	}
	want := "source=" + clientAddress + "\n"
	deadline := time.Now().Add(45 * time.Second)
	var application *orchv1alpha1.Application
	var udpReply, tcpReply string
	for {
		requestContext, cancel := context.WithTimeout(ctx, 2*time.Second)
		application, _ = remote.GetApplication(requestContext, e2eNodePortApplication)
		cancel()
		udpReply, _ = external("udp-probe", net.JoinHostPort(hostAddress, strconv.Itoa(udpPort)))
		tcpReply, _ = external("fetch", "http://"+net.JoinHostPort(hostAddress, strconv.Itoa(tcpPort))+"/source")
		active := 0
		for _, port := range application.GetNodePorts() {
			if port.GetState() == orchv1alpha1.RouteState_ROUTE_STATE_ACTIVE {
				active++
			}
		}
		if active == 2 && udpReply == want && tcpReply == "status=200 body="+want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node ports were not reachable with client addresses: ports=%#v udp=%q tcp=%q", application.GetNodePorts(), udpReply, tcpReply)
		}
		time.Sleep(200 * time.Millisecond)
	}

	if _, err := remote.RemoveApplication(ctx, e2eNodePortApplication, "containerd-e2e-node-ports-remove"); err != nil {
		t.Fatal(err)
	}
	waitForNamedApplication(t, ctx, remote, process, e2eNodePortApplication, 1, orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_REMOVED)
	if reply, err := external("udp-probe", net.JoinHostPort(hostAddress, strconv.Itoa(udpPort))); err == nil {
		t.Fatalf("withdrawn UDP node port still answered: %q", reply)
	}
}

func writeNetworkCleanupState(t *testing.T, agentDir string) {
	t.Helper()
	if err := copyNetworkCleanupState(agentDir); err != nil {
		t.Fatalf("write network cleanup state: %v", err)
	}
}

func copyNetworkCleanupState(agentDir string) error {
	destination := os.Getenv("ORCH_E2E_NETWORK_FILE")
	if destination == "" {
		return nil
	}
	contents, err := os.ReadFile(filepath.Join(agentDir, "network", "firewall-owner.json"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(destination, contents, 0o600); err != nil {
		return err
	}
	return nil
}

func createForeignCNIState(t *testing.T, agentDir string) string {
	t.Helper()
	resultsDir := filepath.Join(agentDir, "network", "cni-cache", "results")
	if err := os.MkdirAll(resultsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(resultsDir, "foreign-network-foreign-container-eth9")
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refusing to replace foreign CNI sentinel %s: %v", path, err)
	}
	// This is a valid libcni cache identity for a different network. It proves
	// orch's CNI garbage collection does not sweep unrelated attachment state.
	if err := os.WriteFile(path, []byte(foreignCNIContents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const foreignCNIContents = "{\"kind\":\"cniCacheV1\",\"containerId\":\"foreign-container\",\"ifName\":\"eth9\",\"networkName\":\"foreign-network\"}\n"

type agentProcess struct {
	cancel context.CancelFunc
	done   chan error
}

func readCaddyPID(t *testing.T, ingressDataDir string) int {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(ingressDataDir, "caddy.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(contents)))
	if err != nil || pid < 2 {
		t.Fatalf("invalid Caddy PID %q: %v", contents, err)
	}
	return pid
}

func startAgent(t *testing.T, parent context.Context, dataDir, socket, snapshotter, cniBinDir, orchBinary,
	caddyBinary, caddyUser, ingressDataDir string, config agentctl.Config,
) *agentProcess {
	t.Helper()
	networkManager, err := network.New(network.Options{
		ClusterID: config.ClusterID, NodeID: config.NodeID, BinDir: cniBinDir,
		StateDir: filepath.Join(dataDir, "network"),
	})
	if err != nil {
		t.Fatal(err)
	}
	runtimeClient, err := containerdruntime.New(socket, orchruntime.Options{
		ClusterID: config.ClusterID, NodeID: config.NodeID, Snapshotter: snapshotter,
		LogsDir:    filepath.Join(dataDir, agentctl.LogsDirectory),
		VolumesDir: filepath.Join(dataDir, agentctl.VolumesDirectory), LoggerPath: orchBinary,
		ResolverPath: filepath.Join(dataDir, "network", "resolv.conf"), Network: networkManager,
	})
	if err != nil {
		_ = networkManager.Close()
		t.Fatal(err)
	}
	forwarders, err := discovery.SystemForwarders("/etc/resolv.conf")
	if err != nil {
		_ = runtimeClient.Close()
		t.Fatal(err)
	}
	dnsServer, err := discovery.New(discovery.DefaultZone, forwarders)
	if err != nil {
		_ = runtimeClient.Close()
		t.Fatal(err)
	}
	caddy, err := ingress.NewCaddy(ingress.CaddyOptions{
		Binary: caddyBinary, AdminSocket: filepath.Join(ingressDataDir, "admin.sock"),
		DataDir: ingressDataDir, StateDir: filepath.Join(dataDir, "ingress"), User: caddyUser,
	})
	if err != nil {
		_ = runtimeClient.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(parent)
	process := &agentProcess{cancel: cancel, done: make(chan error, 1)}
	go func() {
		process.done <- agentctl.Serve(ctx, agentctl.ServeOptions{
			DataDir: dataDir, Snapshotter: snapshotter, Runtime: runtimeClient,
			Discovery: dnsServer, Ingress: caddy, Now: time.Now, Random: rand.Reader,
		})
	}()
	return process
}

func (process *agentProcess) stop(t *testing.T) {
	t.Helper()
	if process == nil || process.done == nil {
		return
	}
	process.cancel()
	select {
	case err := <-process.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("agent exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("agent did not exit within 10 seconds")
	}
	process.done = nil
}

func (process *agentProcess) failure() error {
	if process == nil || process.done == nil {
		return nil
	}
	select {
	case err := <-process.done:
		process.done = nil
		if err == nil {
			return errors.New("agent exited unexpectedly")
		}
		return err
	default:
		return nil
	}
}

func importFixture(ctx context.Context, client *containerdclient.Client, path, reference string) error {
	archive, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open OCI fixture: %w", err)
	}
	defer archive.Close()
	imported, err := client.Import(ctx, archive, containerdclient.WithImportPlatform(platforms.DefaultStrict()))
	if err != nil {
		return fmt.Errorf("import OCI fixture: %w", err)
	}
	for _, image := range imported {
		if image.Name == reference && strings.HasSuffix(reference, "@"+image.Target.Digest.String()) {
			return nil
		}
	}
	return fmt.Errorf("OCI fixture did not import exact image reference %q: %#v", reference, imported)
}

func runtimeManifest(t *testing.T, imageReference, workerToken string) []byte {
	t.Helper()
	return runtimeManifestWithStableRevision(t, imageReference, workerToken, "stable-v1", false)
}

func runtimeManifestWithStableRevision(t *testing.T, imageReference, workerToken, stableRevision string, failHealth bool) []byte {
	t.Helper()
	healthFailure := "0"
	if failHealth {
		healthFailure = "1"
	}
	manifest := &config.Manifest{
		Schema: config.SchemaVersion,
		Name:   e2eApplication,
		Services: map[string]config.Service{
			"stable": {
				Image: imageReference, Command: []string{"/orch-fixture"}, Args: []string{"serve", "stable"},
				Replicas: 2, Restart: "always", Resources: config.Resources{CPU: "125m", Memory: "32Mi"},
				Env: map[string]string{
					"ORCH_E2E_TOKEN": "stable", "ORCH_E2E_HTTP_PORT": "8080",
					"ORCH_E2E_REVISION": stableRevision, "ORCH_E2E_HEALTH_FAIL": healthFailure,
				},
				HTTP: &config.HTTP{Port: 8080, Domains: []string{e2eIngressDomain}, HealthPath: "/healthz", HTTPS: false},
			},
			"worker": {
				Image: imageReference, Command: []string{"/orch-fixture"}, Args: []string{"serve", workerToken},
				Replicas: 1, Restart: "always", Resources: config.Resources{CPU: "250m", Memory: "64Mi"},
				Env:    map[string]string{"ORCH_E2E_TOKEN": workerToken, "ORCH_E2E_START_FILE": "/data/starts"},
				Mounts: []config.Mount{{Source: "state", Target: "/data"}},
			},
		},
		Volumes: map[string]config.Volume{"state": {Driver: "local"}},
		Deploy: config.Deployment{
			MaxSurge: 1, MaxUnavailable: 0, HealthDeadline: "30s", AutoRollback: true,
		},
	}
	if err := config.Validate(manifest); err != nil {
		t.Fatal(err)
	}
	canonical, err := config.CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func assertDNSRecords(t *testing.T, nodeSubnet, name string, want []string) {
	t.Helper()
	prefix, err := netip.ParsePrefix(nodeSubnet)
	if err != nil || !prefix.Addr().Is4() {
		t.Fatalf("node subnet = %q, %v", nodeSubnet, err)
	}
	server := net.JoinHostPort(prefix.Addr().Next().String(), "53")
	for _, protocol := range []string{"udp", "tcp"} {
		request := new(dns.Msg)
		request.SetQuestion(name, dns.TypeA)
		response, _, err := (&dns.Client{Net: protocol, Timeout: 2 * time.Second}).Exchange(request, server)
		if err != nil {
			t.Fatalf("%s DNS query failed: %v", protocol, err)
		}
		if response.Rcode != dns.RcodeSuccess {
			t.Fatalf("%s DNS response code = %s", protocol, dns.RcodeToString[response.Rcode])
		}
		got := make([]string, 0, len(response.Answer))
		for _, answer := range response.Answer {
			record, ok := answer.(*dns.A)
			if !ok || record.Hdr.Ttl != discovery.RecordTTL {
				t.Fatalf("%s DNS answer = %#v", protocol, answer)
			}
			got = append(got, record.A.String())
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s DNS addresses = %v, want %v", protocol, got, want)
		}
	}
}

func assertDNSRcode(t *testing.T, nodeSubnet, name string, want int) {
	t.Helper()
	prefix, err := netip.ParsePrefix(nodeSubnet)
	if err != nil || !prefix.Addr().Is4() {
		t.Fatalf("node subnet = %q, %v", nodeSubnet, err)
	}
	server := net.JoinHostPort(prefix.Addr().Next().String(), "53")
	for _, protocol := range []string{"udp", "tcp"} {
		request := new(dns.Msg)
		request.SetQuestion(name, dns.TypeA)
		response, _, err := (&dns.Client{Net: protocol, Timeout: 2 * time.Second}).Exchange(request, server)
		if err != nil || response.Rcode != want {
			t.Fatalf("%s DNS response = %#v, %v; want rcode %s", protocol, response, err, dns.RcodeToString[want])
		}
	}
}

func serviceAddresses(t *testing.T, application *orchv1alpha1.Application, service string) []string {
	t.Helper()
	var result []string
	for _, allocation := range application.GetAllocations() {
		if allocation.GetService() == service {
			if allocation.GetAddress() == "" {
				t.Fatalf("allocation %s/%d has no active address", service, allocation.GetReplica())
			}
			result = append(result, allocation.GetAddress())
		}
	}
	sort.Strings(result)
	if len(result) == 0 {
		t.Fatalf("service %s has no allocations", service)
	}
	return result
}

func assertIngressBackends(t *testing.T, domain string, addresses []string, revision string) {
	t.Helper()
	want := make(map[string]struct{}, len(addresses))
	for _, address := range addresses {
		want[address] = struct{}{}
	}
	seen := make(map[string]struct{}, len(addresses))
	for attempt := 0; attempt < 64 && len(seen) != len(want); attempt++ {
		status, body, err := requestIngress(domain, "/identity")
		if err != nil || status != http.StatusOK {
			t.Fatalf("ingress request = status %d body %q, %v", status, body, err)
		}
		fields := strings.Fields(strings.TrimSpace(body))
		if len(fields) != 2 || !strings.HasPrefix(fields[0], "address=") || fields[1] != "revision="+revision {
			t.Fatalf("ingress identity response = %q", body)
		}
		address := strings.TrimPrefix(fields[0], "address=")
		if _, ok := want[address]; !ok {
			t.Fatalf("ingress reached unexpected backend %q; want %v", address, addresses)
		}
		seen[address] = struct{}{}
	}
	if len(seen) != len(want) {
		t.Fatalf("ingress reached %v; want every backend %v", sortedStringSet(seen), addresses)
	}
}

func assertIngressWithdrawn(t *testing.T, domain string) {
	t.Helper()
	status, body, err := requestIngress(domain, "/identity")
	if err != nil {
		t.Fatal(err)
	}
	if status == http.StatusOK && strings.Contains(body, "revision=") {
		t.Fatalf("withdrawn ingress route still reached a workload: %q", body)
	}
}

func requestIngress(domain, path string) (int, string, error) {
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp4", "127.0.0.1:80")
	}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, _ := http.NewRequest(http.MethodGet, "http://"+domain+path, nil)
	response, err := client.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(body) > 4096 {
		return response.StatusCode, "", fmt.Errorf("read bounded ingress response: %w", err)
	}
	return response.StatusCode, string(body), nil
}

func sortedStringSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func waitForNode(t *testing.T, ctx context.Context, remote *controlclient.Client, process *agentProcess, want orchv1alpha1.NodeConnectionStatus) {
	t.Helper()
	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		if err := process.failure(); err != nil {
			t.Fatalf("agent exited while waiting for node state: %v", err)
		}
		requestContext, cancel := context.WithTimeout(ctx, 2*time.Second)
		nodes, err := remote.ListNodes(requestContext)
		cancel()
		if err == nil && len(nodes) == 1 && nodes[0].GetConnectionStatus() == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("node did not reach %s", want)
}

func waitForApplication(t *testing.T, ctx context.Context, remote *controlclient.Client, process *agentProcess, generation uint64, status orchv1alpha1.ExecutionStatus) *orchv1alpha1.Application {
	t.Helper()
	return waitForNamedApplication(t, ctx, remote, process, e2eApplication, generation, status)
}

func waitForNamedApplication(t *testing.T, ctx context.Context, remote *controlclient.Client, process *agentProcess, name string, generation uint64, status orchv1alpha1.ExecutionStatus) *orchv1alpha1.Application {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	var last *orchv1alpha1.Application
	var lastErr error
	for time.Now().Before(deadline) {
		if err := process.failure(); err != nil {
			t.Fatalf("agent exited while waiting for application state: %v", err)
		}
		requestContext, cancel := context.WithTimeout(ctx, 2*time.Second)
		application, err := remote.GetApplication(requestContext, name)
		cancel()
		if err == nil {
			last = application
			if application.GetGeneration() == generation && application.GetObservedGeneration() == generation && application.GetExecutionStatus() == status {
				return application
			}
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("application did not reach generation %d and %s; last=%#v error=%v", generation, status, last, lastErr)
	return nil
}

func waitForRestart(t *testing.T, ctx context.Context, remote *controlclient.Client, process *agentProcess, raw *containerdclient.Client, service string, replica uint32, oldPID uint32) *orchv1alpha1.Application {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if err := process.failure(); err != nil {
			t.Fatalf("agent exited while waiting for restart: %v", err)
		}
		requestContext, cancel := context.WithTimeout(ctx, 2*time.Second)
		application, err := remote.GetApplication(requestContext, e2eApplication)
		cancel()
		if err == nil && (application.GetExecutionStatus() == orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_RUNNING ||
			application.GetExecutionStatus() == orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_HEALTHY) {
			item := findAllocation(application, service, replica)
			if item != nil && item.GetRestartCount() >= 1 {
				id := orchruntime.ObjectID(item.GetId(), item.GetSpecDigest())
				if pid, pidErr := taskPID(ctx, raw, id); pidErr == nil && pid != oldPID {
					return application
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s/%d did not restart", service, replica)
	return nil
}

func waitForLogs(t *testing.T, ctx context.Context, remote *controlclient.Client, process *agentProcess, token string) {
	t.Helper()
	wantStdout := "fixture stdout token=" + token + " args=serve," + token + "\n"
	wantStderr := "fixture stderr token=" + token + " args=serve," + token + "\n"
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := process.failure(); err != nil {
			t.Fatalf("agent exited while waiting for logs: %v", err)
		}
		requestContext, cancel := context.WithTimeout(ctx, 3*time.Second)
		stream, err := remote.TailLogs(requestContext, e2eApplication, "worker", 1, 100)
		var stdout, stderr strings.Builder
		if err == nil {
			for stream.Receive() {
				frame := stream.Msg()
				if frame.GetStream() == orchv1alpha1.LogStream_LOG_STREAM_STDERR {
					stderr.Write(frame.GetData())
				} else {
					stdout.Write(frame.GetData())
				}
			}
			err = stream.Err()
			_ = stream.Close()
		}
		cancel()
		if err == nil && strings.Contains(stdout.String(), wantStdout) && strings.Contains(stderr.String(), wantStderr) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("logs for token %q did not become available", token)
}

func execute(t *testing.T, ctx context.Context, remote *controlclient.Client, application, service string, replica uint32, command []string) (string, string, int32) {
	t.Helper()
	stdout, stderr, exitCode, err := executeResult(ctx, remote, application, service, replica, command)
	if err != nil {
		t.Fatal(err)
	}
	return stdout, stderr, exitCode
}

func executeResult(ctx context.Context, remote *controlclient.Client, application, service string, replica uint32,
	command []string,
) (string, string, int32, error) {
	requestContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stream, err := remote.Exec(requestContext, application, service, replica, command, 8)
	if err != nil {
		return "", "", 0, err
	}
	defer stream.Close()
	var stdout, stderr strings.Builder
	var exited bool
	var exitCode int32
	for stream.Receive() {
		frame := stream.Msg()
		if frame.GetStream() == orchv1alpha1.LogStream_LOG_STREAM_STDERR {
			stderr.Write(frame.GetData())
		} else {
			stdout.Write(frame.GetData())
		}
		if frame.GetExited() {
			exited = true
			exitCode = frame.GetExitCode()
		}
	}
	if err := stream.Err(); err != nil {
		return "", "", 0, err
	}
	if !exited {
		return "", "", 0, errors.New("exec stream ended without an exit frame")
	}
	return stdout.String(), stderr.String(), exitCode, nil
}

type trafficResult struct {
	requests int
	err      error
}

func startContinuousServiceTraffic(parent context.Context) (<-chan struct{}, context.CancelFunc, <-chan trafficResult) {
	ctx, cancel := context.WithCancel(parent)
	ready := make(chan struct{})
	done := make(chan trafficResult, 1)
	go func() {
		requests := 0
		for {
			status, body, err := requestIngress(e2eIngressDomain, "/")
			if err != nil || status != http.StatusOK || body != "token=stable\n" {
				if ctx.Err() != nil {
					done <- trafficResult{requests: requests}
					return
				}
				if err == nil {
					err = fmt.Errorf("ingress response status=%d body=%q", status, body)
				}
				done <- trafficResult{requests: requests, err: err}
				return
			}
			requests++
			if requests == 1 {
				close(ready)
			}
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				done <- trafficResult{requests: requests}
				return
			case <-timer.C:
			}
		}
	}()
	return ready, cancel, done
}

func allocation(t *testing.T, application *orchv1alpha1.Application, service string, replica uint32) *orchv1alpha1.AllocationStatus {
	t.Helper()
	item := findAllocation(application, service, replica)
	if item == nil {
		t.Fatalf("allocation %s/%d was not reported", service, replica)
	}
	return item
}

func findAllocation(application *orchv1alpha1.Application, service string, replica uint32) *orchv1alpha1.AllocationStatus {
	for _, item := range application.GetAllocations() {
		if item.GetService() == service && item.GetReplica() == replica {
			return item
		}
	}
	return nil
}

func applicationPIDs(t *testing.T, ctx context.Context, raw *containerdclient.Client, application *orchv1alpha1.Application) map[string]uint32 {
	t.Helper()
	result := make(map[string]uint32, len(application.GetAllocations()))
	for _, item := range application.GetAllocations() {
		id := orchruntime.ObjectID(item.GetId(), item.GetSpecDigest())
		pid, err := taskPID(ctx, raw, id)
		if err != nil {
			t.Fatalf("load task %s: %v", id, err)
		}
		result[id] = pid
	}
	return result
}

func servicePIDs(t *testing.T, ctx context.Context, raw *containerdclient.Client, application *orchv1alpha1.Application, service string) map[string]uint32 {
	t.Helper()
	result := make(map[string]uint32)
	for _, item := range application.GetAllocations() {
		if item.GetService() != service {
			continue
		}
		id := orchruntime.ObjectID(item.GetId(), item.GetSpecDigest())
		pid, err := taskPID(ctx, raw, id)
		if err != nil {
			t.Fatal(err)
		}
		result[id] = pid
	}
	return result
}

func assertActiveServicePIDs(t *testing.T, ctx context.Context, raw *containerdclient.Client,
	application *orchv1alpha1.Application, service string, want map[string]uint32,
) {
	t.Helper()
	got := make(map[string]uint32)
	for _, item := range application.GetAllocations() {
		if item.GetService() != service {
			continue
		}
		id := orchruntime.ObjectID(item.GetId(), item.GetActiveSpecDigest())
		pid, err := taskPID(ctx, raw, id)
		if err != nil {
			t.Fatalf("load active task %s: %v", id, err)
		}
		got[id] = pid
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("active %s PIDs = %#v, want %#v", service, got, want)
	}
}

func assertPIDs(t *testing.T, ctx context.Context, raw *containerdclient.Client, application *orchv1alpha1.Application, want map[string]uint32) {
	t.Helper()
	got := applicationPIDs(t, ctx, raw, application)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("task identity changed: got %#v want %#v", got, want)
	}
}

func assertServicePIDs(t *testing.T, ctx context.Context, raw *containerdclient.Client, application *orchv1alpha1.Application, service string, want map[string]uint32) {
	t.Helper()
	got := servicePIDs(t, ctx, raw, application, service)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unaffected %s task identity changed: got %#v want %#v", service, got, want)
	}
}

func taskPID(ctx context.Context, raw *containerdclient.Client, id string) (uint32, error) {
	container, err := raw.LoadContainer(ctx, id)
	if err != nil {
		return 0, err
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		return 0, err
	}
	status, err := task.Status(ctx)
	if err != nil {
		return 0, err
	}
	if status.Status != containerdclient.Running {
		return 0, fmt.Errorf("task %s status is %s", id, status.Status)
	}
	return task.Pid(), nil
}

func waitForTaskRunning(t *testing.T, ctx context.Context, raw *containerdclient.Client, id string) {
	t.Helper()
	deadline := time.Now().Add(35 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := taskPID(ctx, raw, id); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("task %s did not return to running state: %v", id, lastErr)
}

func assertContainerContract(t *testing.T, ctx context.Context, raw *containerdclient.Client, clusterID, nodeID, agentDir string, item *orchv1alpha1.AllocationStatus, token, snapshotter string) {
	t.Helper()
	id := orchruntime.ObjectID(item.GetId(), item.GetSpecDigest())
	container, err := raw.LoadContainer(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	labels, err := container.Labels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantLabels := map[string]string{
		managedLabel: "true", clusterLabel: clusterID, nodeLabel: nodeID,
		allocationLabel: item.GetId(), specLabel: item.GetSpecDigest(),
	}
	for name, want := range wantLabels {
		if labels[name] != want {
			t.Fatalf("container label %s = %q, want %q", name, labels[name], want)
		}
	}
	info, err := container.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Runtime.Name != plugins.RuntimeRuncV2 {
		t.Fatalf("runtime = %q, want %q", info.Runtime.Name, plugins.RuntimeRuncV2)
	}
	spec, err := container.Spec(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Process == nil || !reflect.DeepEqual(spec.Process.Args, []string{"/orch-fixture", "serve", token}) || !spec.Process.NoNewPrivileges {
		t.Fatalf("process spec = %#v", spec.Process)
	}
	environment := make(map[string]string)
	for _, entry := range spec.Process.Env {
		name, value, found := strings.Cut(entry, "=")
		if found {
			environment[name] = value
		}
	}
	if environment["ORCH_E2E_TOKEN"] != token || environment["ORCH_E2E_START_FILE"] != "/data/starts" {
		t.Fatalf("runtime environment = %#v", environment)
	}
	if spec.Linux == nil || spec.Linux.Resources == nil || spec.Linux.Resources.CPU == nil ||
		spec.Linux.Resources.CPU.Quota == nil || *spec.Linux.Resources.CPU.Quota != 25_000 ||
		spec.Linux.Resources.CPU.Period == nil || *spec.Linux.Resources.CPU.Period != 100_000 ||
		spec.Linux.Resources.Memory == nil || spec.Linux.Resources.Memory.Limit == nil || *spec.Linux.Resources.Memory.Limit != 64<<20 ||
		spec.Linux.Resources.Memory.Swap == nil || *spec.Linux.Resources.Memory.Swap != *spec.Linux.Resources.Memory.Limit {
		t.Fatalf("cgroup resources = %#v", spec.Linux)
	}
	hasNetworkNamespace := false
	for _, namespace := range spec.Linux.Namespaces {
		if namespace.Type == "network" {
			hasNetworkNamespace = true
		}
	}
	if !hasNetworkNamespace {
		t.Fatalf("OCI namespaces omit a network namespace: %#v", spec.Linux.Namespaces)
	}
	wantVolume := filepath.Join(agentDir, agentctl.VolumesDirectory, e2eApplication, "state")
	hasVolume := false
	hasResolver := false
	for _, mount := range spec.Mounts {
		if mount.Destination == "/data" && mount.Type == "bind" && mount.Source == wantVolume {
			hasVolume = true
		}
		if mount.Destination == "/etc/resolv.conf" && mount.Type == "bind" &&
			mount.Source == filepath.Join(agentDir, "network", "resolv-"+e2eApplication+".conf") {
			hasResolver = true
		}
	}
	if !hasVolume {
		t.Fatalf("OCI mounts omit agent-owned volume %s: %#v", wantVolume, spec.Mounts)
	}
	if !hasResolver {
		t.Fatalf("OCI mounts omit the agent-owned resolver: %#v", spec.Mounts)
	}
	resolverPath := filepath.Join(agentDir, "network", "resolv-"+e2eApplication+".conf")
	resolver, err := os.ReadFile(resolverPath)
	if err != nil || !strings.Contains(string(resolver), "search "+e2eApplication+".svc.internal\n") {
		t.Fatalf("application resolver = %q, %v", resolver, err)
	}
	resolverInfo, err := os.Lstat(resolverPath)
	if err != nil {
		t.Fatalf("stat application resolver: %v", err)
	}
	if resolverInfo.Mode().Perm() != 0o644 {
		t.Fatalf("application resolver mode = %v", resolverInfo.Mode().Perm())
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	hostNetwork, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	containerNetwork, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", task.Pid()))
	if err != nil {
		t.Fatal(err)
	}
	if hostNetwork == containerNetwork {
		t.Fatalf("task shares the host network namespace %q", hostNetwork)
	}
	foundLease := false
	allocationLeases, err := raw.LeasesService().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, lease := range allocationLeases {
		if lease.ID == id+"-lease" && lease.Labels[clusterLabel] == clusterID && lease.Labels[allocationLabel] == item.GetId() {
			foundLease = true
		}
	}
	if !foundLease {
		t.Fatalf("persistent allocation lease %s-lease was not found", id)
	}
	if _, err := raw.SnapshotService(snapshotter).Stat(ctx, id+"-rootfs"); err != nil {
		t.Fatalf("inspect writable snapshot: %v", err)
	}
}

func assertForeignResource(t *testing.T, ctx context.Context, raw *containerdclient.Client, id string) {
	t.Helper()
	container, err := raw.LoadContainer(ctx, id)
	if err != nil {
		t.Fatalf("agent touched foreign container %s: %v", id, err)
	}
	labels, err := container.Labels(ctx)
	if err != nil || !reflect.DeepEqual(labels, map[string]string{"io.example.owner": "foreign"}) {
		t.Fatalf("foreign container labels = %#v, %v", labels, err)
	}
}

func waitForFile(t *testing.T, ctx context.Context, path, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(path)
		if err == nil && string(contents) == want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	contents, err := os.ReadFile(path)
	t.Fatalf("file %s = %q, %v; want %q", path, contents, err, want)
}

func assertNoManagedResources(t *testing.T, ctx context.Context, raw *containerdclient.Client, clusterID, snapshotter, logsDir string) {
	t.Helper()
	containers, err := raw.Containers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, container := range containers {
		labels, labelErr := container.Labels(ctx)
		if labelErr != nil {
			t.Fatal(labelErr)
		}
		if labels[managedLabel] == "true" && labels[clusterLabel] == clusterID {
			t.Fatalf("managed container leaked after removal: %s", container.ID())
		}
	}
	allocationLeases, err := raw.LeasesService().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, lease := range allocationLeases {
		if lease.Labels[managedLabel] == "true" && lease.Labels[clusterLabel] == clusterID {
			t.Fatalf("managed lease leaked after removal: %s", lease.ID)
		}
	}
	if err := raw.SnapshotService(snapshotter).Walk(ctx, func(_ context.Context, info snapshots.Info) error {
		if info.Labels[managedLabel] == "true" && info.Labels[clusterLabel] == clusterID {
			return fmt.Errorf("managed snapshot leaked after removal: %s", info.Name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	logEntries, err := os.ReadDir(logsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(logEntries) != 0 {
		t.Fatalf("managed log files leaked after removal: %v", logEntries)
	}
}

func cleanupContainerdNamespace(t *testing.T, raw *containerdclient.Client, namespace string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	containers, err := raw.Containers(ctx)
	if err == nil {
		for _, container := range containers {
			if task, taskErr := container.Task(ctx, nil); taskErr == nil {
				_, _ = task.Delete(ctx, containerdclient.WithProcessKill)
			}
			if deleteErr := container.Delete(ctx, containerdclient.WithSnapshotCleanup); deleteErr != nil && !errdefs.IsNotFound(deleteErr) {
				t.Errorf("cleanup container %s: %v", container.ID(), deleteErr)
			}
		}
	} else {
		t.Errorf("list cleanup containers: %v", err)
	}
	allocationLeases, err := raw.LeasesService().List(ctx)
	if err == nil {
		for _, lease := range allocationLeases {
			if deleteErr := raw.LeasesService().Delete(ctx, lease, leases.SynchronousDelete); deleteErr != nil && !errdefs.IsNotFound(deleteErr) {
				t.Errorf("cleanup lease %s: %v", lease.ID, deleteErr)
			}
		}
	} else {
		t.Errorf("list cleanup leases: %v", err)
	}
	storedImages, err := raw.ImageService().List(ctx)
	if err == nil {
		for _, image := range storedImages {
			if deleteErr := raw.ImageService().Delete(ctx, image.Name, images.SynchronousDelete()); deleteErr != nil && !errdefs.IsNotFound(deleteErr) {
				t.Errorf("cleanup image %s: %v", image.Name, deleteErr)
			}
		}
	} else {
		t.Errorf("list cleanup images: %v", err)
	}
	if err := raw.NamespaceService().Delete(ctx, namespace); err != nil && !errdefs.IsNotFound(err) {
		t.Errorf("cleanup namespace %s: %v", namespace, err)
	}
}

func requiredEnvironment(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("%s is required when ORCH_CONTAINERD_E2E=1", name)
	}
	return value
}

func requiredRegularFile(t *testing.T, name string, executable bool) string {
	t.Helper()
	path := requiredEnvironment(t, name)
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("%s must name a regular file: %v", name, err)
	}
	if executable && info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("%s must name an executable", name)
	}
	return absolute
}

func reserveLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}
