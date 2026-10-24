package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/TorstenDittmann/orch/internal/buildinfo"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/security"
	"github.com/TorstenDittmann/orch/internal/store"
)

// metricsInput is everything one scrape reports, read from the store and the
// controller's own state when the scrape arrives.
type metricsInput struct {
	now                time.Time
	nodes              []model.Node
	applications       []model.Application
	sessions           int
	caNotAfter         time.Time
	controllerNotAfter time.Time
	// adminNotAfter is the scraper's own administrator certificate; zero on
	// the admin socket, where there is none.
	adminNotAfter time.Time
	// backups is nil when automatic controller backups are off.
	backups *backupStatus
	// offsite is nil when no backup destination is set.
	offsite *offsiteStatus
	// autoscale holds each autoscaled service's status keyed by
	// application and service.
	autoscale map[string]map[string]AutoscaleStatus
}

// metricsHandler serves the Prometheus text exposition format. Every value is
// computed from current state at scrape time, so the controller keeps no
// metric state and a restart loses nothing.
func (s *service) metricsHandler(controllerNotAfter time.Time) http.Handler {
	caNotAfter, _ := security.CertificateNotAfter(s.caPEM)
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			response.Header().Set("Allow", "GET, HEAD")
			http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := requireAdministrator(request.Context()); err != nil {
			status := http.StatusUnauthorized
			if connect.CodeOf(err) == connect.CodePermissionDenied {
				status = http.StatusForbidden
			}
			http.Error(response, "administrator credentials are required", status)
			return
		}
		input, err := s.collectMetrics(request.Context())
		if err != nil {
			http.Error(response, "read cluster state", http.StatusInternalServerError)
			return
		}
		input.caNotAfter, input.controllerNotAfter = caNotAfter, controllerNotAfter
		if principal, ok := request.Context().Value(principalKey{}).(authenticatedPrincipal); ok && principal.certificate != nil {
			input.adminNotAfter = principal.certificate.NotAfter
		}
		response.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		response.Header().Set("Cache-Control", "no-store")
		response.WriteHeader(http.StatusOK)
		if request.Method == http.MethodGet {
			_, _ = response.Write(renderMetrics(input))
		}
	})
}

func (s *service) collectMetrics(ctx context.Context) (metricsInput, error) {
	input := metricsInput{now: s.now()}
	if status := s.offsite.status(); status.URL != "" {
		input.offsite = &status
	}
	if s.backups != nil {
		status := s.backups.status()
		input.backups = &status
	}
	nodes, err := s.state.Nodes(ctx)
	if err != nil {
		return input, err
	}
	if err := s.decorateNodeUsage(ctx, nodes); err != nil {
		return input, err
	}
	for _, node := range nodes {
		input.nodes = append(input.nodes, s.decorateNodeConnection(node))
	}
	applications, err := s.state.Applications(ctx, false)
	if err != nil {
		return input, err
	}
	for _, summary := range applications {
		application, err := s.state.Application(ctx, summary.Name)
		if errors.Is(err, store.ErrApplicationNotFound) {
			continue // removed between the two reads
		}
		if err != nil {
			return input, err
		}
		input.applications = append(input.applications, application)
	}
	scales, err := s.state.ServiceScales(ctx)
	if err != nil {
		return input, err
	}
	for _, application := range input.applications {
		if statuses := s.autoscaler.Status(application, scales[application.Name]); len(statuses) != 0 {
			if input.autoscale == nil {
				input.autoscale = map[string]map[string]AutoscaleStatus{}
			}
			input.autoscale[application.Name] = statuses
		}
	}
	if s.sessions != nil {
		input.sessions = s.sessions.ConnectedCount()
	}
	return input, nil
}

// renderMetrics writes the exposition text. Label values are lowercase so
// alert rules do not depend on the CLI's display casing.
func renderMetrics(input metricsInput) []byte {
	var out metricsWriter
	descriptor := buildinfo.Current()
	out.family("orch_build_info", "Controller build; the value is always 1.", []metricSample{
		{labels: metricLabels("version", descriptor.Version, "build_id", descriptor.BuildID, "go_version", runtime.Version()), value: 1},
	})
	out.family("orch_agent_sessions", "Agent sessions with a recent heartbeat.", []metricSample{{value: float64(input.sessions)}})

	certificates := []metricSample{}
	for _, item := range []struct {
		name     string
		notAfter time.Time
	}{{"ca", input.caNotAfter}, {"controller", input.controllerNotAfter}, {"admin", input.adminNotAfter}} {
		if !item.notAfter.IsZero() {
			certificates = append(certificates, metricSample{labels: metricLabels("certificate", item.name), value: secondsUntil(item.notAfter, input.now)})
		}
	}
	out.family("orch_certificate_expiry_seconds", "Seconds until a cluster certificate expires; admin is the scraper's own certificate.", certificates)
	if input.backups != nil {
		latest := []metricSample{}
		if !input.backups.Latest.CreatedAt.IsZero() {
			latest = append(latest, metricSample{value: float64(input.backups.Latest.CreatedAt.Unix())})
		}
		out.family("orch_controller_backup_timestamp_seconds", "Unix time of the newest automatic controller backup.", latest)
		failed := 0.0
		if input.backups.Failure != "" {
			failed = 1
		}
		out.family("orch_controller_backup_failed", "1 when the last automatic controller backup attempt failed.", []metricSample{{value: failed}})
	}
	if input.offsite != nil {
		synced := []metricSample{}
		if !input.offsite.SyncedAt.IsZero() {
			synced = append(synced, metricSample{value: float64(input.offsite.SyncedAt.Unix())})
		}
		out.family("orch_backup_upload_timestamp_seconds", "Unix time of the last run that copied every backup to the bucket.", synced)
		failed := 0.0
		if input.offsite.Failure != "" {
			failed = 1
		}
		out.family("orch_backup_upload_failed", "1 when the last run that copies backups to the bucket failed.", []metricSample{{value: failed}})
	}

	nodeCounts := metricCounter{}
	connected := []metricSample{}
	nodeCertificates := []metricSample{}
	var cpuUsed, memoryUsed, diskUsed, diskTotal []metricSample
	for _, node := range input.nodes {
		if usage := node.Usage; usage != nil {
			labels := metricLabels("node", node.Name)
			cpuUsed = append(cpuUsed, metricSample{labels: labels, value: float64(usage.CPUUsedMillis) / 1000})
			memoryUsed = append(memoryUsed, metricSample{labels: labels, value: float64(usage.MemoryUsedBytes)})
			diskUsed = append(diskUsed, metricSample{labels: labels, value: float64(usage.DiskUsedBytes)})
			diskTotal = append(diskTotal, metricSample{labels: labels, value: float64(usage.DiskTotalBytes)})
		}
		lifecycle, connection := lower(node.Lifecycle), lower(node.ConnectionStatus)
		nodeCounts.add(metricLabels("lifecycle", lifecycle, "connection", connection), 1)
		up := 0.0
		if node.ConnectionStatus == model.NodeConnected {
			up = 1
		}
		connected = append(connected, metricSample{labels: metricLabels("node", node.Name, "lifecycle", lifecycle), value: up})
		if !node.CertificateNotAfter.IsZero() {
			nodeCertificates = append(nodeCertificates, metricSample{labels: metricLabels("node", node.Name), value: secondsUntil(node.CertificateNotAfter, input.now)})
		}
	}
	out.family("orch_nodes", "Nodes by lifecycle and connection (unreachable means no heartbeat for 45s).", nodeCounts.samples())
	out.family("orch_node_connected", "1 when the node's agent is connected.", connected)
	out.family("orch_node_certificate_expiry_seconds", "Seconds until the node's agent certificate expires.", nodeCertificates)
	out.family("orch_node_cpu_used_cores", "CPU cores the node used over its last heartbeat interval.", cpuUsed)
	out.family("orch_node_memory_used_bytes", "Memory in use on the node, excluding reclaimable cache.", memoryUsed)
	out.family("orch_node_disk_used_bytes", "Used bytes of the filesystem holding the agent's data.", diskUsed)
	out.family("orch_node_disk_total_bytes", "Size of the filesystem holding the agent's data.", diskTotal)

	statuses := []metricSample{}
	allocations := metricCounter{}
	restarts := metricCounter{}
	routes := metricCounter{}
	cron := []metricSample{}
	for _, application := range input.applications {
		name := application.Name
		statuses = append(statuses, metricSample{labels: metricLabels("application", name, "status", lower(application.Execution.Status)), value: 1})
		for _, allocation := range application.Execution.Allocations {
			allocations.add(metricLabels("application", name, "service", allocation.Service,
				"phase", lower(allocation.Phase), "health", lower(allocation.HealthStatus)), 1)
			restarts.add(metricLabels("application", name, "service", allocation.Service), float64(allocation.RestartCount))
		}
		for _, route := range application.Execution.Routes {
			routes.add(metricLabels("application", name, "kind", "http", "state", lower(route.State)), 1)
		}
		for _, route := range application.Execution.TCPRoutes {
			routes.add(metricLabels("application", name, "kind", "tcp", "state", lower(route.State)), 1)
		}
		for _, job := range application.Execution.CronJobs {
			failed := 0.0
			if job.LastState == model.JobRunFailed || job.LastState == model.JobRunTimedOut {
				failed = 1
			}
			cron = append(cron, metricSample{labels: metricLabels("application", name, "job", job.Job), value: failed})
		}
	}
	out.family("orch_application_status", "Current execution status of each application; the value is always 1.", statuses)
	out.family("orch_allocations", "Allocations by phase and health; phase backoff means the process is crash-looping.", allocations.samples())
	out.family("orch_allocation_restarts", "Restarts of the current allocations of a service.", restarts.samples())
	out.family("orch_routes", "Ingress routes per node by kind and state.", routes.samples())
	out.family("orch_cron_job_last_run_failed", "1 when the cron job's last run failed or timed out.", cron)

	var scaleReplicas, scaleCPU, scaleLimited []metricSample
	for application, services := range input.autoscale {
		for service, status := range services {
			labels := metricLabels("application", application, "service", service)
			scaleReplicas = append(scaleReplicas, metricSample{labels: labels, value: float64(status.Replicas)})
			if status.Utilization >= 0 {
				scaleCPU = append(scaleCPU, metricSample{labels: labels, value: math.Round(status.Utilization*1000) / 1000})
			}
			limited := 0.0
			if status.Limited != "" {
				limited = 1
			}
			scaleLimited = append(scaleLimited, metricSample{labels: labels, value: limited})
		}
	}
	out.family("orch_service_autoscale_replicas", "Replicas the autoscaler chose for a service.", scaleReplicas)
	out.family("orch_service_autoscale_cpu_ratio", "Average CPU use of an autoscaled service's ready replicas as a fraction of their request.", scaleCPU)
	out.family("orch_service_autoscale_limited", "1 when an autoscaled service wants more replicas than the cluster has room for.", scaleLimited)
	return out.buffer.Bytes()
}

type metricSample struct {
	labels string
	value  float64
}

type metricsWriter struct {
	buffer bytes.Buffer
}

func (w *metricsWriter) family(name, help string, samples []metricSample) {
	fmt.Fprintf(&w.buffer, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	slices.SortFunc(samples, func(a, b metricSample) int { return strings.Compare(a.labels, b.labels) })
	for _, sample := range samples {
		fmt.Fprintf(&w.buffer, "%s%s %s\n", name, sample.labels, strconv.FormatFloat(sample.value, 'f', -1, 64))
	}
}

// metricCounter sums samples that share a label set.
type metricCounter map[string]float64

func (c metricCounter) add(labels string, value float64) { c[labels] += value }

func (c metricCounter) samples() []metricSample {
	result := make([]metricSample, 0, len(c))
	for labels, value := range c {
		result = append(result, metricSample{labels: labels, value: value})
	}
	return result
}

var metricLabelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// metricLabels renders name/value pairs as {name="value",...}.
func metricLabels(pairs ...string) string {
	var builder strings.Builder
	builder.WriteByte('{')
	for index := 0; index+1 < len(pairs); index += 2 {
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(pairs[index])
		builder.WriteString(`="`)
		builder.WriteString(metricLabelEscaper.Replace(pairs[index+1]))
		builder.WriteByte('"')
	}
	builder.WriteByte('}')
	return builder.String()
}

func secondsUntil(at, now time.Time) float64 {
	return float64(at.Sub(now).Truncate(time.Second) / time.Second)
}

func lower(value string) string { return strings.ToLower(value) }
