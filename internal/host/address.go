package host

import (
	"errors"
	"net"
)

// DetectAdvertiseAddress returns the IPv4 address of the interface that
// carries the default route. Dialing UDP only selects a source address; no
// packet is sent.
func DetectAdvertiseAddress() (string, error) {
	connection, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return "", errors.New("could not detect this host's address; pass --advertise-address")
	}
	defer connection.Close()
	local, ok := connection.LocalAddr().(*net.UDPAddr)
	if !ok || local.IP.To4() == nil || local.IP.IsLoopback() || local.IP.IsUnspecified() {
		return "", errors.New("could not detect this host's address; pass --advertise-address")
	}
	return local.IP.To4().String(), nil
}
