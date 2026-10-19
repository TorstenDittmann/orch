package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
)

// orch mcp serves the CLI to AI assistants over the Model Context Protocol
// (newline-delimited JSON-RPC 2.0 on stdio). Each tool runs the matching orch
// command in-process with the same target and credentials the CLI uses, so
// the assistant can do what the person running it can do, and nothing more.

const mcpLatestProtocol = "2025-06-18"

var mcpProtocols = map[string]bool{"2024-11-05": true, "2025-03-26": true, "2025-06-18": true}

// mcpMaxOutput bounds what one tool call returns to the model.
const mcpMaxOutput = 256 << 10

const mcpInstructions = `orch runs applications on a small cluster of your own servers. An application
is described by a Pkl manifest (orch.pkl) and deployed with the deploy tool.

Start with status and list_apps to see the cluster. app_status explains why an
application is not ready (allocations, routes, cron jobs), and logs shows a
replica's output. Before deploying, read schema for the manifest format, then
run validate and plan; deploy with wait=true reports when the rollout is ready.
rollback returns an application to an earlier revision from app_history.

Remote tools use the selected orch target unless a target is passed; targets
lists them. Tools marked destructive change or delete running workloads or
data; confirm with the person before calling them.`

type mcpParam struct {
	name        string
	kind        string // string, integer, boolean, array (of strings)
	description string
	required    bool
	enum        []string
}

type mcpTool struct {
	name        string
	title       string
	description string
	params      []mcpParam
	readOnly    bool
	destructive bool
	idempotent  bool
	// remote tools talk to a cluster and accept an optional target.
	remote bool
	// json appends --json; the command prints text otherwise.
	json bool
	// build returns the orch arguments, any arguments after --, and stdin.
	build func(mcpArgs) (args, command []string, input string, err error)
}

func runMCP(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("mcp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var targetName string
	var readOnly bool
	flags.StringVar(&targetName, "target", "", "target for remote tools (defaults to the selected target)")
	flags.BoolVar(&readOnly, "read-only", false, "offer only tools that do not change the cluster")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "mcp accepts flags only")
		return ExitInvalid
	}
	server := newMCPServer(targetName, readOnly)
	if err := server.serve(ctx, stdin, stdout); err != nil && !errors.Is(err, context.Canceled) {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	return ExitOK
}

type mcpServer struct {
	target string
	tools  []mcpTool
	byName map[string]mcpTool
	// run executes one orch command; tests replace it.
	run func(ctx context.Context, args []string, stdout, stderr io.Writer) int

	writeMu  sync.Mutex
	cancelMu sync.Mutex
	cancels  map[string]context.CancelFunc
}

func newMCPServer(targetName string, readOnly bool) *mcpServer {
	server := &mcpServer{target: targetName, byName: map[string]mcpTool{}, cancels: map[string]context.CancelFunc{}, run: RunContext}
	for _, tool := range mcpTools() {
		if readOnly && !tool.readOnly {
			continue
		}
		server.tools = append(server.tools, tool)
		server.byName[tool.name] = tool
	}
	return server
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *mcpServer) serve(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	reader := bufio.NewReaderSize(stdin, 64<<10)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// At the end of input, calls already running still finish and answer.
	var calls sync.WaitGroup
	defer calls.Wait()
	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		for {
			line, err := reader.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) != 0 {
				select {
				case lines <- line:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case line := <-lines:
			var request mcpRequest
			if err := json.Unmarshal(line, &request); err != nil || request.Method == "" {
				s.respond(stdout, json.RawMessage("null"), nil, &mcpError{Code: -32700, Message: "invalid JSON-RPC message"})
				continue
			}
			if request.Method == "tools/call" && len(request.ID) != 0 {
				// Calls can run for minutes (deploy --wait), so they do not
				// hold up pings, listings or other calls.
				callCtx, callCancel := context.WithCancel(ctx)
				s.cancelMu.Lock()
				s.cancels[string(request.ID)] = callCancel
				s.cancelMu.Unlock()
				calls.Add(1)
				go func() {
					defer calls.Done()
					defer func() {
						s.cancelMu.Lock()
						delete(s.cancels, string(request.ID))
						s.cancelMu.Unlock()
						callCancel()
					}()
					result, failure := s.callTool(callCtx, request.Params)
					s.respond(stdout, request.ID, result, failure)
				}()
				continue
			}
			result, failure := s.handle(request)
			if len(request.ID) != 0 {
				s.respond(stdout, request.ID, result, failure)
			}
		}
	}
}

func (s *mcpServer) respond(stdout io.Writer, id json.RawMessage, result any, failure *mcpError) {
	message := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result,omitempty"`
		Error   *mcpError       `json:"error,omitempty"`
	}{JSONRPC: "2.0", ID: id, Result: result, Error: failure}
	if failure == nil && result == nil {
		message.Result = struct{}{}
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		encoded, _ = json.Marshal(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Error   mcpError        `json:"error"`
		}{"2.0", id, mcpError{Code: -32603, Message: err.Error()}})
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, _ = stdout.Write(append(encoded, '\n'))
}

func (s *mcpServer) handle(request mcpRequest) (any, *mcpError) {
	switch request.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(request.Params, &params)
		version := mcpLatestProtocol
		if mcpProtocols[params.ProtocolVersion] {
			version = params.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "orch", "title": "orch", "version": Version},
			"instructions":    mcpInstructions,
		}, nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		tools := make([]map[string]any, 0, len(s.tools))
		for _, tool := range s.tools {
			tools = append(tools, tool.definition())
		}
		return map[string]any{"tools": tools}, nil
	case "notifications/cancelled":
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(request.Params, &params) == nil {
			s.cancelMu.Lock()
			if cancel := s.cancels[string(params.RequestID)]; cancel != nil {
				cancel()
			}
			s.cancelMu.Unlock()
		}
		return nil, nil
	}
	if strings.HasPrefix(request.Method, "notifications/") {
		return nil, nil
	}
	return nil, &mcpError{Code: -32601, Message: "method not found: " + request.Method}
}

func (s *mcpServer) callTool(ctx context.Context, raw json.RawMessage) (any, *mcpError) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, &mcpError{Code: -32602, Message: "invalid tools/call params"}
	}
	tool, ok := s.byName[params.Name]
	if !ok {
		return nil, &mcpError{Code: -32602, Message: "unknown tool: " + params.Name}
	}
	text, failed := s.invoke(ctx, tool, mcpArgs(params.Arguments))
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": failed,
	}, nil
}

func (s *mcpServer) invoke(ctx context.Context, tool mcpTool, arguments mcpArgs) (string, bool) {
	args, command, input, err := tool.build(arguments)
	if err != nil {
		return err.Error(), true
	}
	if tool.json {
		args = append(args, "--json")
	}
	if tool.remote {
		target := s.target
		if value, err := arguments.str("target"); err != nil {
			return err.Error(), true
		} else if value != "" {
			target = value
		}
		if target != "" {
			args = append(args, "--target", target)
		}
	}
	if command != nil {
		args = append(append(args, "--"), command...)
	}
	var stdout, stderr bytes.Buffer
	code := s.run(withCommandInput(ctx, strings.NewReader(input)), args, &stdout, &stderr)
	text := strings.TrimRight(stdout.String(), "\n")
	if extra := strings.TrimRight(stderr.String(), "\n"); extra != "" {
		if text != "" {
			text += "\n\n"
		}
		text += extra
	}
	if text == "" {
		text = "done"
		if code != ExitOK {
			text = fmt.Sprintf("orch exited with status %d", code)
		}
	}
	if len(text) > mcpMaxOutput {
		text = text[:mcpMaxOutput] + "\n[output truncated]"
	}
	return text, code != ExitOK
}

func (tool mcpTool) definition() map[string]any {
	properties := map[string]any{}
	required := []string{}
	params := tool.params
	if tool.remote {
		params = append(params[:len(params):len(params)], mcpParam{name: "target", kind: "string", description: "orch target to use instead of the selected one"})
	}
	for _, param := range params {
		property := map[string]any{"type": param.kind, "description": param.description}
		if param.kind == "array" {
			property["items"] = map[string]any{"type": "string"}
		}
		if len(param.enum) != 0 {
			property["enum"] = param.enum
		}
		properties[param.name] = property
		if param.required {
			required = append(required, param.name)
		}
	}
	return map[string]any{
		"name":        tool.name,
		"title":       tool.title,
		"description": tool.description,
		"inputSchema": map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false},
		"annotations": map[string]any{
			"title":           tool.title,
			"readOnlyHint":    tool.readOnly,
			"destructiveHint": tool.destructive,
			"idempotentHint":  tool.idempotent,
			"openWorldHint":   tool.remote,
		},
	}
}

// mcpArgs reads tool arguments, rejecting values of the wrong type.
type mcpArgs map[string]any

func (a mcpArgs) str(name string) (string, error) {
	value, ok := a[name]
	if !ok || value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return text, nil
}

func (a mcpArgs) integer(name string) (int64, bool, error) {
	value, ok := a[name]
	if !ok || value == nil {
		return 0, false, nil
	}
	number, ok := value.(float64)
	if !ok || number != math.Trunc(number) || number < 0 || number > math.MaxUint32 {
		return 0, false, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return int64(number), true, nil
}

func (a mcpArgs) boolean(name string) (bool, error) {
	value, ok := a[name]
	if !ok || value == nil {
		return false, nil
	}
	flag, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return flag, nil
}

func (a mcpArgs) strings(name string) ([]string, error) {
	value, ok := a[name]
	if !ok || value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array of strings", name)
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s must be an array of strings", name)
		}
		result = append(result, text)
	}
	return result, nil
}

// mcpBuilder accumulates a command line from tool arguments and keeps the
// first error, so each tool's build reads as a flat list of fields.
type mcpBuilder struct {
	in          mcpArgs
	flags       []string
	positionals []string
	err         error
}

func (b *mcpBuilder) fail(err error) {
	if b.err == nil {
		b.err = err
	}
}

// positional adds a required (or optional) name such as an application.
// Values may not look like flags, so a name cannot smuggle in options.
func (b *mcpBuilder) positional(name string, required bool) string {
	value, err := b.in.str(name)
	if err != nil {
		b.fail(err)
		return ""
	}
	value = strings.TrimSpace(value)
	switch {
	case value == "" && required:
		b.fail(fmt.Errorf("%s is required", name))
	case strings.HasPrefix(value, "-"):
		b.fail(fmt.Errorf("%s must not start with -", name))
	case value != "":
		b.positionals = append(b.positionals, value)
	}
	return value
}

func (b *mcpBuilder) positionalList(name string, required bool) {
	values, err := b.in.strings(name)
	if err != nil {
		b.fail(err)
		return
	}
	if len(values) == 0 && required {
		b.fail(fmt.Errorf("%s is required", name))
	}
	for _, value := range values {
		if value == "" || strings.HasPrefix(value, "-") {
			b.fail(fmt.Errorf("%s entries must be non-empty and must not start with -", name))
			return
		}
		b.positionals = append(b.positionals, value)
	}
}

func (b *mcpBuilder) stringFlag(name, flagName string) {
	value, err := b.in.str(name)
	if err != nil {
		b.fail(err)
	} else if value != "" {
		b.flags = append(b.flags, "--"+flagName+"="+value)
	}
}

func (b *mcpBuilder) intFlag(name, flagName string) {
	value, ok, err := b.in.integer(name)
	if err != nil {
		b.fail(err)
	} else if ok {
		b.flags = append(b.flags, "--"+flagName+"="+strconv.FormatInt(value, 10))
	}
}

func (b *mcpBuilder) boolFlag(name, flagName string) {
	value, err := b.in.boolean(name)
	if err != nil {
		b.fail(err)
	} else if value {
		b.flags = append(b.flags, "--"+flagName)
	}
}

// mcpCommand builds a tool from the orch subcommand words and a function
// that adds the tool's fields; flags go before positionals because some
// commands stop parsing flags at the first positional.
func mcpCommand(words []string, fields func(*mcpBuilder)) func(mcpArgs) ([]string, []string, string, error) {
	return func(in mcpArgs) ([]string, []string, string, error) {
		builder := &mcpBuilder{in: in}
		if fields != nil {
			fields(builder)
		}
		if builder.err != nil {
			return nil, nil, "", builder.err
		}
		args := append(append(append([]string(nil), words...), builder.flags...), builder.positionals...)
		return args, nil, "", nil
	}
}

var (
	mcpApp      = mcpParam{name: "app", kind: "string", description: "application name", required: true}
	mcpService  = mcpParam{name: "service", kind: "string", description: "service name from the manifest", required: true}
	mcpReplica  = mcpParam{name: "replica", kind: "integer", description: "replica ordinal, starting at 1 (default 1)"}
	mcpTail     = mcpParam{name: "tail", kind: "integer", description: "maximum lines per stream, 1-10000 (default 100)"}
	mcpFile     = mcpParam{name: "file", kind: "string", description: "path to the Pkl manifest (default orch.pkl in the working directory)"}
	mcpRoot     = mcpParam{name: "root", kind: "string", description: "Pkl evaluation root (defaults to the manifest directory)"}
	mcpWait     = mcpParam{name: "wait", kind: "boolean", description: "wait until the rollout is ready or fails"}
	mcpTimeout  = mcpParam{name: "timeout", kind: "string", description: "how long to wait, as a Go duration such as 10m (default 10m)"}
	mcpJobID    = mcpParam{name: "id", kind: "string", description: "job ID", required: true}
	mcpVolume   = mcpParam{name: "volume", kind: "string", description: "volume name from the manifest", required: true}
	mcpNode     = mcpParam{name: "node", kind: "string", description: "node name", required: true}
	mcpCronJob  = mcpParam{name: "job", kind: "string", description: "cron job name from the manifest", required: true}
	mcpCommandP = mcpParam{name: "command", kind: "array", description: "command and arguments, for example [\"rails\", \"db:migrate\"]", required: true}
)

func manifestFields(b *mcpBuilder) {
	b.stringFlag("file", "file")
	b.stringFlag("root", "root")
}

func mcpTools() []mcpTool {
	return []mcpTool{
		// Local tools.
		{name: "targets", title: "List targets", readOnly: true, idempotent: true, json: true,
			description: "List the configured orch targets (clusters) and which one is selected.",
			build:       mcpCommand([]string{"target", "list"}, nil)},
		{name: "schema", title: "Manifest schema", readOnly: true, idempotent: true,
			description: "Print the Pkl schema for orch.pkl application manifests. Read it before writing or changing a manifest.",
			build:       mcpCommand([]string{"schema"}, nil)},
		{name: "validate", title: "Validate manifest", readOnly: true, idempotent: true, json: true,
			description: "Validate an orch.pkl manifest and report problems.",
			params:      []mcpParam{mcpFile, mcpRoot}, build: mcpCommand([]string{"validate"}, manifestFields)},
		{name: "plan", title: "Plan manifest change", readOnly: true, idempotent: true, json: true,
			description: "Show what a manifest defines, or what changed compared with another manifest file.",
			params:      []mcpParam{mcpFile, mcpRoot, {name: "against", kind: "string", description: "earlier manifest file to compare against"}},
			build: mcpCommand([]string{"plan"}, func(b *mcpBuilder) {
				manifestFields(b)
				b.stringFlag("against", "against")
			})},
		{name: "render", title: "Render manifest", readOnly: true, idempotent: true,
			description: "Render a manifest to the exact JSON spec orch would deploy.",
			params:      []mcpParam{mcpFile, mcpRoot}, build: mcpCommand([]string{"render"}, manifestFields)},

		// Cluster state.
		{name: "status", title: "Cluster status", readOnly: true, idempotent: true, remote: true, json: true,
			description: "Show the cluster: name, server, network, certificate expiry and controller backups.",
			build:       mcpCommand([]string{"status"}, nil)},
		{name: "list_nodes", title: "List nodes", readOnly: true, idempotent: true, remote: true, json: true,
			description: "List the cluster's nodes with their state, labels, ingress and resources.",
			build:       mcpCommand([]string{"nodes"}, nil)},
		{name: "list_apps", title: "List applications", readOnly: true, idempotent: true, remote: true, json: true,
			description: "List every application on the cluster with its revision and rollout status.",
			build:       mcpCommand([]string{"app", "list"}, nil)},
		{name: "app_status", title: "Application status", readOnly: true, idempotent: true, remote: true, json: true,
			description: "Show an application's replicas, health, routes, ports and cron jobs, with the reason anything is not ready.",
			params:      []mcpParam{mcpApp},
			build:       mcpCommand([]string{"app", "status"}, func(b *mcpBuilder) { b.positional("app", true) })},
		{name: "app_history", title: "Deployment history", readOnly: true, idempotent: true, remote: true, json: true,
			description: "List an application's deployed revisions, newest first.",
			params: []mcpParam{mcpApp, {name: "limit", kind: "integer", description: "page size, 1-50 (default 20)"},
				{name: "before_generation", kind: "integer", description: "return revisions older than this generation"}},
			build: mcpCommand([]string{"app", "history"}, func(b *mcpBuilder) {
				b.intFlag("limit", "limit")
				b.intFlag("before_generation", "before-generation")
				b.positional("app", true)
			})},
		{name: "app_diff", title: "Compare revisions", readOnly: true, idempotent: true, remote: true, json: true,
			description: "Compare a revision of an application with the current one, or with another revision.",
			params: []mcpParam{mcpApp, {name: "revision", kind: "string", description: "revision ID from app_history", required: true},
				{name: "against", kind: "string", description: "revision to compare with (default the current desired state)"}},
			build: mcpCommand([]string{"app", "diff"}, func(b *mcpBuilder) {
				b.stringFlag("against", "against")
				b.positional("app", true)
				b.positional("revision", true)
			})},
		{name: "wait_for_rollout", title: "Wait for rollout", readOnly: true, idempotent: true, remote: true, json: true,
			description: "Wait until an application's revision is ready or fails, and report the outcome.",
			params:      []mcpParam{mcpApp, {name: "revision", kind: "string", description: "revision to wait for (default the current desired state)"}, mcpTimeout},
			build: mcpCommand([]string{"app", "watch"}, func(b *mcpBuilder) {
				b.stringFlag("revision", "revision")
				b.stringFlag("timeout", "timeout")
				b.positional("app", true)
			})},
		{name: "events", title: "Cluster events", readOnly: true, idempotent: true, remote: true, json: true,
			description: "List cluster events (deployments, node changes, failures) after a sequence cursor.",
			params: []mcpParam{{name: "after", kind: "integer", description: "return events after this sequence number"},
				{name: "limit", kind: "integer", description: "maximum events, 1-200 (default 50)"}},
			build: mcpCommand([]string{"events"}, func(b *mcpBuilder) {
				b.intFlag("after", "after")
				b.intFlag("limit", "limit")
			})},
		{name: "logs", title: "Service logs", readOnly: true, idempotent: true, remote: true,
			description: "Show the latest output of one replica of a service.",
			params: []mcpParam{mcpApp, mcpService, mcpReplica, mcpTail,
				{name: "previous", kind: "boolean", description: "show the replica's previous instance, such as the one a deploy or restart replaced (kept 24 hours)"}},
			build: mcpCommand([]string{"logs"}, func(b *mcpBuilder) {
				b.intFlag("replica", "replica")
				b.intFlag("tail", "tail")
				b.boolFlag("previous", "previous")
				b.positional("app", true)
				b.positional("service", true)
			})},

		// Deploying.
		{name: "deploy", title: "Deploy", remote: true, json: true,
			description: "Deploy the application described by a manifest. Validate and plan first; use wait=true to report when it is ready.",
			params:      []mcpParam{mcpFile, mcpRoot, mcpWait, mcpTimeout},
			build: mcpCommand([]string{"deploy"}, func(b *mcpBuilder) {
				manifestFields(b)
				b.boolFlag("wait", "wait")
				b.stringFlag("timeout", "timeout")
			})},
		{name: "rollback", title: "Roll back", remote: true, json: true,
			description: "Return an application to an earlier revision from app_history.",
			params:      []mcpParam{mcpApp, {name: "revision", kind: "string", description: "revision ID to return to", required: true}, mcpWait, mcpTimeout},
			build: mcpCommand([]string{"rollback"}, func(b *mcpBuilder) {
				b.boolFlag("wait", "wait")
				b.stringFlag("timeout", "timeout")
				b.positional("app", true)
				b.positional("revision", true)
			})},
		{name: "restart", title: "Restart", remote: true,
			description: "Restart a service, or every service of an application, one replica at a time.",
			params:      []mcpParam{mcpApp, {name: "service", kind: "string", description: "service to restart (default all services)"}, {name: "replica", kind: "integer", description: "restart only this replica"}},
			build: mcpCommand([]string{"restart"}, func(b *mcpBuilder) {
				b.intFlag("replica", "replica")
				b.positional("app", true)
				b.positional("service", false)
			})},
		{name: "remove_app", title: "Remove application", remote: true, destructive: true, idempotent: true, json: true,
			description: "Remove an application and stop all of its workloads.",
			params:      []mcpParam{mcpApp},
			build:       mcpCommand([]string{"remove"}, func(b *mcpBuilder) { b.positional("app", true) })},
		{name: "exec", title: "Run in replica", remote: true, destructive: true,
			description: "Run a non-interactive command inside a running replica and return its output.",
			params:      []mcpParam{mcpApp, mcpService, mcpCommandP, mcpReplica, {name: "timeout", kind: "integer", description: "timeout in seconds, 1-300 (default 30)"}},
			build: withCommand(mcpCommand([]string{"exec"}, func(b *mcpBuilder) {
				b.intFlag("replica", "replica")
				b.intFlag("timeout", "timeout")
				b.positional("app", true)
				b.positional("service", true)
			}))},

		// Jobs and cron.
		{name: "run_job", title: "Run job", remote: true, destructive: true, json: true,
			description: "Run a one-off command, such as a migration, with a service's image and settings.",
			params:      []mcpParam{mcpApp, mcpService, mcpCommandP, {name: "wait", kind: "boolean", description: "wait for the job to finish"}, {name: "timeout", kind: "string", description: "process timeout, 10s-24h (default 15m)"}},
			build: withCommand(mcpCommand([]string{"job", "run"}, func(b *mcpBuilder) {
				b.boolFlag("wait", "wait")
				b.stringFlag("timeout", "timeout")
				b.positional("app", true)
				b.positional("service", true)
			}))},
		{name: "list_jobs", title: "List jobs", readOnly: true, idempotent: true, remote: true, json: true,
			description: "List one-off jobs, newest first, for the cluster or one application.",
			params: []mcpParam{{name: "app", kind: "string", description: "only this application"}, {name: "limit", kind: "integer", description: "maximum jobs, 1-100 (default 20)"},
				{name: "before", kind: "string", description: "job ID cursor from a previous page"}},
			build: mcpCommand([]string{"job", "list"}, func(b *mcpBuilder) {
				b.intFlag("limit", "limit")
				b.stringFlag("before", "before")
				b.positional("app", false)
			})},
		{name: "job_status", title: "Job status", readOnly: true, idempotent: true, remote: true, json: true,
			description: "Show a job's state, exit code and timing.",
			params:      []mcpParam{mcpJobID},
			build:       mcpCommand([]string{"job", "show"}, func(b *mcpBuilder) { b.positional("id", true) })},
		{name: "job_logs", title: "Job logs", readOnly: true, idempotent: true, remote: true,
			description: "Show a job's output.",
			params:      []mcpParam{mcpJobID, mcpTail},
			build: mcpCommand([]string{"job", "logs"}, func(b *mcpBuilder) {
				b.intFlag("tail", "tail")
				b.positional("id", true)
			})},
		{name: "cancel_job", title: "Cancel job", remote: true, destructive: true, idempotent: true, json: true,
			description: "Cancel a pending or running job.",
			params:      []mcpParam{mcpJobID},
			build:       mcpCommand([]string{"job", "cancel"}, func(b *mcpBuilder) { b.positional("id", true) })},
		{name: "retry_job", title: "Retry job", remote: true, json: true,
			description: "Run a finished job again with the same command.",
			params:      []mcpParam{mcpJobID},
			build:       mcpCommand([]string{"job", "retry"}, func(b *mcpBuilder) { b.positional("id", true) })},
		{name: "run_cron", title: "Run cron job now", remote: true, json: true,
			description: "Start one run of a scheduled job now, with the settings its schedule uses.",
			params:      []mcpParam{mcpApp, mcpCronJob, {name: "wait", kind: "boolean", description: "wait for the run to finish"}},
			build: mcpCommand([]string{"cron", "run"}, func(b *mcpBuilder) {
				b.boolFlag("wait", "wait")
				b.positional("app", true)
				b.positional("job", true)
			})},
		{name: "cron_runs", title: "Cron runs", readOnly: true, idempotent: true, remote: true, json: true,
			description: "List recent runs of a scheduled job.",
			params:      []mcpParam{mcpApp, mcpCronJob, {name: "limit", kind: "integer", description: "maximum runs, 1-100 (default 20)"}},
			build: mcpCommand([]string{"cron", "runs"}, func(b *mcpBuilder) {
				b.intFlag("limit", "limit")
				b.positional("app", true)
				b.positional("job", true)
			})},
		{name: "cron_logs", title: "Cron run logs", readOnly: true, idempotent: true, remote: true,
			description: "Show the output of one run of a scheduled job.",
			params:      []mcpParam{mcpApp, mcpCronJob, {name: "run_id", kind: "string", description: "run ID from cron_runs", required: true}, mcpTail},
			build: mcpCommand([]string{"cron", "logs"}, func(b *mcpBuilder) {
				b.intFlag("tail", "tail")
				b.positional("app", true)
				b.positional("job", true)
				b.positional("run_id", true)
			})},

		// Secrets and registries.
		{name: "list_secrets", title: "List secrets", readOnly: true, idempotent: true, remote: true, json: true,
			description: "List an application's secret names and versions. Values are write-only and never shown.",
			params:      []mcpParam{mcpApp},
			build:       mcpCommand([]string{"secret", "list"}, func(b *mcpBuilder) { b.positional("app", true) })},
		{name: "set_secret", title: "Set secret", remote: true, destructive: true, json: true,
			description: "Store or replace an application secret. Running replicas get the new value on their next deploy or restart.",
			params:      []mcpParam{mcpApp, {name: "name", kind: "string", description: "secret name, a lowercase DNS label such as api-token", required: true}, {name: "value", kind: "string", description: "secret value", required: true}},
			build:       withInput("value", mcpCommand([]string{"secret", "set", "--from-stdin"}, func(b *mcpBuilder) { b.positional("app", true); b.positional("name", true) }))},
		{name: "delete_secret", title: "Delete secret", remote: true, destructive: true, idempotent: true, json: true,
			description: "Delete an application secret.",
			params:      []mcpParam{mcpApp, {name: "name", kind: "string", description: "secret name", required: true}},
			build:       mcpCommand([]string{"secret", "delete"}, func(b *mcpBuilder) { b.positional("app", true); b.positional("name", true) })},
		{name: "list_registries", title: "List registries", readOnly: true, idempotent: true, remote: true, json: true,
			description: "List the private image registries the cluster can pull from.",
			build:       mcpCommand([]string{"registry", "list"}, nil)},
		{name: "registry_login", title: "Registry login", remote: true, idempotent: true, json: true,
			description: "Store credentials for a private image registry.",
			params: []mcpParam{{name: "host", kind: "string", description: "registry host, such as ghcr.io", required: true},
				{name: "username", kind: "string", description: "registry username", required: true}, {name: "password", kind: "string", description: "password or token", required: true}},
			build: withInput("password", mcpCommand([]string{"registry", "login", "--password-stdin"}, func(b *mcpBuilder) {
				b.stringFlag("username", "username")
				b.positional("host", true)
			}))},
		{name: "registry_logout", title: "Registry logout", remote: true, destructive: true, idempotent: true, json: true,
			description: "Delete stored credentials for a private image registry.",
			params:      []mcpParam{{name: "host", kind: "string", description: "registry host", required: true}},
			build:       mcpCommand([]string{"registry", "logout"}, func(b *mcpBuilder) { b.positional("host", true) })},

		// Volumes.
		{name: "list_volumes", title: "List volumes", readOnly: true, idempotent: true, remote: true, json: true,
			description: "List local volumes, their node and their latest backup, for the cluster or one application.",
			params:      []mcpParam{{name: "app", kind: "string", description: "only this application"}},
			build:       mcpCommand([]string{"volume", "list"}, func(b *mcpBuilder) { b.positional("app", false) })},
		{name: "list_volume_backups", title: "List volume backups", readOnly: true, idempotent: true, remote: true, json: true,
			description: "List the backups of one volume.",
			params:      []mcpParam{mcpApp, mcpVolume},
			build:       mcpCommand([]string{"volume", "backups"}, func(b *mcpBuilder) { b.positional("app", true); b.positional("volume", true) })},
		{name: "backup_volume", title: "Back up volume", remote: true, json: true,
			description: "Back up a volume now, on the node that holds it.",
			params:      []mcpParam{mcpApp, mcpVolume},
			build:       mcpCommand([]string{"volume", "backup"}, func(b *mcpBuilder) { b.positional("app", true); b.positional("volume", true) })},
		{name: "restore_volume", title: "Restore volume", remote: true, destructive: true, json: true,
			description: "Replace a volume's contents with a backup. Stops the service that mounts it while restoring; the previous contents are kept as a backup first.",
			params:      []mcpParam{mcpApp, mcpVolume, {name: "backup", kind: "string", description: "backup ID from list_volume_backups", required: true}},
			build: mcpCommand([]string{"volume", "restore"}, func(b *mcpBuilder) {
				b.positional("app", true)
				b.positional("volume", true)
				b.positional("backup", true)
			})},

		// Nodes.
		{name: "update_node", title: "Cordon, drain or reactivate node", remote: true, destructive: true, idempotent: true, json: true,
			description: "cordon stops new work landing on a node, drain also moves its workloads away, uncordon makes it schedulable again.",
			params:      []mcpParam{mcpNode, {name: "action", kind: "string", description: "what to do", required: true, enum: []string{"cordon", "drain", "uncordon"}}},
			build: func(in mcpArgs) ([]string, []string, string, error) {
				action, err := in.str("action")
				if err != nil {
					return nil, nil, "", err
				}
				if action != "cordon" && action != "drain" && action != "uncordon" {
					return nil, nil, "", errors.New("action must be cordon, drain or uncordon")
				}
				return mcpCommand([]string{"node", action}, func(b *mcpBuilder) { b.positional("node", true) })(in)
			}},
		{name: "label_node", title: "Label node", remote: true, idempotent: true, json: true,
			description: "Set labels on a node; manifests use them to place services.",
			params:      []mcpParam{mcpNode, {name: "labels", kind: "array", description: "labels as KEY=VALUE", required: true}},
			build: mcpCommand([]string{"node", "label"}, func(b *mcpBuilder) {
				b.positional("node", true)
				b.positionalList("labels", true)
			})},
		{name: "unlabel_node", title: "Remove node labels", remote: true, idempotent: true, json: true,
			description: "Remove labels from a node.",
			params:      []mcpParam{mcpNode, {name: "keys", kind: "array", description: "label keys to remove", required: true}},
			build: mcpCommand([]string{"node", "unlabel"}, func(b *mcpBuilder) {
				b.positional("node", true)
				b.positionalList("keys", true)
			})},
	}
}

// withCommand passes the command argument after --, verbatim.
func withCommand(build func(mcpArgs) ([]string, []string, string, error)) func(mcpArgs) ([]string, []string, string, error) {
	return func(in mcpArgs) ([]string, []string, string, error) {
		args, _, input, err := build(in)
		if err != nil {
			return nil, nil, "", err
		}
		command, err := in.strings("command")
		if err != nil {
			return nil, nil, "", err
		}
		if len(command) == 0 || command[0] == "" {
			return nil, nil, "", errors.New("command is required")
		}
		return args, command, input, nil
	}
}

// withInput sends a write-only value on stdin, as the CLI expects for
// secrets, instead of putting it on the command line.
func withInput(name string, build func(mcpArgs) ([]string, []string, string, error)) func(mcpArgs) ([]string, []string, string, error) {
	return func(in mcpArgs) ([]string, []string, string, error) {
		args, command, _, err := build(in)
		if err != nil {
			return nil, nil, "", err
		}
		value, err := in.str(name)
		if err != nil {
			return nil, nil, "", err
		}
		if value == "" {
			return nil, nil, "", fmt.Errorf("%s is required", name)
		}
		return args, command, value, nil
	}
}
