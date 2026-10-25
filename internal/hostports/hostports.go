// Package hostports finds the ports that services on the host own, so that
// workloads never take them over.
package hostports

import (
	"context"
	"os/exec"
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

func parseSSH(config string) []uint16 {
	var ports []uint16
	for _, line := range strings.Split(config, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "port" {
			continue
		}
		if port, err := strconv.ParseUint(fields[1], 10, 16); err == nil && port != 0 {
			ports = append(ports, uint16(port))
		}
	}
	return ports
}
