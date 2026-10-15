package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeProvider struct {
	cluster      *orchv1alpha1.Cluster
	applications []*orchv1alpha1.ApplicationSummary
	nodes        []*orchv1alpha1.Node
	runtimes     []*orchv1alpha1.RuntimeSummary
	events       []*orchv1alpha1.Event
	update       *orchv1alpha1.UpdateNodeRequest
	err          error
}

func (f *fakeProvider) ListDeployments(context.Context, string, uint64, uint32) (*orchv1alpha1.ListDeploymentsResponse, error) {
	return &orchv1alpha1.ListDeploymentsResponse{}, f.err
}
func (f *fakeProvider) CompareDeployments(context.Context, string, string, string) (*orchv1alpha1.CompareDeploymentsResponse, error) {
	return &orchv1alpha1.CompareDeploymentsResponse{}, f.err
}
func (f *fakeProvider) RollbackApplication(context.Context, *orchv1alpha1.RollbackApplicationRequest) (*orchv1alpha1.ApplyApplicationResponse, error) {
	return &orchv1alpha1.ApplyApplicationResponse{}, f.err
}

func (f *fakeProvider) GetCluster(context.Context) (*orchv1alpha1.Cluster, error) {
	return f.cluster, f.err
}
func (f *fakeProvider) ListApplications(context.Context) ([]*orchv1alpha1.ApplicationSummary, error) {
	return f.applications, f.err
}
func (f *fakeProvider) ListRuntimes(context.Context, uint32) (*orchv1alpha1.ListRuntimesResponse, error) {
	return &orchv1alpha1.ListRuntimesResponse{Runtimes: f.runtimes}, f.err
}
func (f *fakeProvider) GetApplication(_ context.Context, name string) (*orchv1alpha1.Application, error) {
	return &orchv1alpha1.Application{Name: name, Allocations: []*orchv1alpha1.AllocationStatus{{Application: name, Service: "web"}}}, f.err
}
func (f *fakeProvider) ListNodes(context.Context) ([]*orchv1alpha1.Node, error) {
	return f.nodes, f.err
}
func (f *fakeProvider) ListEvents(context.Context, uint64, uint32) (*orchv1alpha1.ListEventsResponse, error) {
	return &orchv1alpha1.ListEventsResponse{Events: f.events, NextAfterSequence: uint64(len(f.events))}, f.err
}
func (f *fakeProvider) ListRecentEvents(context.Context, uint32) (*orchv1alpha1.ListRecentEventsResponse, error) {
	return &orchv1alpha1.ListRecentEventsResponse{Events: f.events, NextAfterSequence: uint64(len(f.events))}, f.err
}
func (f *fakeProvider) UpdateNode(_ context.Context, request *orchv1alpha1.UpdateNodeRequest) (*orchv1alpha1.UpdateNodeResponse, error) {
	f.update = request
	return &orchv1alpha1.UpdateNodeResponse{Node: &orchv1alpha1.Node{Name: request.GetName()}, Changed: true, EventSequence: 42}, f.err
}
func (f *fakeProvider) LoadLogs(context.Context, string, string, uint32, uint32, string) (LogBatch, error) {
	return LogBatch{Lines: []LogLine{{Stream: "stdout", Text: "ready\nmatched output\n"}, {Stream: "stderr", Text: "matched warning\n"}}, NextCursor: "next"}, f.err
}
func (f *fakeProvider) LoadCronLogs(context.Context, string, string, string, uint32, string) (LogBatch, error) {
	return LogBatch{Lines: []LogLine{{Stream: "stdout", Text: "backup complete\n"}}, NextCursor: "next", Exited: true}, f.err
}

func TestInitialRefreshAndResponsiveRenders(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	provider := fixtureProvider()
	model := New(context.Background(), provider, "production", "ssh", time.Minute)
	message := model.Init()()
	batch, ok := message.(tea.BatchMsg)
	if !ok || len(batch) == 0 {
		t.Fatalf("initial command returned %T", message)
	}
	var refreshed tea.Msg
	for _, command := range batch {
		if candidate := command(); candidate != nil {
			if _, ok := candidate.(refreshMsg); ok {
				refreshed = candidate
				break
			}
		}
	}
	if refreshed == nil {
		t.Fatal("initial refresh message missing")
	}
	_, _ = model.Update(refreshed)
	for _, size := range [][2]int{{60, 18}, {80, 24}, {120, 40}} {
		for _, view := range []ViewID{ViewOverview, ViewApplications, ViewNodes, ViewEvents, ViewLogs} {
			model.width, model.height, model.view = size[0], size[1], view
			rendered := model.View().Content
			if strings.Contains(rendered, "\x1b[") {
				t.Fatalf("NO_COLOR render at %dx%d contains ANSI styling", size[0], size[1])
			}
			if !strings.Contains(rendered, "ORCH") || !strings.Contains(rendered, "production") {
				t.Fatalf("render at %dx%d lacks identity: %q", size[0], size[1], rendered)
			}
			lines := strings.Split(rendered, "\n")
			if len(lines) > size[1]+1 {
				t.Fatalf("render at %dx%d has %d lines", size[0], size[1], len(lines))
			}
			for line, value := range lines {
				if lipgloss.Width(value) > size[0] {
					t.Fatalf("view %v at %dx%d line %d width %d", view, size[0], size[1], line, lipgloss.Width(value))
				}
			}
		}
	}
}

func TestPaletteNavigationAndNodeConfirmation(t *testing.T) {
	provider := fixtureProvider()
	model := New(context.Background(), provider, "production", "direct", time.Minute)
	model.snapshot = fixtureSnapshot(provider)
	model.width, model.height = 100, 30
	model.openPalette("api")
	items := model.paletteItems()
	found := -1
	for index, item := range items {
		if item.name == "api" {
			found = index
		}
	}
	if found < 0 {
		t.Fatalf("palette items = %#v", items)
	}
	model.paletteIndex = found
	_, _ = model.handlePalette(tea.KeyPressMsg{Code: tea.KeyEnter})
	if model.view != ViewApplications || model.selected[ViewApplications] != 1 {
		t.Fatalf("palette selection view=%v index=%d", model.view, model.selected[ViewApplications])
	}
	_, _ = model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if model.selected[ViewApplications] != 0 {
		t.Fatalf("mouse wheel selection=%d", model.selected[ViewApplications])
	}
	model.view = ViewNodes
	model.selected[ViewNodes] = 0
	model.prepareNodeAction("d")
	if model.confirm == nil || model.confirm.request.GetLifecycle() != orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_DRAINING {
		t.Fatalf("confirmation = %#v", model.confirm)
	}
	commandModel, command := model.handleKey(tea.KeyPressMsg{Code: 'y', Text: "y"})
	model = commandModel.(*Model)
	result := command().(mutationMsg)
	_, followup := model.Update(result)
	if provider.update == nil || provider.update.GetName() != "node-1" || followup == nil {
		t.Fatalf("mutation request=%#v followup=%v", provider.update, followup)
	}
}

func TestStaleGenerationAndSanitization(t *testing.T) {
	model := New(context.Background(), fixtureProvider(), "production", "direct", time.Minute)
	model.generation = 2
	old := refreshMsg{generation: 1, snapshot: Snapshot{Cluster: &orchv1alpha1.Cluster{Name: "wrong"}}}
	_, _ = model.Update(old)
	if model.snapshot.Cluster != nil {
		t.Fatal("stale refresh overwrote snapshot")
	}
	value := sanitize("ok\x1b[31m\x00bad")
	if strings.Contains(value, "\x1b") || strings.ContainsRune(value, '\x00') || !strings.Contains(value, "bad") {
		t.Fatalf("sanitize = %q", value)
	}
}

func TestEventAndLogInspectionControls(t *testing.T) {
	provider := fixtureProvider()
	provider.events = append(provider.events, &orchv1alpha1.Event{Sequence: 2, Kind: "node.disconnected", PayloadJson: `{"name":"node-1"}`})
	model := New(context.Background(), provider, "production", "direct", time.Minute)
	model.snapshot = fixtureSnapshot(provider)
	model.view = ViewEvents
	model.applySearch("node")
	if events := model.filteredEvents(); len(events) != 1 || events[0].GetSequence() != 2 {
		t.Fatalf("filtered events = %#v", events)
	}
	model.view = ViewLogs
	model.runtimeFocus = 1
	model.logs = []LogLine{{Stream: "stdout", Text: "ready\nmatched output\n"}, {Stream: "stderr", Text: "matched warning\n"}}
	model.logSource = logSource{kind: "workload-log", application: "web", resource: "web", replica: 1}
	model.applySearch("matched")
	if lines := model.displayLogs(); len(lines) != 2 {
		t.Fatalf("filtered logs = %#v", lines)
	}
	_, _ = model.handleKey(tea.KeyPressMsg{Code: 's', Text: "s"})
	if model.logStream != "stdout" || len(model.displayLogs()) != 1 {
		t.Fatalf("stream=%q logs=%#v", model.logStream, model.displayLogs())
	}
	_, _ = model.handleKey(tea.KeyPressMsg{Code: 'p', Text: "p"})
	if !model.logPaused {
		t.Fatal("pause key did not pause log refresh")
	}
}

func TestRuntimeWorkspaceSelectsExactReplicaAndCronRun(t *testing.T) {
	provider := fixtureProvider()
	provider.runtimes = append(provider.runtimes,
		&orchv1alpha1.RuntimeSummary{Id: "web-web-2", Kind: orchv1alpha1.RuntimeKind_RUNTIME_KIND_SERVICE, Application: "web", Resource: "web", Replica: 2, NodeName: "node-2", AllocationPhase: orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_RUNNING},
		&orchv1alpha1.RuntimeSummary{Id: "run-exact", RunId: "run-exact", Kind: orchv1alpha1.RuntimeKind_RUNTIME_KIND_CRON_RUN, Application: "web", Resource: "backup", JobRunState: orchv1alpha1.JobRunState_JOB_RUN_STATE_SUCCEEDED})
	model := New(context.Background(), provider, "production", "ssh", time.Minute)
	model.snapshot = fixtureSnapshot(provider)
	model.view = ViewLogs
	model.selectRuntimeID("web-web-2")
	command := model.openSelected()
	if command == nil {
		t.Fatal("replica selection did not load logs")
	}
	message := command().(logsMsg)
	if message.replica != 2 || message.resource != "web" || message.runID != "" {
		t.Fatalf("replica log message = %#v", message)
	}
	_, _ = model.Update(message)
	if model.logSource.replica != 2 || model.runtimeFocus != 1 || model.logCursor != "next" {
		t.Fatalf("loaded source = %#v, focus=%d cursor=%q", model.logSource, model.runtimeFocus, model.logCursor)
	}
	model.runtimeFocus = 0
	model.selectRuntimeID("run-exact")
	message = model.openSelected()().(logsMsg)
	if message.runID != "run-exact" || message.kind != "cron-log" {
		t.Fatalf("cron log message = %#v", message)
	}
}

func TestRuntimeGroupingPreservesSelectionAndScrollingPausesFollow(t *testing.T) {
	provider := fixtureProvider()
	provider.runtimes = append(provider.runtimes, &orchv1alpha1.RuntimeSummary{Id: "api-worker-1", Kind: orchv1alpha1.RuntimeKind_RUNTIME_KIND_SERVICE, Application: "api", Resource: "worker", Replica: 1, NodeName: "node-0"})
	model := New(context.Background(), provider, "production", "direct", time.Minute)
	model.snapshot = fixtureSnapshot(provider)
	model.view = ViewLogs
	model.runtimeSelected = 1
	want := model.selectedRuntimeID()
	_, _ = model.handleKey(tea.KeyPressMsg{Code: 'g', Text: "g"})
	if model.runtimeGroup != "node" || model.selectedRuntimeID() != want {
		t.Fatalf("group=%q selected=%q, want %q", model.runtimeGroup, model.selectedRuntimeID(), want)
	}
	model.runtimeFocus = 1
	model.logs = []LogLine{{Stream: "stdout", Text: "one\ntwo\nthree\n"}}
	model.selected[ViewLogs] = 2
	_, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyUp})
	if !model.logPaused || model.selected[ViewLogs] != 1 {
		t.Fatalf("paused=%v selected=%d", model.logPaused, model.selected[ViewLogs])
	}
	_, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnd})
	if model.logPaused || model.selected[ViewLogs] != 2 {
		t.Fatalf("resumed=%v selected=%d", !model.logPaused, model.selected[ViewLogs])
	}
}

func TestIncrementalLogsFollowBottomAndExposeReset(t *testing.T) {
	model := New(context.Background(), fixtureProvider(), "production", "direct", time.Minute)
	model.view, model.runtimeFocus = ViewLogs, 1
	source := logSource{kind: "workload-log", application: "web", resource: "web", replica: 1}
	model.logSource, model.logCursor = source, "cursor-one"
	model.logs = []LogLine{{Stream: "stdout", Text: "one\ntwo\n"}}
	model.selected[ViewLogs] = 1
	_, _ = model.Update(logsMsg{kind: source.kind, application: source.application, resource: source.resource, replica: source.replica, batch: LogBatch{Lines: []LogLine{{Stream: "stdout", Text: "three\n"}}, NextCursor: "cursor-two"}})
	if got := len(model.displayLogs()); got != 3 || model.selected[ViewLogs] != 2 || model.logUnseen != 0 || model.logCursor != "cursor-two" {
		t.Fatalf("follow append lines=%d selected=%d unseen=%d cursor=%q", got, model.selected[ViewLogs], model.logUnseen, model.logCursor)
	}
	model.logPaused, model.selected[ViewLogs] = true, 0
	_, _ = model.Update(logsMsg{kind: source.kind, application: source.application, resource: source.resource, replica: source.replica, batch: LogBatch{Lines: []LogLine{{Stream: "stderr", Text: "four\n"}}, NextCursor: "cursor-three"}})
	if model.logUnseen != 1 || model.selected[ViewLogs] != 0 {
		t.Fatalf("paused append selected=%d unseen=%d", model.selected[ViewLogs], model.logUnseen)
	}
	_, _ = model.Update(logsMsg{kind: source.kind, application: source.application, resource: source.resource, replica: source.replica, batch: LogBatch{Lines: []LogLine{{Stream: "stdout", Text: "rotated\n"}}, NextCursor: "cursor-four", CursorReset: true}})
	if len(model.displayLogs()) != 1 || !strings.Contains(model.logNotice, "cursor reset") {
		t.Fatalf("reset logs=%#v notice=%q", model.logs, model.logNotice)
	}
}

func TestBoundedSnapshotRenderBudget(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	model := New(context.Background(), fixtureProvider(), "production", "wireguard", time.Minute)
	model.width, model.height = 120, 40
	model.snapshot = maximumSnapshot()
	for _, view := range []ViewID{ViewOverview, ViewApplications, ViewNodes, ViewEvents, ViewLogs} {
		model.view = view
		started := time.Now()
		_ = model.View()
		if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
			t.Fatalf("view %v rendered in %s, over 50ms budget", view, elapsed)
		}
	}
}

func BenchmarkMaximumSnapshotRender(b *testing.B) {
	model := New(context.Background(), fixtureProvider(), "production", "wireguard", time.Minute)
	model.width, model.height = 120, 40
	model.snapshot = maximumSnapshot()
	for b.Loop() {
		_ = model.View()
	}
}

func fixtureProvider() *fakeProvider {
	now := timestamppb.Now()
	return &fakeProvider{
		cluster: &orchv1alpha1.Cluster{Name: "demo", Id: "clu_demo"},
		applications: []*orchv1alpha1.ApplicationSummary{
			{Name: "web", Generation: 2, ObservedGeneration: 2, ActiveGeneration: 2, ExecutionStatus: orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_HEALTHY, ExecutionUpdatedAt: now},
			{Name: "api", Generation: 3, ObservedGeneration: 2, ActiveGeneration: 2, ExecutionStatus: orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_STARTING, ExecutionUpdatedAt: now},
		},
		nodes:    []*orchv1alpha1.Node{{Name: "node-1", ConnectionStatus: orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_CONNECTED, Lifecycle: orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_ACTIVE, AllocatableCpuMillis: 4000, AllocatableMemoryBytes: 8 << 30}},
		runtimes: []*orchv1alpha1.RuntimeSummary{{Id: "web-web-1", Kind: orchv1alpha1.RuntimeKind_RUNTIME_KIND_SERVICE, Application: "web", Resource: "web", Replica: 1, NodeName: "node-1", NodeConnection: orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_CONNECTED, AllocationPhase: orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_RUNNING, HealthStatus: orchv1alpha1.HealthStatus_HEALTH_STATUS_READY, UpdatedAt: now}},
		events:   []*orchv1alpha1.Event{{Sequence: 1, Kind: "application.healthy", PayloadJson: `{"name":"web"}`, CreatedAt: now}},
	}
}

func fixtureSnapshot(provider *fakeProvider) Snapshot {
	return Snapshot{Cluster: provider.cluster, Applications: provider.applications, Runtimes: provider.runtimes, Nodes: provider.nodes, Events: provider.events, EventCursor: 1, RefreshedAt: time.Now()}
}

func maximumSnapshot() Snapshot {
	now := timestamppb.Now()
	snapshot := Snapshot{Cluster: &orchv1alpha1.Cluster{Name: "maximum"}, RefreshedAt: time.Now(), EventCursor: 1000}
	for index := 0; index < 100; index++ {
		snapshot.Applications = append(snapshot.Applications, &orchv1alpha1.ApplicationSummary{Name: fmt.Sprintf("application-%03d", index), Generation: 2, ObservedGeneration: 2, ActiveGeneration: 2, ExecutionStatus: orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_HEALTHY, ExecutionUpdatedAt: now})
	}
	for index := 0; index < 20; index++ {
		snapshot.Nodes = append(snapshot.Nodes, &orchv1alpha1.Node{Name: fmt.Sprintf("node-%02d", index), ConnectionStatus: orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_CONNECTED, Lifecycle: orchv1alpha1.NodeLifecycle_NODE_LIFECYCLE_ACTIVE})
	}
	for index := 0; index < 200; index++ {
		snapshot.Runtimes = append(snapshot.Runtimes, &orchv1alpha1.RuntimeSummary{Id: fmt.Sprintf("runtime-%03d", index), Kind: orchv1alpha1.RuntimeKind_RUNTIME_KIND_SERVICE, Application: fmt.Sprintf("application-%03d", index%100), Resource: "web", Replica: uint32(index%3 + 1), NodeName: fmt.Sprintf("node-%02d", index%20), AllocationPhase: orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_RUNNING, UpdatedAt: now})
	}
	for index := 1; index <= 1000; index++ {
		snapshot.Events = append(snapshot.Events, &orchv1alpha1.Event{Sequence: uint64(index), Kind: "application.execution.changed", PayloadJson: `{"status":"healthy"}`, CreatedAt: now})
	}
	return snapshot
}
