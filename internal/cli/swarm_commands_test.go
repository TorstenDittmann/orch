package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentctl "github.com/TorstenDittmann/orch/internal/agent"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/control"
	"github.com/TorstenDittmann/orch/internal/target"
)

func TestSocketTargetStatusAndJoinTokenWithoutCertificates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dataDir := filepath.Join(root, "state")
	if _, err := clusterctl.Bootstrap(ctx, clusterctl.BootstrapOptions{Name: "test", Server: "https://" + address, DataDir: dataDir,
		ContextPath: filepath.Join(root, "admin", "context.json"), Now: time.Now, Random: rand.Reader}); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "admin.sock")
	server, err := control.Start(ctx, control.Options{DataDir: dataDir, Listen: address, AdminSocket: socket})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	info, err := os.Lstat(socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("admin socket = %v, %v", info, err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	t.Setenv("ORCH_TARGET", "")

	run := func(args ...string) (string, string, int) {
		var stdout, stderr bytes.Buffer
		code := RunContext(ctx, args, &stdout, &stderr)
		return stdout.String(), stderr.String(), code
	}
	if out, errOut, code := run("target", "add", "prod", "unix://"+socket); code != ExitOK || !strings.Contains(out, "Using target prod") {
		t.Fatalf("target add = %d, %q, %q", code, out, errOut)
	}
	out, errOut, code := run("status")
	if code != ExitOK || !strings.Contains(out, "Cluster:    test") || strings.Contains(out, "Admin cert") {
		t.Fatalf("status = %d, %q, %q", code, out, errOut)
	}

	out, errOut, code = run("join-token", "worker-1", "--json")
	if code != ExitOK {
		t.Fatalf("join-token = %d, %q", code, errOut)
	}
	var created struct {
		NodeName string `json:"node_name"`
		Token    string `json:"token"`
		Command  string `json:"command"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil || created.NodeName != "worker-1" || created.Command != "orch join "+created.Token {
		t.Fatalf("join-token output = %q, %v", out, err)
	}
	artifact, err := agentctl.ResolveJoinToken(ctx, created.Token, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dataDir, "pki", "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if artifact.NodeName != "worker-1" || artifact.ControllerEndpoint != "https://"+address || !bytes.Equal(bytes.TrimSpace(artifact.CACertificatePEM), bytes.TrimSpace(caPEM)) {
		t.Fatalf("resolved artifact = %+v", artifact)
	}
	if _, err := agentctl.ResolveJoinToken(ctx, tamperFingerprint(t, created.Token), time.Now()); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("tampered fingerprint error = %v", err)
	}
	encoded, err := agentctl.EncodeJoinArtifact(artifact)
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := agentctl.EnrollWithJoin(ctx, agentctl.JoinEnrollOptions{DataDir: filepath.Join(root, "agent"), JoinFile: "-", JoinInput: bytes.NewReader(encoded)})
	if err != nil || enrolled.Config.NodeName != "worker-1" {
		t.Fatalf("enroll with resolved join token = %+v, %v", enrolled, err)
	}
	if out, errOut, code := run("nodes"); code != ExitOK || !strings.Contains(out, "worker-1") {
		t.Fatalf("nodes over socket = %d, %q, %q", code, out, errOut)
	}

	output := filepath.Join(root, "worker-2.join")
	if _, errOut, code := run("node", "join", "create", "worker-2", "--output", output); code != ExitOK {
		t.Fatalf("node join create over socket = %d, %q", code, errOut)
	}

	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatalf("admin socket left behind: %v", err)
	}
}

func tamperFingerprint(t *testing.T, token string) string {
	t.Helper()
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, agentctl.JoinTokenPrefix))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	fields["f"] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	payload, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return agentctl.JoinTokenPrefix + base64.RawURLEncoding.EncodeToString(payload)
}

func TestParseSocketLocation(t *testing.T) {
	for _, test := range []struct {
		location string
		want     target.Transport
	}{
		{"ssh://root@203.0.113.10", target.Transport{Type: target.TransportSocket, SSHHost: "root@203.0.113.10", Socket: target.DefaultAdminSocket}},
		{"ssh://prod", target.Transport{Type: target.TransportSocket, SSHHost: "prod", Socket: target.DefaultAdminSocket}},
		{"ssh://admin@[2001:db8::1]:2222/run/orch.sock", target.Transport{Type: target.TransportSocket, SSHHost: "admin@2001:db8::1", SSHPort: 2222, Socket: "/run/orch.sock"}},
		{"unix:///var/run/orch.sock", target.Transport{Type: target.TransportSocket, Socket: "/var/run/orch.sock"}},
	} {
		got, err := parseSocketLocation(test.location)
		if err != nil || got != test.want {
			t.Fatalf("parseSocketLocation(%q) = %+v, %v; want %+v", test.location, got, err, test.want)
		}
	}
	for _, location := range []string{"root@host", "http://host", "ssh://", "ssh://root:secret@host", "ssh://host:0", "ssh://-oProxyCommand=x", "unix://host/path", "ssh://host/run/a:b.sock"} {
		if _, err := parseSocketLocation(location); err == nil {
			t.Fatalf("parseSocketLocation(%q) unexpectedly succeeded", location)
		}
	}
}
