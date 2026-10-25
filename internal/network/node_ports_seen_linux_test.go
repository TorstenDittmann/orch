package network

import (
	"net"
	"testing"
)

func TestNodePortSeenInUseStaysRefused(t *testing.T) {
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	port := NodePort{Protocol: "tcp", Port: uint16(listener.Addr().(*net.TCPAddr).Port), TargetPort: 80}
	f := &firewall{}
	// The firewall is not prepared, but ports are checked before that.
	if results, _ := f.publish([]NodePort{port}); results[0] == nil {
		t.Fatal("a port in use was accepted")
	}
	_ = listener.Close()
	// The service that held it is restarting; the port is still its own.
	if results, _ := f.publish([]NodePort{port}); results[0] == nil {
		t.Fatal("a host service's port was accepted while it was down")
	}
}
