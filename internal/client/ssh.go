package client

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
	// directory holds a forwarded local socket and is removed on Close.
	directory string
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
		if t.directory != "" {
			_ = os.RemoveAll(t.directory)
		}
	})
	return result
}

// startSSHSocketTunnel forwards a private local unix socket to a unix socket
// on the SSH host. The local end lives in a fresh 0700 directory so no other
// local user can reach the uncredentialed administrator socket through it.
func startSSHSocketTunnel(binary, host string, port int, remoteSocket string) (*sshTunnel, error) {
	if binary == "" {
		binary = "ssh"
	}
	if strings.HasPrefix(host, "-") || strings.ContainsAny(host, "\x00\r\n") {
		return nil, fmt.Errorf("SSH host must be a safe OpenSSH host or alias")
	}
	directory, err := os.MkdirTemp("", "orch-ssh-")
	if err != nil {
		return nil, fmt.Errorf("create SSH socket directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		_ = os.RemoveAll(directory)
		return nil, fmt.Errorf("protect SSH socket directory: %w", err)
	}
	local := filepath.Join(directory, "admin.sock")
	arguments := []string{
		"-N", "-T",
		"-o", "BatchMode=yes",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "StreamLocalBindMask=0177",
	}
	if port != 0 {
		arguments = append(arguments, "-p", strconv.Itoa(port))
	}
	arguments = append(arguments, "-L", local+":"+remoteSocket, "--", host)
	var stderr bytes.Buffer
	command := exec.Command(binary, arguments...)
	command.Stderr = &limitedWriter{buffer: &stderr, limit: 4 << 10}
	if err := command.Start(); err != nil {
		_ = os.RemoveAll(directory)
		return nil, fmt.Errorf("start SSH via %s: %w", host, err)
	}
	tunnel := &sshTunnel{address: local, command: command, done: make(chan struct{}), directory: directory}
	go func() {
		_ = command.Wait()
		close(tunnel.done)
	}()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			connection, dialErr := net.DialTimeout("unix", local, 100*time.Millisecond)
			if dialErr == nil {
				_ = connection.Close()
				return tunnel, nil
			}
		case <-tunnel.done:
			_ = tunnel.Close()
			message := strings.TrimSpace(stderr.String())
			if message == "" {
				message = "ssh exited"
			}
			return nil, fmt.Errorf("SSH to %s failed: %s", host, message)
		case <-deadline.C:
			_ = tunnel.Close()
			return nil, fmt.Errorf("SSH to %s did not become ready", host)
		}
	}
}

// limitedWriter keeps the first bytes of ssh diagnostics for error messages.
type limitedWriter struct {
	mutex  sync.Mutex
	buffer *bytes.Buffer
	limit  int
}

func (writer *limitedWriter) Write(data []byte) (int, error) {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	if remaining := writer.limit - writer.buffer.Len(); remaining > 0 {
		if len(data) > remaining {
			writer.buffer.Write(data[:remaining])
		} else {
			writer.buffer.Write(data)
		}
	}
	return len(data), nil
}
