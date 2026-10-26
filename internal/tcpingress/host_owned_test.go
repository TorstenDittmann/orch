package tcpingress

import (
	"context"
	"net"
	"syscall"
	"testing"

	"github.com/TorstenDittmann/orch/internal/model"
)

func TestRouteNeverTakesAPortAHostServiceUsed(t *testing.T) {
	previous := sshPorts
	sshPorts = func() []uint16 { return nil }
	t.Cleanup(func() { sshPorts = previous })
	held, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint32(held.Addr().(*net.TCPAddr).Port)
	controller := New(0)
	defer controller.Close()
	route := Route{Application: "shop", Service: "db", PublicPort: port, TargetPort: 5432}
	if status := controller.Apply(context.Background(), []Route{route})[0]; status.State != model.TCPRouteFailed {
		t.Fatalf("port in use: %+v", status)
	}
	// The host service restarts and briefly frees its port.
	_ = held.Close()
	if status := controller.Apply(context.Background(), []Route{route})[0]; status.State != model.TCPRouteFailed {
		t.Fatalf("host service's port while it was down: %+v", status)
	}
}

func TestRouteRetriesAPortThatWasBusyWithoutAListener(t *testing.T) {
	previous := sshPorts
	sshPorts = func() []uint16 { return nil }
	t.Cleanup(func() { sshPorts = previous })
	// A socket bound without listening, like an outgoing connection's source
	// port, makes the bind fail with EADDRINUSE but is not a host service.
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{}); err != nil {
		t.Fatal(err)
	}
	address, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	port := uint32(address.(*syscall.SockaddrInet4).Port)
	controller := New(0)
	defer controller.Close()
	route := Route{Application: "shop", Service: "db", PublicPort: port, TargetPort: 5432}
	if status := controller.Apply(context.Background(), []Route{route})[0]; status.State != model.TCPRouteFailed {
		t.Fatalf("port in use: %+v", status)
	}
	_ = syscall.Close(fd)
	if status := controller.Apply(context.Background(), []Route{route})[0]; status.State == model.TCPRouteFailed {
		t.Fatalf("port was not retried once it was free: %+v", status)
	}
}
