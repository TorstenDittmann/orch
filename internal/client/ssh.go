package client

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type sshTunnel struct {
	address string
	command *exec.Cmd
	done    chan struct{}
	close   sync.Once
}

func startSSHTunnel(binary, host, destination string) (*sshTunnel, error) {
	if binary == "" {
		binary = "ssh"
	}
	if strings.HasPrefix(host, "-") || strings.ContainsAny(host, "\x00\r\n") {
		return nil, fmt.Errorf("SSH host must be a safe OpenSSH host or alias")
	}
	for attempt := 0; attempt < 3; attempt++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("reserve SSH tunnel address: %w", err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
		address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		forward := address + ":" + destination
		command := exec.Command(binary,
			"-N", "-T",
			"-o", "BatchMode=yes",
			"-o", "ExitOnForwardFailure=yes",
			"-L", forward,
			host,
		)
		if err := command.Start(); err != nil {
			return nil, fmt.Errorf("start SSH tunnel via %s: %w", host, err)
		}
		tunnel := &sshTunnel{address: address, command: command, done: make(chan struct{})}
		go func() {
			_ = command.Wait()
			close(tunnel.done)
		}()
		deadline := time.NewTimer(5 * time.Second)
		ticker := time.NewTicker(25 * time.Millisecond)
		ready := false
		failed := false
		for !ready && !failed {
			select {
			case <-ticker.C:
				connection, dialErr := net.DialTimeout("tcp", address, 100*time.Millisecond)
				if dialErr == nil {
					_ = connection.Close()
					ready = true
				}
			case <-tunnel.done:
				failed = true
			case <-deadline.C:
				failed = true
			}
		}
		ticker.Stop()
		if !deadline.Stop() {
			select {
			case <-deadline.C:
			default:
			}
		}
		if ready {
			return tunnel, nil
		}
		_ = tunnel.Close()
		if attempt == 2 {
			return nil, fmt.Errorf("SSH tunnel via %s did not become ready", host)
		}
	}
	return nil, fmt.Errorf("SSH tunnel via %s did not become ready", host)
}

func (t *sshTunnel) Address() string { return t.address }

func (t *sshTunnel) Close() error {
	if t == nil {
		return nil
	}
	var result error
	t.close.Do(func() {
		if t.command.Process != nil {
			if err := t.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				result = fmt.Errorf("stop SSH tunnel: %w", err)
			}
		}
		select {
		case <-t.done:
		case <-time.After(time.Second):
			if result == nil {
				result = fmt.Errorf("wait for SSH tunnel shutdown")
			}
		}
	})
	return result
}
