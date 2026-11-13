package cli

import (
	_ "embed"
	"encoding/json"
	"strings"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
)

// orch mcp answers some tools with an interactive view as well as text, using
// the MCP Apps extension (io.modelcontextprotocol/ui): a tool names a ui://
// resource, and clients that support it render that HTML page with the
// tool's result. The model still gets the same text as before.

const (
	mcpUIExtension = "io.modelcontextprotocol/ui"
	mcpUIMimeType  = "text/html;profile=mcp-app"
)

//go:embed mcp_ui.html
var mcpUIPage string

type mcpView struct {
	name        string
	title       string
	description string
}

var mcpViews = []mcpView{
	{"cluster", "Cluster status", "The cluster's server, network, certificates and controller backups."},
	{"apps", "Applications", "Every application with its rollout status; select one to see its replicas and routes."},
	{"app", "Application status", "An application's replicas, health, routes and cron jobs."},
	{"nodes", "Nodes", "Every node with its CPU and memory in use and reserved; select one to see its usage over the last day."},
	{"node", "Node usage", "A node's CPU, memory and disk use over the last 24 hours against what it reserves and can offer."},
	{"deploy", "Deploy", "A deploy or rollback, followed live until the rollout is ready or fails."},
}

func mcpViewURI(name string) string { return "ui://orch/" + name }

func mcpViewByURI(uri string) (mcpView, bool) {
	for _, view := range mcpViews {
		if mcpViewURI(view.name) == uri {
			return view, true
		}
	}
	return mcpView{}, false
}

// mcpUIResource is a view's page: one shared page told which view to draw,
// with the enum names it needs because the commands' JSON has enum numbers.
func mcpUIResource(view mcpView) map[string]any {
	config, _ := json.Marshal(map[string]any{
		"view":    view.name,
		"version": Version,
		"enums": map[string]map[int32]string{
			"ExecutionStatus": orchv1alpha1.ExecutionStatus_name,
			"AllocationPhase": orchv1alpha1.AllocationPhase_name,
			"HealthStatus":    orchv1alpha1.HealthStatus_name,
			"RouteState":      orchv1alpha1.RouteState_name,
			"JobRunState":     orchv1alpha1.JobRunState_name,
			"NodeLifecycle":   orchv1alpha1.NodeLifecycle_name,
			"NodeConnection":  orchv1alpha1.NodeConnectionStatus_name,
		},
	})
	return map[string]any{
		"uri":      mcpViewURI(view.name),
		"mimeType": mcpUIMimeType,
		"text":     strings.Replace(mcpUIPage, "ORCH_CONFIG", string(config), 1),
		"_meta":    map[string]any{"ui": map[string]any{"prefersBorder": true}},
	}
}

func (s *mcpServer) listResources() map[string]any {
	resources := []map[string]any{}
	for _, view := range mcpViews {
		resources = append(resources, map[string]any{
			"uri": mcpViewURI(view.name), "name": view.name, "title": view.title,
			"description": view.description, "mimeType": mcpUIMimeType,
		})
	}
	return map[string]any{"resources": resources}
}

func (s *mcpServer) readResource(raw json.RawMessage) (any, *mcpError) {
	var params struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, &mcpError{Code: -32602, Message: "invalid resources/read params"}
	}
	view, ok := mcpViewByURI(params.URI)
	if !ok {
		return nil, &mcpError{Code: -32002, Message: "resource not found: " + params.URI}
	}
	return map[string]any{"contents": []map[string]any{mcpUIResource(view)}}, nil
}

// mcpClientSupportsUI reports whether initialize advertised MCP Apps with
// HTML views, so tools only point at views the client can show.
func mcpClientSupportsUI(capabilities json.RawMessage) bool {
	var parsed struct {
		Extensions map[string]struct {
			MimeTypes []string `json:"mimeTypes"`
		} `json:"extensions"`
	}
	if json.Unmarshal(capabilities, &parsed) != nil {
		return false
	}
	extension, ok := parsed.Extensions[mcpUIExtension]
	if !ok {
		return false
	}
	for _, mimeType := range extension.MimeTypes {
		if mimeType == mcpUIMimeType {
			return true
		}
	}
	return false
}

// mcpStructured returns a command's JSON output as structured content for a
// view, or nil when the output is not JSON. Structured content must be an
// object, so a list (such as orch nodes) is wrapped as {"items": [...]}.
func mcpStructured(stdout string) map[string]any {
	var value any
	if json.Unmarshal([]byte(stdout), &value) != nil {
		return nil
	}
	switch value := value.(type) {
	case map[string]any:
		return value
	case []any:
		return map[string]any{"items": value}
	}
	return nil
}
