package control

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"change.codes/torsten/better-swarm/internal/buildinfo"
	clusterctl "change.codes/torsten/better-swarm/internal/cluster"
	"change.codes/torsten/better-swarm/internal/model"
)

func TestRenderMetricsReportsFailureStates(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	input := metricsInput{
		now: now, sessions: 1,
		caNotAfter:         now.Add(3650 * 24 * time.Hour),
		controllerNotAfter: now.Add(20*24*time.Hour + 500*time.Millisecond),
		nodes: []model.Node{
			{Name: "worker-1", Lifecycle: model.NodeLifecycleActive, ConnectionStatus: model.NodeConnected, CertificateNotAfter: now.Add(time.Hour)},
			{Name: "worker-2", Lifecycle: model.NodeLifecycleCordoned, ConnectionStatus: model.NodeUnreachable},
		},
		applications: []model.Application{{
			Name: "shop", Execution: model.ApplicationExecution{
				Status: model.ExecutionDegraded,
				Allocations: []model.AllocationStatus{
					{Service: "web", Phase: model.AllocationRunning, HealthStatus: model.HealthReady, RestartCount: 1},
					{Service: "web", Phase: model.AllocationBackoff, HealthStatus: model.HealthUnhealthy, RestartCount: 6},
				},
				Routes:    []model.RouteStatus{{Domain: "shop.example", State: model.RouteFailed}},
				TCPRoutes: []model.TCPRouteStatus{{PublicPort: 5432, State: model.TCPRouteActive}},
				CronJobs: []model.CronJobStatus{
					{Job: "cleanup", LastState: model.JobRunTimedOut},
					{Job: `say "hi"`, LastState: model.JobRunSucceeded},
				},
			},
		}},
	}
	want := `# HELP orch_build_info Controller build; the value is always 1.
# TYPE orch_build_info gauge
orch_build_info{version="` + buildinfo.Version + `",build_id="` + buildinfo.BuildID + `",go_version="` + runtime.Version() + `"} 1
# HELP orch_agent_sessions Agent sessions with a recent heartbeat.
# TYPE orch_agent_sessions gauge
orch_agent_sessions 1
# HELP orch_certificate_expiry_seconds Seconds until a cluster certificate expires; admin is the scraper's own certificate.
# TYPE orch_certificate_expiry_seconds gauge
orch_certificate_expiry_seconds{certificate="ca"} 315360000
orch_certificate_expiry_seconds{certificate="controller"} 1728000
# HELP orch_nodes Nodes by lifecycle and connection (unreachable means no heartbeat for 45s).
# TYPE orch_nodes gauge
orch_nodes{lifecycle="active",connection="connected"} 1
orch_nodes{lifecycle="cordoned",connection="unreachable"} 1
# HELP orch_node_connected 1 when the node's agent is connected.
# TYPE orch_node_connected gauge
orch_node_connected{node="worker-1",lifecycle="active"} 1
orch_node_connected{node="worker-2",lifecycle="cordoned"} 0
# HELP orch_node_certificate_expiry_seconds Seconds until the node's agent certificate expires.
# TYPE orch_node_certificate_expiry_seconds gauge
orch_node_certificate_expiry_seconds{node="worker-1"} 3600
# HELP orch_application_status Current execution status of each application; the value is always 1.
# TYPE orch_application_status gauge
orch_application_status{application="shop",status="degraded"} 1
# HELP orch_allocations Allocations by phase and health; phase backoff means the process is crash-looping.
# TYPE orch_allocations gauge
orch_allocations{application="shop",service="web",phase="backoff",health="unhealthy"} 1
orch_allocations{application="shop",service="web",phase="running",health="ready"} 1
# HELP orch_allocation_restarts Restarts of the current allocations of a service.
# TYPE orch_allocation_restarts gauge
orch_allocation_restarts{application="shop",service="web"} 7
# HELP orch_routes Ingress routes per node by kind and state.
# TYPE orch_routes gauge
orch_routes{application="shop",kind="http",state="failed"} 1
orch_routes{application="shop",kind="tcp",state="active"} 1
# HELP orch_cron_job_last_run_failed 1 when the cron job's last run failed or timed out.
# TYPE orch_cron_job_last_run_failed gauge
orch_cron_job_last_run_failed{application="shop",job="cleanup"} 1
orch_cron_job_last_run_failed{application="shop",job="say \"hi\""} 0
`
	if got := string(renderMetrics(input)); got != want {
		t.Fatalf("metrics =\n%s\nwant\n%s", got, want)
	}
}

func TestCollectMetricsReadsNodesAndApplicationsFromStore(t *testing.T) {
	state, cluster := reconcilerStore(t)
	enrollCompatibleNode(t, state)
	applyManifest(t, state, runtimeManifest(executableImage), "revision-one", "metrics-apply-key-one", reconcilerTestTime)
	now := reconcilerTestTime.Add(time.Hour)
	s := newService(state, cluster, nil, nil, nil, newSessionManager(func() time.Time { return now }), func() time.Time { return now })
	input, err := s.collectMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body := string(renderMetrics(input))
	for _, want := range []string{
		`orch_nodes{lifecycle="active",connection="unreachable"} 1`,
		`orch_node_connected{node="node-1",lifecycle="active"} 0`,
		`orch_node_certificate_expiry_seconds{node="node-1"} `,
		`orch_application_status{application="hello",status="`,
		"orch_agent_sessions 0",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
}

func TestControllerServesMetricsOnlyToAdministrators(t *testing.T) {
	_, dataDir, contextPath, _ := bootstrapController(t)
	socketDir, err := os.MkdirTemp("", "orch-metrics-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "admin.sock")
	server, err := Start(context.Background(), Options{DataDir: dataDir, Listen: "127.0.0.1:0", AdminSocket: socket})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	endpoint := "https://" + server.Addr().String() + "/metrics"
	credentials, err := clusterctl.LoadAdministratorCredentials(contextPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	get := func(client *http.Client, url string) (int, string) {
		t.Helper()
		response, err := client.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(body)
	}

	if status, body := get(tlsHTTPClient(credentials.RootCAs, tls.Certificate{}), endpoint); status != http.StatusUnauthorized || strings.Contains(body, "orch_") {
		t.Fatalf("anonymous metrics = %d %q", status, body)
	}
	status, body := get(tlsHTTPClient(credentials.RootCAs, credentials.Certificate), endpoint)
	if status != http.StatusOK {
		t.Fatalf("administrator metrics = %d %q", status, body)
	}
	for _, want := range []string{`orch_certificate_expiry_seconds{certificate="ca"}`, `orch_certificate_expiry_seconds{certificate="controller"}`,
		`orch_certificate_expiry_seconds{certificate="admin"}`, "orch_agent_sessions 0", "orch_build_info{"} {
		if !strings.Contains(body, want) {
			t.Fatalf("administrator metrics missing %q:\n%s", want, body)
		}
	}

	local := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	status, body = get(local, "http://orch/metrics")
	if status != http.StatusOK || !strings.Contains(body, `orch_certificate_expiry_seconds{certificate="controller"}`) ||
		strings.Contains(body, `certificate="admin"`) {
		t.Fatalf("socket metrics = %d %q", status, body)
	}
}
