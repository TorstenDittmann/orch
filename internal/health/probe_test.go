package health

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

func (f dialContextFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

func TestTrackerHysteresis(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tracker := Tracker{Status: model.HealthChecking}
	tracker, _ = tracker.Observe(true, now)
	if tracker.Status != model.HealthChecking {
		t.Fatalf("one success status = %q", tracker.Status)
	}
	tracker, changed := tracker.Observe(true, now.Add(time.Second))
	if !changed || tracker.Status != model.HealthReady || tracker.ReadyAt == nil {
		t.Fatalf("ready tracker = %#v, changed=%v", tracker, changed)
	}
	for index := 0; index < 2; index++ {
		tracker, _ = tracker.Observe(false, now.Add(time.Duration(index+2)*time.Second))
	}
	if tracker.Status != model.HealthReady {
		t.Fatalf("two failures status = %q", tracker.Status)
	}
	tracker, changed = tracker.Observe(false, now.Add(4*time.Second))
	if !changed || tracker.Status != model.HealthUnhealthy || tracker.ReadyAt != nil {
		t.Fatalf("unhealthy tracker = %#v, changed=%v", tracker, changed)
	}
}

func TestTrackerStartupFailuresStayCheckingUntilReady(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	tracker := Tracker{}
	for range UnhealthyFailures + 3 {
		tracker, _ = tracker.Observe(false, now)
		if tracker.Status != model.HealthChecking || tracker.ReadyAt != nil {
			t.Fatalf("startup probe failures bypassed the caller's health deadline: %#v", tracker)
		}
	}
	for range ReadySuccesses {
		tracker, _ = tracker.Observe(true, now)
	}
	if tracker.Status != model.HealthReady {
		t.Fatalf("startup could not recover: %#v", tracker)
	}
	for range UnhealthyFailures {
		tracker, _ = tracker.Observe(false, now)
	}
	if tracker.Status != model.HealthUnhealthy {
		t.Fatalf("startup grace hid a failure after readiness passed: %#v", tracker)
	}
}

func TestNetworkProberHTTPDoesNotFollowRedirects(t *testing.T) {
	redirected := false
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirected" {
			redirected = true
		}
		if request.Host != "web.example.svc.internal" {
			t.Errorf("Host = %q", request.Host)
		}
		http.Redirect(response, request, "/redirected", http.StatusFound)
	}))
	defer server.Close()
	address := server.Listener.Addr().(*net.TCPAddr)
	prober := NewNetworkProber()
	prober.dialer = dialContextFunc(func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	})
	err := prober.Probe(context.Background(), Target{
		Address: netip.MustParseAddr("10.88.0.2"), Port: uint16(address.Port),
		Path: "/health", Host: "web.example.svc.internal",
	})
	if err != nil {
		t.Fatal(err)
	}
	if redirected {
		t.Fatal("health probe followed a redirect")
	}
}

func TestNetworkProberRejectsUnhealthyStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	address := server.Listener.Addr().(*net.TCPAddr)
	prober := NewNetworkProber()
	prober.dialer = dialContextFunc(func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	})
	err := prober.Probe(context.Background(), Target{
		Address: netip.MustParseAddr("10.88.0.2"), Port: uint16(address.Port),
		Path: "/health", Host: "web.example.svc.internal",
	})
	if err == nil {
		t.Fatal("unhealthy HTTP status succeeded")
	}
}

func TestNetworkProberTCPConnectsToThePrivateTarget(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			_ = connection.Close()
			close(accepted)
		}
	}()
	prober := NewNetworkProber()
	prober.dialer = dialContextFunc(func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, listener.Addr().String())
	})
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	if err := prober.Probe(context.Background(), Target{Address: netip.MustParseAddr("10.88.0.9"), Port: port}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("TCP probe did not connect")
	}
}

func TestNetworkProberEnforcesAttemptTimeout(t *testing.T) {
	prober := NewNetworkProber()
	prober.dialer = dialContextFunc(func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	started := time.Now()
	err := prober.Probe(context.Background(), Target{Address: netip.MustParseAddr("10.88.0.9"), Port: 8080})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed < ProbeTimeout || elapsed > ProbeTimeout+500*time.Millisecond {
		t.Fatalf("probe elapsed = %s", elapsed)
	}
}

func TestNetworkProberRejectsUnassignedTargets(t *testing.T) {
	prober := NewNetworkProber()
	for _, target := range []Target{
		{Address: netip.MustParseAddr("203.0.113.9"), Port: 8080},
		{Address: netip.MustParseAddr("10.88.0.9")},
		{Address: netip.MustParseAddr("10.88.0.9"), Port: 8080, Path: "/health", Host: "public.example.com"},
		{Address: netip.MustParseAddr("10.88.0.9"), Port: 8080, Path: "health", Host: "web.example.svc.internal."},
	} {
		if err := prober.Probe(context.Background(), target); err == nil {
			t.Fatalf("Probe(%#v) succeeded", target)
		}
	}
}
