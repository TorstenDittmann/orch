package tcpingress

import (
	"context"
	"io"
	"net"
	"strconv"
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
