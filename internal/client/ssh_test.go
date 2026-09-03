package client

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSSHTunnelUsesArgumentVectorAndRejectsOptionHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	root := t.TempDir()
	arguments := filepath.Join(root, "arguments")
	binary := filepath.Join(root, "fake-ssh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + arguments + "\nexit 1\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := startSSHTunnel(binary, "prod-controller", "127.0.0.1:9443"); err == nil {
		t.Fatal("failed SSH command unexpectedly became ready")
	}
	contents, err := os.ReadFile(arguments)
	if err != nil {
		t.Fatal(err)
	}
	value := string(contents)
	for _, expected := range []string{"-N\n", "-T\n", "BatchMode=yes\n", "ExitOnForwardFailure=yes\n", "127.0.0.1:9443\n", "prod-controller\n"} {
		if !strings.Contains(value, expected) {
			t.Fatalf("SSH argv %q lacks %q", value, expected)
		}
	}
	if _, err := startSSHTunnel(binary, "-oProxyCommand=bad", "127.0.0.1:9443"); err == nil {
		t.Fatal("option-shaped SSH host was accepted")
	}
}

func TestSSHTunnelReadinessAndCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_TEST_BINARY", executable)
	t.Setenv("ORCH_SSH_HELPER", "1")
	binary := filepath.Join(t.TempDir(), "fake-ssh")
	script := "#!/bin/sh\nexec \"$ORCH_TEST_BINARY\" -test.run=TestSSHHelperProcess -- \"$@\"\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	tunnel, err := startSSHTunnel(binary, "prod-controller", "127.0.0.1:9443")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tunnel.Close() })
	if host, _, splitErr := net.SplitHostPort(tunnel.Address()); splitErr != nil || host != "127.0.0.1" {
		t.Fatalf("tunnel address = %q, err=%v", tunnel.Address(), splitErr)
	}
	if err := tunnel.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tunnel.done:
	default:
		t.Fatal("SSH child remained running after close")
	}
	if err := tunnel.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
}

func TestSSHHelperProcess(t *testing.T) {
	if os.Getenv("ORCH_SSH_HELPER") != "1" {
		return
	}
	arguments := os.Args
	for index, argument := range arguments {
		if argument == "--" {
			arguments = arguments[index+1:]
			break
		}
	}
	forward := ""
	for index, argument := range arguments {
		if argument == "-L" && index+1 < len(arguments) {
			forward = arguments[index+1]
			break
		}
	}
	parts := strings.SplitN(forward, ":", 3)
	if len(parts) != 3 {
		os.Exit(2)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(parts[0], parts[1]))
	if err != nil {
		os.Exit(3)
	}
	defer listener.Close()
	for {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			os.Exit(4)
		}
		_ = connection.Close()
	}
}
