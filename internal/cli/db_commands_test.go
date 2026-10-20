package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentctl "github.com/TorstenDittmann/orch/internal/agent"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/control"
	"github.com/TorstenDittmann/orch/internal/discovery"
	"github.com/TorstenDittmann/orch/internal/ingress"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

func TestDatabaseCLIThroughControllerAndAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback controller/agent")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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
	dir, path := filepath.Join(root, "controller"), filepath.Join(root, "context.json")
	if _, err := clusterctl.Bootstrap(ctx, clusterctl.BootstrapOptions{Name: "test", Server: "https://" + address, DataDir: dir, ContextPath: path, Now: time.Now, Random: rand.Reader}); err != nil {
		t.Fatal(err)
	}
	server, err := control.Start(ctx, control.Options{DataDir: dir, Listen: address, BackupKeep: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown(context.Background())
	rewriteCLIContextServer(t, path, "https://"+server.Addr().String())
	agentDir := filepath.Join(root, "agent")
	if _, err := agentctl.Enroll(ctx, agentctl.EnrollOptions{Name: "node-one", ContextPath: path, DataDir: agentDir, Now: time.Now, Random: rand.Reader}); err != nil {
		t.Fatal(err)
	}
	runtime := orchruntime.NewFake()
	runtime.Capabilities.TotalCPUMillis = 4000
	runtime.Capabilities.TotalMemoryBytes = 8 << 30
	agentCtx, stopAgent := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- agentctl.Serve(agentCtx, agentctl.ServeOptions{DataDir: agentDir, Snapshotter: "overlayfs", Runtime: runtime, Discovery: discovery.NewFake(), Ingress: &ingress.Fake{}, Now: time.Now, Random: rand.Reader})
	}()
	defer func() {
		stopAgent()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("agent did not stop")
		}
	}()
	remote, err := controlclient.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.CloseIdleConnections()
	invoke := func(want int, args ...string) string {
		t.Helper()
		var out, errOut bytes.Buffer
		if code := RunContext(ctx, append(args, "--context", path), &out, &errOut); code != want {
			t.Fatalf("%v: code=%d want=%d out=%s err=%s", args, code, want, out.String(), errOut.String())
		}
		return out.String() + errOut.String()
	}
	wait := func(what string, test func() bool) {
		t.Helper()
		for !test() {
			select {
			case <-ctx.Done():
				t.Fatal("timed out waiting for " + what)
			case <-time.After(30 * time.Millisecond):
			}
		}
	}
	// Every command run in the database succeeds, so dumps, readiness checks
	// and loads all pass in the fake runtime.
	running := func() string {
		for id, object := range runtime.SnapshotObjects() {
			if object.Running {
				runtime.SetExecFrames(id, "sh", []orchruntime.ExecFrame{{Exited: true}})
				return id
			}
		}
		return ""
	}
	dumpDir := filepath.Join(agentDir, agentctl.VolumesDirectory, "shop", "main-backup")
	if err := os.MkdirAll(dumpDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dumpDir, "dump.pgc"), []byte("monday"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := writeManifest(t, `amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"
name = "shop"
databases {
  ["main"] = new Postgres { image = "postgres@sha256:`+strings.Repeat("b", 64)+`"; backup = new DatabaseBackup { keep = 3 } }
}
`)
	invoke(ExitOK, "deploy", "-f", file)
	// The fake runtime cannot answer TCP readiness probes, so the database
	// stays starting; a running replica is all these commands need.
	wait("the database", func() bool { return running() != "" })
	if output := invoke(ExitOK, "secret", "list", "shop"); !strings.Contains(output, "main-password") || !strings.Contains(output, "main-url") {
		t.Fatalf("secret list: %s", output)
	}
	var databases []dbOutput
	if err := json.Unmarshal([]byte(invoke(ExitOK, "db", "list", "--json")), &databases); err != nil || len(databases) != 1 ||
		databases[0].Engine != "postgres" || databases[0].BackupVolume != "main-backup" || databases[0].BackupKeep != 3 {
		t.Fatalf("db list: %+v %v", databases, err)
	}
	if output := invoke(ExitOK, "db", "list"); !strings.Contains(output, "main-url") {
		t.Fatalf("db list: %s", output)
	}
	if output := invoke(ExitOK, "db", "backup", "shop", "main"); !strings.Contains(output, "Backed up shop/main-backup") {
		t.Fatalf("db backup: %s", output)
	}
	if err := os.WriteFile(filepath.Join(dumpDir, "dump.pgc"), []byte("tuesday"), 0o600); err != nil {
		t.Fatal(err)
	}
	invoke(ExitRuntime, "db", "backup", "shop", "missing")

	restoring, finish := context.WithCancel(ctx)
	defer finish()
	go func() {
		for restoring.Err() == nil {
			running()
			time.Sleep(20 * time.Millisecond)
		}
	}()
	if output := invoke(ExitOK, "db", "restore", "shop", "main"); !strings.Contains(output, "Restored database shop/main") || !strings.Contains(output, "Loading the dump") {
		t.Fatalf("db restore: %s", output)
	}
	finish()
	if contents, _ := os.ReadFile(filepath.Join(dumpDir, "dump.pgc")); string(contents) != "monday" {
		t.Fatalf("restored dump = %q", contents)
	}
}
