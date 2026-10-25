package tcpingress

import (
	"context"
	"net"
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
