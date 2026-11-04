package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// moveAgent answers a node's assignments and volume commands the way an
// agent does, keeping each volume backup in memory.
type moveAgent struct {
	t       *testing.T
	state   *store.Store
	manager *sessionManager
	session *agentSession
	nodeID  string

	mu       sync.Mutex
	backups  map[string][]byte
	incoming map[string][]byte
	restored []string
	stopped  []string
	failOn   orchv1alpha1.VolumeAction
}

func startMoveAgent(t *testing.T, state *store.Store, manager *sessionManager, nodeID string) *moveAgent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	session, err := manager.register(ctx, nodeID, "boot-"+nodeID, cancel)
	if err != nil {
		t.Fatal(err)
	}
	session.volumeMoves = true
	agent := &moveAgent{t: t, state: state, manager: manager, session: session, nodeID: nodeID,
		backups: make(map[string][]byte), incoming: make(map[string][]byte)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case message := <-session.send:
				agent.handle(ctx, message)
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return agent
}

func (a *moveAgent) handle(ctx context.Context, message *orchv1alpha1.ConnectResponse) {
	switch value := message.GetMessage().(type) {
	case *orchv1alpha1.ConnectResponse_Assignment:
		snapshot := value.Assignment
		_ = a.state.AcknowledgeAssignment(ctx, a.nodeID, snapshot.GetGeneration(), snapshot.GetDigest())
	case *orchv1alpha1.ConnectResponse_Volume:
		for _, response := range a.volume(value.Volume) {
			_ = a.manager.routeOperation(a.session, response)
		}
	}
}

func (a *moveAgent) volume(command *orchv1alpha1.VolumeCommand) []*orchv1alpha1.AgentOperationResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	reply := func(response *orchv1alpha1.VolumeResponse) *orchv1alpha1.AgentOperationResult {
		response.RequestId = command.GetRequestId()
		return &orchv1alpha1.AgentOperationResult{Result: &orchv1alpha1.AgentOperationResult_Volume{Volume: response}}
	}
	if command.GetAction() == a.failOn {
		return []*orchv1alpha1.AgentOperationResult{{Result: &orchv1alpha1.AgentOperationResult_Error{Error: &orchv1alpha1.OperationError{
			RequestId: command.GetRequestId(), Code: "failed", Message: "disk full"}}}}
	}
	key := command.GetVolume()
	switch command.GetAction() {
	case orchv1alpha1.VolumeAction_VOLUME_ACTION_BACKUP:
		a.stopped = append(a.stopped, command.GetAllocationId())
		name := "20261103T120000Z-manual"
		a.backups[key+"/"+name] = bytes.Repeat([]byte(key), 300_000)
		return []*orchv1alpha1.AgentOperationResult{reply(&orchv1alpha1.VolumeResponse{Complete: true, Backup: &orchv1alpha1.VolumeBackupInfo{
			Name: name, CreatedAt: timestamppb.New(time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)), SizeBytes: uint64(len(a.backups[key+"/"+name])),
		}})}
	case orchv1alpha1.VolumeAction_VOLUME_ACTION_READ:
		data := a.backups[key+"/"+command.GetBackup()]
		end := min(uint64(len(data)), command.GetOffset()+volumeReadTestWindow)
		var frames []*orchv1alpha1.AgentOperationResult
		for offset := command.GetOffset(); offset < end; offset += 64 << 10 {
			stop := min(end, offset+64<<10)
			frames = append(frames, reply(&orchv1alpha1.VolumeResponse{Data: data[offset:stop], SizeBytes: uint64(len(data)), Complete: stop == end}))
		}
		return frames
	case orchv1alpha1.VolumeAction_VOLUME_ACTION_WRITE:
		name := key + "/" + command.GetBackup()
		if uint64(len(a.incoming[name])) != command.GetOffset() {
			a.t.Errorf("write at %d, have %d", command.GetOffset(), len(a.incoming[name]))
		}
		a.incoming[name] = append(a.incoming[name], command.GetData()...)
		if command.GetComplete() {
			a.backups[name] = a.incoming[name]
		}
		return []*orchv1alpha1.AgentOperationResult{reply(&orchv1alpha1.VolumeResponse{Complete: true})}
	case orchv1alpha1.VolumeAction_VOLUME_ACTION_RESTORE:
		a.restored = append(a.restored, key+"/"+command.GetBackup())
		return []*orchv1alpha1.AgentOperationResult{reply(&orchv1alpha1.VolumeResponse{Complete: true})}
	}
	return []*orchv1alpha1.AgentOperationResult{{Result: &orchv1alpha1.AgentOperationResult_Error{Error: &orchv1alpha1.OperationError{
		RequestId: command.GetRequestId(), Code: "failed", Message: "unexpected action"}}}}
}

// volumeReadTestWindow matches the agent's read window.
const volumeReadTestWindow = 1 << 20

type volumeMoveFixture struct {
	state      *store.Store
	service    *service
	ctx        context.Context
	now        *time.Time
	from, to   string
	fromAgent  *moveAgent
	toAgent    *moveAgent
	toNodeName string
}

func newVolumeMoveFixture(t *testing.T) volumeMoveFixture {
	t.Helper()
	state, _, now := readyDistributedCluster(t, nil)
	cluster, err := state.Cluster(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return *now }
	sessions := newSessionManager(clock)
	reconciler := NewReconciler(state, cluster, clock, sessions.PublishAssignment)
	reconciler.sessions = sessions
	s := newService(state, cluster, nil, nil, nil, sessions, clock)
	s.reconciler = reconciler
	s.random = rand.Reader
	agents := map[string]*moveAgent{
		"node-one": startMoveAgent(t, state, sessions, "node-one"),
		"node-two": startMoveAgent(t, state, sessions, "node-two"),
	}
	// A database with a dump volume mounts two volumes; both move.
	manifest := newTestManifest("shop", map[string]config.Service{
		"db": {Image: executableImage, Mounts: []config.Mount{{Source: "data", Target: "/data"}, {Source: "dumps", Target: "/dumps"}}},
	})
	manifest.Volumes = map[string]config.Volume{"data": {Driver: "local"}, "dumps": {Driver: "local"}}
	applyManifest(t, state, manifest, "revision-one", "application-key-one", *now)
	*now = now.Add(time.Second)
	ctx := context.WithValue(context.Background(), principalKey{}, authenticatedPrincipal{certificate: &x509.Certificate{}})
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	from, err := state.VolumeClaim(ctx, "shop", "data")
	if err != nil || from == "" {
		t.Fatalf("volume claim = %q, %v", from, err)
	}
	to, toName := "node-two", "node-2"
	if from == "node-two" {
		to, toName = "node-one", "node-1"
	}
	return volumeMoveFixture{state: state, service: s, ctx: ctx, now: now, from: from, to: to,
		fromAgent: agents[from], toAgent: agents[to], toNodeName: toName}
}

func (f volumeMoveFixture) placement(t *testing.T) string {
	t.Helper()
	for _, nodeID := range []string{"node-one", "node-two"} {
		assignment, err := f.state.CurrentAssignment(f.ctx, nodeID)
		if err != nil {
			t.Fatal(err)
		}
		for _, allocation := range assignment.Allocations {
			if allocation.Application == "shop" && allocation.Service == "db" {
				return nodeID
			}
		}
	}
	return ""
}

func TestMoveVolumeCopiesEveryVolumeOfTheServiceAndStartsItOnTheNewNode(t *testing.T) {
	f := newVolumeMoveFixture(t)
	before, _, err := f.state.DesiredAllocationNode(f.ctx, "shop", "db", 1)
	if err != nil {
		t.Fatal(err)
	}
	claimedAt, err := f.state.VolumeClaimedAt(f.ctx, "shop", "data")
	if err != nil {
		t.Fatal(err)
	}
	response, err := f.service.MoveVolume(f.ctx, connect.NewRequest(&orchv1alpha1.MoveVolumeRequest{Application: "shop", Volume: "dumps", Node: f.toNodeName}))
	if err != nil {
		t.Fatal(err)
	}
	if response.Msg.GetService() != "db" || response.Msg.GetToNode() != f.toNodeName || len(response.Msg.GetVolumes()) != 2 ||
		response.Msg.GetVolumes()[0].GetVolume() != "data" || response.Msg.GetVolumes()[1].GetVolume() != "dumps" {
		t.Fatalf("response = %#v", response.Msg)
	}
	f.fromAgent.mu.Lock()
	stopped := append([]string(nil), f.fromAgent.stopped...)
	source := f.fromAgent.backups["data/20261103T120000Z-manual"]
	f.fromAgent.mu.Unlock()
	if len(stopped) != 2 || stopped[0] != before.ID {
		t.Fatalf("final backups stopped %v, want allocation %s", stopped, before.ID)
	}
	f.toAgent.mu.Lock()
	copied := f.toAgent.backups["data/20261103T120000Z-manual"]
	restored := append([]string(nil), f.toAgent.restored...)
	f.toAgent.mu.Unlock()
	if len(source) <= volumeReadTestWindow || !bytes.Equal(copied, source) {
		t.Fatalf("copied %d bytes of %d", len(copied), len(source))
	}
	if strings.Join(restored, ",") != "data/20261103T120000Z-manual,dumps/20261103T120000Z-manual" {
		t.Fatalf("restored = %v", restored)
	}
	for _, volume := range []string{"data", "dumps"} {
		if claim, err := f.state.VolumeClaim(f.ctx, "shop", volume); err != nil || claim != f.to {
			t.Fatalf("%s claim = %q, %v", volume, claim, err)
		}
	}
	// The data moved with the claim, so its old backups are not a lost node's.
	if moved, err := f.state.VolumeClaimedAt(f.ctx, "shop", "data"); err != nil || !moved.Equal(claimedAt) {
		t.Fatalf("claimed at = %v, %v; want %v", moved, err, claimedAt)
	}
	if node := f.placement(t); node != f.to {
		t.Fatalf("service placed on %q, want %q", node, f.to)
	}
	if moves, err := f.state.VolumeMoves(f.ctx); err != nil || len(moves) != 0 {
		t.Fatalf("moves = %v, %v", moves, err)
	}
}

func TestMoveVolumeFailureStartsTheServiceOnItsOldNode(t *testing.T) {
	f := newVolumeMoveFixture(t)
	f.toAgent.mu.Lock()
	f.toAgent.failOn = orchv1alpha1.VolumeAction_VOLUME_ACTION_WRITE
	f.toAgent.mu.Unlock()
	_, err := f.service.MoveVolume(f.ctx, connect.NewRequest(&orchv1alpha1.MoveVolumeRequest{Application: "shop", Volume: "data", Node: f.toNodeName}))
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("MoveVolume() = %v", err)
	}
	if claim, err := f.state.VolumeClaim(f.ctx, "shop", "data"); err != nil || claim != f.from {
		t.Fatalf("claim = %q, %v", claim, err)
	}
	if node := f.placement(t); node != f.from {
		t.Fatalf("service placed on %q, want %q", node, f.from)
	}
	if moves, err := f.state.VolumeMoves(f.ctx); err != nil || len(moves) != 0 {
		t.Fatalf("moves = %v, %v", moves, err)
	}
}

func TestMoveVolumeRejectsUnusableDestinations(t *testing.T) {
	f := newVolumeMoveFixture(t)
	fromName := "node-1"
	if f.from == "node-two" {
		fromName = "node-2"
	}
	cases := map[string]struct {
		node  string
		setup func()
		code  connect.Code
		want  string
	}{
		"same node": {node: fromName, code: connect.CodeFailedPrecondition, want: "already on node"},
		"unknown":   {node: "node-9", code: connect.CodeNotFound, want: "was not found"},
		"cordoned": {node: f.toNodeName, code: connect.CodeFailedPrecondition, want: "is cordoned", setup: func() {
			if _, err := f.state.UpdateNode(f.ctx, model.UpdateNode{NodeName: f.toNodeName, SetLifecycle: true, Lifecycle: model.NodeLifecycleCordoned,
				IdempotencyKey: "cordon-destination-key", RequestHash: strings.Repeat("c", 64), UpdatedAt: *f.now}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, name := range []string{"same node", "unknown", "cordoned"} {
		item := cases[name]
		if item.setup != nil {
			item.setup()
		}
		_, err := f.service.MoveVolume(f.ctx, connect.NewRequest(&orchv1alpha1.MoveVolumeRequest{Application: "shop", Volume: "data", Node: item.node}))
		if connect.CodeOf(err) != item.code || !strings.Contains(err.Error(), item.want) {
			t.Fatalf("%s: MoveVolume() = %v", name, err)
		}
	}
	var connectErr *connect.Error
	if _, err := f.service.MoveVolume(f.ctx, connect.NewRequest(&orchv1alpha1.MoveVolumeRequest{Application: "shop", Volume: "data"})); !errors.As(err, &connectErr) ||
		connectErr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("MoveVolume() without a node = %v", err)
	}
}
