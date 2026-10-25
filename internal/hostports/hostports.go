// Package hostports finds the ports that services on the host own, so that
// workloads never take them over.
package hostports

import (
	"context"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// SSH returns the ports sshd is configured to listen on, so a workload port
// never shadows SSH, even while sshd is restarting. It returns nothing when
// sshd is missing or its configuration cannot be read.
func SSH() []uint16 {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "sshd", "-T").Output()
	if err != nil {
		return nil
	}
	return parseSSH(string(output))
}

// parseSSH reads the ports of sshd -T output: the port lines, and the
// ports of listenaddress lines such as "listenaddress 0.0.0.0:2222" or
// "listenaddress [::]:2222 rdomain blue", which sshd listens on instead.
func parseSSH(config string) []uint16 {
	var ports []uint16
	for _, line := range strings.Split(config, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value := fields[1]
		switch {
		case fields[0] == "port" && len(fields) == 2:
		case fields[0] == "listenaddress":
			_, port, err := net.SplitHostPort(value)
			if err != nil {
				continue
			}
			value = port
		default:
			continue
		}
		if port, err := strconv.ParseUint(value, 10, 16); err == nil && port != 0 && !slices.Contains(ports, uint16(port)) {
			ports = append(ports, uint16(port))
		}
	}
	return ports
}

// Listening reports whether a socket on the host listens on port: a TCP
// socket in the LISTEN state, or a UDP socket bound without a peer. A port
// that is only some connection's source port, or a socket lingering in
// TIME_WAIT, does not count.
func Listening(protocol string, port uint16) bool {
	for _, family := range []string{"", "6"} {
		table, err := os.ReadFile("/proc/net/" + protocol + family)
		if err == nil && listening(string(table), protocol, port) {
			return true
		}
	}
	return false
}

// listening reads a /proc/net/tcp or /proc/net/udp table.
func listening(table, protocol string, port uint16) bool {
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		_, local, ok := strings.Cut(fields[1], ":")
		_, remote, _ := strings.Cut(fields[2], ":")
		if !ok {
			continue
		}
		if value, err := strconv.ParseUint(local, 16, 16); err != nil || uint16(value) != port {
			continue
		}
		if protocol == "tcp" && fields[3] == "0A" || protocol == "udp" && remote == "0000" {
			return true
		}
	}
	return false
}
