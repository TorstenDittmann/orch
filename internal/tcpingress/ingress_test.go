package tcpingress

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

func TestControllerProxiesBytesAndRemovesListener(t *testing.T) {
	upstream := echoListener(t)
	defer upstream.Close()
	upstreamPort := uint32(upstream.Addr().(*net.TCPAddr).Port)
	publicPort := freePort(t)
	controller := New(10 * time.Millisecond)
	defer controller.Close()
	statuses := controller.Apply(context.Background(), []Route{{
		Application: "app", Service: "gitd", PublicPort: publicPort, TargetPort: upstreamPort, Upstreams: []string{"127.0.0.1"},
	}})
	if len(statuses) != 1 || statuses[0].State != model.TCPRouteActive {
		t.Fatalf("statuses = %#v", statuses)
	}
	connection, err := net.Dial("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(publicPort))))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("git protocol")); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, len("git protocol"))
	if _, err := io.ReadFull(connection, buffer); err != nil || string(buffer) != "git protocol" {
		t.Fatalf("echo = %q, %v", buffer, err)
	}
	controller.Apply(context.Background(), nil)
}

func TestServeRetriesTemporaryAcceptErrors(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	fake := &fakeListener{
		errs:   []error{&net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}, &net.OpError{Op: "accept", Net: "tcp", Err: syscall.ECONNABORTED}},
		conns:  make(chan net.Conn, 1),
		closed: make(chan struct{}),
	}
	fake.conns <- server
	managed := &listener{listener: fake, active: make(map[net.Conn]struct{})}
	done := make(chan struct{})
	go func() { managed.serve(); close(done) }()
	// The route has no upstreams, so the proxied connection is closed at once.
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("read after temporary accept errors = %v, want EOF from proxied connection", err)
	}
	select {
	case <-done:
		t.Fatal("serve exited before the listener was closed")
	default:
	}
	managed.drain(0)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not exit after the listener was closed")
	}
}

func TestDrainClosesConnectionsTrackedAfterSweep(t *testing.T) {
	fake := &fakeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
	managed := &listener{listener: fake, active: make(map[net.Conn]struct{})}
	managed.drain(0)
	deadline := time.Now().Add(5 * time.Second)
	for {
		managed.mu.Lock()
		drained := managed.drained
		managed.mu.Unlock()
		if drained {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("drain sweep did not run")
		}
		time.Sleep(time.Millisecond)
	}
	late, peer := net.Pipe()
	defer peer.Close()
	managed.track(late, true)
	_ = peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("read from late-tracked connection = %v, want EOF", err)
	}
	managed.mu.Lock()
	defer managed.mu.Unlock()
	if len(managed.active) != 0 {
		t.Fatalf("active = %d, want 0", len(managed.active))
	}
}

// fakeListener returns each queued error from Accept before serving conns.
type fakeListener struct {
	mu        sync.Mutex
	errs      []error
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

func (f *fakeListener) Accept() (net.Conn, error) {
	f.mu.Lock()
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		f.mu.Unlock()
		return nil, err
	}
	f.mu.Unlock()
	select {
	case connection := <-f.conns:
		return connection, nil
	case <-f.closed:
		return nil, net.ErrClosed
	}
}

func (f *fakeListener) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4zero} }

func echoListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer connection.Close(); _, _ = io.Copy(connection, connection) }()
		}
	}()
	return listener
}

func freePort(t *testing.T) uint32 {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint32(listener.Addr().(*net.TCPAddr).Port)
	_ = listener.Close()
	return port
}

func TestListenerRejectsConnectionsBeyondLimit(t *testing.T) {
	upstream := echoListener(t)
	defer upstream.Close()
	managed, address := startTestListener(t, upstream, 1, time.Minute)
	defer managed.drain(0)
	first := dialEcho(t, address)
	defer first.Close()
	rejected, err := net.Dial("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	defer rejected.Close()
	_ = rejected.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := rejected.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("read beyond the connection limit = %v, want closed connection", err)
	}
	_ = first.Close()
	waitForOpen(t, managed, 0)
	dialEcho(t, address).Close()
}

func TestProxyClosesIdleConnections(t *testing.T) {
	upstream := echoListener(t)
	defer upstream.Close()
	managed, address := startTestListener(t, upstream, 0, 300*time.Millisecond)
	defer managed.drain(0)
	connection := dialEcho(t, address)
	defer connection.Close()
	// Traffic keeps the connection open past the idle timeout.
	for range 5 {
		time.Sleep(100 * time.Millisecond)
		if _, err := connection.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(connection, make([]byte, 1)); err != nil {
			t.Fatalf("active connection was closed: %v", err)
		}
	}
	started := time.Now()
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := connection.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("read from idle connection = %v, want EOF", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("idle connection closed after %s", elapsed)
	}
	waitForOpen(t, managed, 0)
}

func TestProxyBoundsWaitAfterHalfClose(t *testing.T) {
	// The upstream reads everything but never answers or closes.
	upstream, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		for {
			connection, err := upstream.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, connection) }()
		}
	}()
	managed, address := startTestListener(t, upstream, 0, 300*time.Millisecond)
	defer managed.drain(0)
	connection, err := net.Dial("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := connection.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := connection.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("read after half-close = %v, want EOF once the idle timeout ends the wait", err)
	}
	waitForOpen(t, managed, 0)
}

func startTestListener(t *testing.T, upstream net.Listener, limit int64, idle time.Duration) (*listener, string) {
	t.Helper()
	public, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	managed := &listener{listener: public, active: make(map[net.Conn]struct{}), limit: limit, idle: idle}
	managed.route.Store(&Route{TargetPort: uint32(upstream.Addr().(*net.TCPAddr).Port), Upstreams: []string{"127.0.0.1"}})
	go managed.serve()
	return managed, public.Addr().String()
}

func dialEcho(t *testing.T, address string) net.Conn {
	t.Helper()
	connection, err := net.Dial("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := connection.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(connection, make([]byte, 4)); err != nil {
		t.Fatalf("echo through proxy: %v", err)
	}
	_ = connection.SetDeadline(time.Time{})
	return connection
}

func waitForOpen(t *testing.T, managed *listener, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for managed.open.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("open connections = %d, want %d", managed.open.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}
