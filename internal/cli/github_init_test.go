package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/control"
)

func TestGitHubRepositoryFromRemote(t *testing.T) {
	for remote, want := range map[string]string{
		"git@github.com:Acme/Hello.git":             "acme/hello",
		"https://github.com/acme/hello":             "acme/hello",
		"https://token@github.com/acme/hello.git":   "acme/hello",
		"ssh://git@github.com/acme/hello.js.git":    "acme/hello.js",
		"https://gitlab.com/acme/hello.git":         "",
		"https://github.com/acme/hello/extra":       "",
		"git@github.com:acme/hello.git\n":           "acme/hello",
		"https://github.com.evil.example/acme/repo": "",
	} {
		if got := githubRepositoryFromRemote(remote); got != want {
			t.Errorf("githubRepositoryFromRemote(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestGitHubActionRefFollowsReleases(t *testing.T) {
	for version, want := range map[string]string{"0.3.0": "v0.3.0", "v0.3.0": "v0.3.0", "0.1.0-dev": "main", "": "main"} {
		if got := githubActionRef(version); got != want {
			t.Errorf("githubActionRef(%q) = %q, want %q", version, got, want)
		}
	}
}

func TestGitHubWorkflowBuildsOnlyWithAnImage(t *testing.T) {
	built := githubWorkflow{Application: "hello", Branch: "main", Cluster: "https://c#sha256:ab", Image: "ghcr.io/acme/hello", Context: "apps/hello", File: "apps/hello/orch.pkl"}.render()
	for _, want := range []string{"packages: write", "image: ghcr.io/acme/hello", "context: apps/hello", "file: apps/hello/orch.pkl", "group: deploy-hello"} {
		if !strings.Contains(built, want) {
			t.Errorf("workflow lacks %q:\n%s", want, built)
		}
	}
	plain := githubWorkflow{Application: "hello", Branch: "main", Cluster: "https://c#sha256:ab", Context: ".", File: "orch.pkl"}.render()
	for _, unwanted := range []string{"packages:", "image:", "context:", "file:", "environment:"} {
		if strings.Contains(plain, unwanted) {
			t.Errorf("workflow has %q:\n%s", unwanted, plain)
		}
	}
}

func TestGitHubWorkflowMatchesNodeArchitectures(t *testing.T) {
	for _, tc := range []struct {
		architectures     []string
		runsOn, platforms string
	}{
		{nil, "", ""},
		{[]string{"amd64", ""}, "", ""},
		{[]string{"arm64", "arm64"}, "ubuntu-24.04-arm", ""},
		{[]string{"amd64", "arm64"}, "", "linux/amd64,linux/arm64"},
	} {
		runsOn, platforms := buildTarget(tc.architectures)
		if runsOn != tc.runsOn || platforms != tc.platforms {
			t.Errorf("buildTarget(%v) = %q, %q; want %q, %q", tc.architectures, runsOn, platforms, tc.runsOn, tc.platforms)
		}
	}
	arm := githubWorkflow{Application: "hello", Branch: "main", Cluster: "https://c#sha256:ab", Image: "ghcr.io/acme/hello", RunsOn: "ubuntu-24.04-arm"}.render()
	if !strings.Contains(arm, "runs-on: ubuntu-24.04-arm") || strings.Contains(arm, "platforms:") {
		t.Errorf("arm64 workflow:\n%s", arm)
	}
	mixed := githubWorkflow{Application: "hello", Branch: "main", Cluster: "https://c#sha256:ab", Image: "ghcr.io/acme/hello", Platforms: "linux/amd64,linux/arm64"}.render()
	if !strings.Contains(mixed, "runs-on: ubuntu-latest") || !strings.Contains(mixed, "platforms: linux/amd64,linux/arm64") {
		t.Errorf("mixed workflow:\n%s", mixed)
	}
}

func TestGitHubInitTrustsTheRepositoryAndWritesTheWorkflow(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	dir, contextPath := filepath.Join(root, "state"), filepath.Join(root, "context.json")
	if _, err := clusterctl.Bootstrap(context.Background(), clusterctl.BootstrapOptions{Name: "test", Server: "https://127.0.0.1", DataDir: dir, ContextPath: contextPath, Now: time.Now, Random: rand.Reader}); err != nil {
		t.Fatal(err)
	}
	server, err := control.Start(context.Background(), control.Options{DataDir: dir, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	rewriteCLIContextServer(t, contextPath, "https://"+server.Addr().String())

	checkout := filepath.Join(root, "hello")
	app := filepath.Join(checkout, "deploy")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"orch.pkl": cliManifest, "Dockerfile": "FROM scratch\n"} {
		if err := os.WriteFile(filepath.Join(app, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q", "-b", "trunk"}, {"remote", "add", "origin", "git@github.com:Acme/Hello.git"}} {
		command := exec.Command("git", args...)
		command.Dir = checkout
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	t.Chdir(app)

	initialize := func(want int, args ...string) []byte {
		t.Helper()
		var out, errOut bytes.Buffer
		args = append([]string{"github", "init", "--context", contextPath, "--json"}, args...)
		if code := Run(args, &out, &errOut); code != want {
			t.Fatalf("%v code=%d want=%d out=%s err=%s", args, code, want, out.String(), errOut.String())
		}
		return out.Bytes()
	}
	var result struct {
		Repository, Application, Branch, Image, Cluster, Workflow string
	}
	if err := json.Unmarshal(initialize(ExitOK), &result); err != nil {
		t.Fatal(err)
	}
	// No origin/HEAD is known locally, so the branch falls back to main.
	if result.Repository != "acme/hello" || result.Application != "hello" || result.Branch != "main" || result.Image != "docker.io/example/web" || !strings.HasPrefix(result.Cluster, "https://") {
		t.Fatalf("init = %+v", result)
	}
	workflow, err := os.ReadFile(filepath.Join(checkout, ".github", "workflows", "deploy.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cluster: " + result.Cluster, "image: docker.io/example/web", "context: deploy", "file: deploy/orch.pkl", "branches: [main]"} {
		if !bytes.Contains(workflow, []byte(want)) {
			t.Errorf("workflow lacks %q:\n%s", want, workflow)
		}
	}
	initialize(ExitInvalid)
	initialize(ExitOK, "--force", "--branch", "release", "--image", "ghcr.io/Acme/Hello")
	workflow, _ = os.ReadFile(filepath.Join(checkout, ".github", "workflows", "deploy.yml"))
	if !bytes.Contains(workflow, []byte("image: ghcr.io/acme/hello")) || !bytes.Contains(workflow, []byte("branches: [release]")) {
		t.Fatalf("forced workflow:\n%s", workflow)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"github", "list", "--context", contextPath}, &out, &errOut); code != ExitOK || !strings.Contains(out.String(), "acme/hello  app hello  branch release") {
		t.Fatalf("list code=%d out=%s err=%s", code, out.String(), errOut.String())
	}
}
