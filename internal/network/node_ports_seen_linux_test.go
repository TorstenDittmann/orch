package network

import (
	"net"
	"syscall"
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

func TestNodePortBusyWithoutAListenerIsRetried(t *testing.T) {
	// A socket bound without listening, like an outgoing connection's source
	// port, makes the check fail with EADDRINUSE but is not a host service.
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
	port := NodePort{Protocol: "tcp", Port: uint16(address.(*syscall.SockaddrInet4).Port), TargetPort: 80}
	f := &firewall{hostOwned: map[string]bool{}}
	if results, _ := f.publish([]NodePort{port}); results[0] == nil {
		t.Fatal("a port in use was accepted")
	}
	_ = syscall.Close(fd)
	if results, _ := f.publish([]NodePort{port}); results[0] != nil {
		t.Fatalf("port was not retried once it was free: %v", results[0])
	}
}
