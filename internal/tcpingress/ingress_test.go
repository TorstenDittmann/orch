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

	"change.codes/torsten/better-swarm/internal/model"
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
