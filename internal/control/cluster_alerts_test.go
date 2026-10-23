package control

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/security"
)

func TestClusterAlertsNotifyOncePerProblem(t *testing.T) {
	state, cluster := reconcilerStore(t)
	enrollCompatibleNode(t, state)
	box, err := security.NewSecretBox(make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), principalKey{}, authenticatedPrincipal{certificate: &x509.Certificate{}})
	var mu sync.Mutex
	var received []string
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Text string }
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		received = append(received, body.Text)
	}))
	defer endpoint.Close()
	texts := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(received)
	}

	now := reconcilerTestTime.Add(time.Hour)
	s := newService(state, cluster, nil, nil, box, nil, func() time.Time { return now })
	s.backups = &backupScheduler{latest: clusterctl.BackupSet{CreatedAt: now.Add(-72 * time.Hour)}}
	n := newNotifier(state, box, "prod")
	if err := n.deliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetNotification(ctx, connect.NewRequest(&orchv1alpha1.SetNotificationRequest{Kind: "slack", Url: "ftp://example.com"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("non-http URL error = %v", err)
	}
	if _, err := s.SetNotification(ctx, connect.NewRequest(&orchv1alpha1.SetNotificationRequest{Kind: "slack", Url: endpoint.URL, Events: []string{"node.gone"}})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("unknown event error = %v", err)
	}
	set, err := s.SetNotification(ctx, connect.NewRequest(&orchv1alpha1.SetNotificationRequest{Kind: "slack", Url: endpoint.URL}))
	if err != nil {
		t.Fatal(err)
	}
	if got := set.Msg.GetNotification(); got.GetName() != "slack" || !slices.Equal(got.GetEvents(), config.ClusterNotificationEvents) {
		t.Fatalf("notification = %+v", got)
	}

	if err := state.RecordNodeUsage(ctx, "node-one", model.NodeUsage{ObservedAt: now.Add(-time.Minute), DiskUsedBytes: 95e9, DiskTotalBytes: 100e9}); err != nil {
		t.Fatal(err)
	}
	alerts := newClusterAlerts(s, now.Add(10*24*time.Hour))
	alerts.startedAt = reconcilerTestTime
	for range 2 {
		if err := alerts.check(ctx); err != nil {
			t.Fatal(err)
		}
		if err := n.deliverPending(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got := texts()
	slices.Sort(got)
	want := []string{
		"[prod] controller certificate expires 2026-09-12T13:00:00Z; run orch cluster certificates renew on the controller host",
		"[prod] node node-1 disk is 95% full (95.0 GB of 100.0 GB used)",
		"[prod] node node-1 is unreachable; last heartbeat 2026-09-02T12:01:00Z",
		"[prod] the newest controller backup is from 2026-08-30T13:00:00Z, more than 2 days ago",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("notifications =\n%s", strings.Join(got, "\n"))
	}

	// A disk that drops below the clear line and fills up again is
	// reported again; one that stays between the lines is not.
	for _, test := range []struct {
		used uint64
		sent int
	}{{88e9, 4}, {80e9, 4}, {92e9, 5}} {
		now = now.Add(time.Minute)
		if err := state.RecordNodeUsage(ctx, "node-one", model.NodeUsage{ObservedAt: now, DiskUsedBytes: test.used, DiskTotalBytes: 100e9}); err != nil {
			t.Fatal(err)
		}
		if err := alerts.check(ctx); err != nil {
			t.Fatal(err)
		}
		if err := n.deliverPending(ctx); err != nil {
			t.Fatal(err)
		}
		if sent := len(texts()); sent != test.sent {
			t.Fatalf("after %d%% used, %d notifications were sent, want %d", test.used/1e9, sent, test.sent)
		}
	}

	tested, err := s.TestNotification(ctx, connect.NewRequest(&orchv1alpha1.TestNotificationRequest{}))
	if err != nil || len(tested.Msg.GetResults()) != 1 || tested.Msg.GetResults()[0].GetError() != "" {
		t.Fatalf("test = %+v, %v", tested, err)
	}
	if last := texts()[len(texts())-1]; last != "[test] test notification from orch; slack is set up" {
		t.Fatalf("test message = %q", last)
	}
	deleted, err := s.DeleteNotification(ctx, connect.NewRequest(&orchv1alpha1.DeleteNotificationRequest{Name: "slack"}))
	if err != nil || !deleted.Msg.GetChanged() {
		t.Fatalf("delete = %+v, %v", deleted, err)
	}
}

func TestClusterAlertsKeepLostNodeAcrossControllerRestart(t *testing.T) {
	state, cluster := reconcilerStore(t)
	enrollCompatibleNode(t, state)
	ctx := context.Background()
	now := reconcilerTestTime.Add(time.Hour)
	s := newService(state, cluster, nil, nil, nil, nil, func() time.Time { return now })
	if _, err := state.RaiseClusterAlert(ctx, "node.lost/node-one", config.NotifyNodeLost, model.ClusterAlert{Message: "lost"}, now); err != nil {
		t.Fatal(err)
	}
	// A restarted controller does not clear the alert during its startup
	// grace, so it does not send it again once the grace ends.
	alerts := newClusterAlerts(s, time.Time{})
	for range 2 {
		if err := alerts.check(ctx); err != nil {
			t.Fatal(err)
		}
		if raised, err := state.ClusterAlerts(ctx); err != nil || !slices.Contains(raised, "node.lost/node-one") {
			t.Fatalf("raised = %v, %v", raised, err)
		}
		now = now.Add(nodeLostAfter)
	}
	events, err := state.EventsAfter(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	lost := 0
	for _, event := range events {
		if event.Kind == config.NotifyNodeLost {
			lost++
		}
	}
	if lost != 1 {
		t.Fatalf("node.lost events = %d, want 1", lost)
	}
}

func TestClusterAlertsIgnoreDowntimeBeforeControllerStart(t *testing.T) {
	state, cluster := reconcilerStore(t)
	enrollCompatibleNode(t, state)
	now := reconcilerTestTime.Add(time.Hour)
	s := newService(state, cluster, nil, nil, nil, nil, func() time.Time { return now })
	alerts := newClusterAlerts(s, time.Time{})
	if alerts.lost(mustNode(t, s), now) {
		t.Fatal("a node was lost right after the controller started")
	}
	now = now.Add(nodeLostAfter)
	if !alerts.lost(mustNode(t, s), now) {
		t.Fatal("a node without heartbeats was not lost")
	}
}

func TestAdministratorCertificatesTrackLongLivedOnes(t *testing.T) {
	now := reconcilerTestTime
	var tracker administratorCertificates
	tracker.record(&x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now, NotAfter: now.Add(15 * time.Minute)}, now)
	tracker.record(&x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: now, NotAfter: now.AddDate(1, 0, 0)}, now)
	if got := tracker.recent(now); len(got) != 1 || !got[0].Equal(now.AddDate(1, 0, 0)) {
		t.Fatalf("recent = %v", got)
	}
	if got := tracker.recent(now.Add(administratorSeenWithin + time.Minute)); len(got) != 0 {
		t.Fatalf("recent after a week unused = %v", got)
	}
}

func mustNode(t *testing.T, s *service) model.Node {
	t.Helper()
	node, err := s.state.NodeByID(context.Background(), "node-one")
	if err != nil {
		t.Fatal(err)
	}
	return node
}
