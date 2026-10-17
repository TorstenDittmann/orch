package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/target"
	"github.com/distribution/reference"
)

// orch dev runs a one-node cluster in a privileged container on the local
// Docker-compatible engine and deploys the manifest to it as target "dev".
const (
	devContainer    = "orch-dev"
	devVolume       = "orch-dev"
	devTarget       = "dev"
	devAPIPort      = 9443
	devPortsLabel   = "orch.dev.ports"
	devHTTPLabel    = "orch.dev.http-port"
	devSocket       = "/run/orch-containerd/containerd.sock"
	devReadyFile    = "/run/orch-dev.ready"
	devAdminDir     = "/var/lib/orch/admin"
	devEnvFile      = "orch.dev.env"
	devSecretsCache = "dev-secrets.json"
	// devImageSourceLabel marks images copied from the local engine.
	devImageSourceLabel = "orch.dev.source"
)

func runDev(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "down":
			return runDevDown(ctx, args[1:], stdout, stderr)
		case "help", "-h", "--help":
			printDevHelp(stdout)
			return ExitOK
		}
	}
	flags := flag.NewFlagSet("dev", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var file, root, envFile string
	var httpPort int
	var timeout time.Duration
	flags.StringVar(&file, "file", "orch.pkl", "Pkl manifest path")
	flags.StringVar(&file, "f", "orch.pkl", "Pkl manifest path")
	flags.StringVar(&root, "root", "", "Pkl evaluation root (defaults to the manifest directory)")
	flags.StringVar(&envFile, "env-file", "", "secret values as NAME=VALUE lines (default: orch.dev.env next to the manifest)")
	flags.IntVar(&httpPort, "http-port", 8080, "local port for HTTP services")
	flags.DurationVar(&timeout, "timeout", 10*time.Minute, "rollout wait deadline")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 || httpPort < 1 || httpPort > 65535 || timeout <= 0 {
		printDevHelp(stderr)
		return ExitInvalid
	}
	manifest, err := config.Load(ctx, file, config.LoadOptions{RootDir: root})
	if err != nil {
		writeError(stderr, err, false)
		return ExitInvalid
	}
	urls, ports := localizeDevManifest(manifest, httpPort)
	if err := config.Validate(manifest); err != nil {
		writeError(stderr, err, false)
		return ExitInvalid
	}
	engine, err := detectDevEngine(ctx)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	started, err := ensureDevNode(ctx, engine, devImage(), httpPort, ports, stderr)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	clusterID, err := installDevTarget(ctx, engine)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	contextReference, err := target.Reference(devTarget)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	remote, err := controlclient.New(contextReference)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	if err := waitDevNode(ctx, remote, 2*time.Minute); err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	if err := importDevImages(ctx, engine, manifest, clusterID, stderr); err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	if envFile == "" {
		envFile = filepath.Join(filepath.Dir(file), devEnvFile)
	} else if _, err := os.Stat(envFile); err != nil {
		writeError(stderr, err, false)
		return ExitInvalid
	}
	if err := syncDevSecrets(ctx, remote, *manifest, clusterID, envFile); err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	canonical, err := config.CanonicalJSON(manifest)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	idempotencyKey, err := newIdempotencyKey(rand.Reader)
	if err != nil {
		writeError(stderr, errors.New("generate idempotency key"), false)
		return ExitRuntime
	}
	response, err := remote.ApplyApplication(ctx, canonical, idempotencyKey)
	if err != nil {
		writeError(stderr, fmt.Errorf("deploy to the dev node failed: %w", err), false)
		return ExitRuntime
	}
	code := waitForDeployment(ctx, remote, response.GetName(), response.GetGeneration(), response.GetRevisionId(), "", timeout, false, stdout, stderr)
	// A node that just started reports its old workloads as failed until it
	// has restarted them, so give it a few more looks before giving up.
	for attempt := 0; code != ExitOK && started && attempt < 3 && ctx.Err() == nil; attempt++ {
		fmt.Fprintln(stderr, "The dev node is restarting its workloads; watching again")
		time.Sleep(5 * time.Second)
		code = waitForDeployment(ctx, remote, response.GetName(), response.GetGeneration(), response.GetRevisionId(), "", timeout, false, stdout, stderr)
	}
	if code != ExitOK {
		return code
	}
	fmt.Fprintln(stdout)
	for _, url := range urls {
		fmt.Fprintf(stdout, "  %s\n", url)
	}
	for _, port := range ports {
		fmt.Fprintf(stdout, "  %s://127.0.0.1:%d (%s)\n", port.protocol, port.port, port.service)
	}
	fmt.Fprintf(stdout, "\nInspect it with orch tui --target %s, or orch logs %s SERVICE --target %s.\nStop it with orch dev down.\n", devTarget, manifest.Name, devTarget)
	return ExitOK
}

func runDevDown(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("dev down", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var volumes bool
	flags.BoolVar(&volumes, "volumes", false, "also delete the dev node's state, images and volume data")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 {
		printDevHelp(stderr)
		return ExitInvalid
	}
	engine, err := detectDevEngine(ctx)
	if err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	if _, err := engine.run(ctx, nil, "rm", "-f", devContainer); err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	if !volumes {
		fmt.Fprintln(stdout, "Stopped the dev node. Its data is kept; orch dev down --volumes deletes it.")
		return ExitOK
	}
	if _, err := engine.run(ctx, nil, "volume", "rm", "-f", devVolume); err != nil {
		writeError(stderr, err, false)
		return ExitRuntime
	}
	if store, err := target.Default(); err == nil {
		_ = store.Remove(devTarget)
	}
	fmt.Fprintln(stdout, "Removed the dev node and its data.")
	return ExitOK
}

func printDevHelp(output io.Writer) {
	fmt.Fprintln(output, `orch dev runs an application on this machine, on a one-node cluster inside
Docker Desktop, OrbStack, Colima or Podman.

Usage:
  orch dev [-f orch.pkl] [--root DIR] [--env-file FILE] [--http-port 8080] [--timeout 10m]
  orch dev down [--volumes]

HTTP services are served at http://SERVICE.APP.localhost:8080. Images that exist
in the local engine are used instead of pulling them. Secrets are read from
orch.dev.env next to the manifest, one NAME=VALUE per line. The cluster is
available as target "dev" for every other command.`)
}

type devPort struct {
	protocol string
	port     int
	service  string
}

func (p devPort) publish() string {
	return fmt.Sprintf("127.0.0.1:%d:%d/%s", p.port, p.port, p.protocol)
}

// localizeDevManifest rewrites the parts of a manifest that only make sense on
// a real cluster: domains become SERVICE.APP.localhost over plain HTTP and
// node constraints are dropped, since the dev cluster has one node.
func localizeDevManifest(manifest *config.Manifest, httpPort int) ([]string, []devPort) {
	var urls []string
	var ports []devPort
	names := make([]string, 0, len(manifest.Services))
	for name := range manifest.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	suffix := ""
	if httpPort != 80 {
		suffix = ":" + strconv.Itoa(httpPort)
	}
	for _, name := range names {
		service := manifest.Services[name]
		service.Constraints = nil
		if service.HTTP != nil && (len(service.HTTP.Domains) != 0 || service.HTTP.Public) {
			if service.HTTP.Public {
				urls = append(urls, "http://localhost"+suffix+" ("+name+")")
			}
			domain := name + "." + manifest.Name + ".localhost"
			service.HTTP.Domains = []string{domain}
			service.HTTP.HTTPS = false
			urls = append(urls, "http://"+domain+suffix)
		}
		if service.TCP != nil && service.TCP.PublicPort != nil {
			ports = append(ports, devPort{"tcp", *service.TCP.PublicPort, name})
		}
		if service.TCP != nil && service.TCP.NodePort != nil {
			ports = append(ports, devPort{"tcp", *service.TCP.NodePort, name})
		}
		if service.UDP != nil {
			ports = append(ports, devPort{"udp", service.UDP.NodePort, name})
		}
		manifest.Services[name] = service
	}
	for name, job := range manifest.CronJobs {
		job.Constraints = nil
		manifest.CronJobs[name] = job
	}
	return urls, ports
}

type devEngine struct{ binary string }

func (e devEngine) run(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	command := exec.CommandContext(ctx, e.binary, args...)
	command.Stdin = stdin
	var out, errOut bytes.Buffer
	command.Stdout, command.Stderr = &out, &errOut
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(errOut.String())
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("%s %s: %s", filepath.Base(e.binary), args[0], message)
	}
	return strings.TrimSpace(out.String()), nil
}

// pipe streams the output of one engine command into another.
func (e devEngine) pipe(ctx context.Context, from, to []string) error {
	reader, writer := io.Pipe()
	source := exec.CommandContext(ctx, e.binary, from...)
	var sourceOutput bytes.Buffer
	source.Stdout, source.Stderr = writer, &sourceOutput
	if err := source.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		err := source.Wait()
		writer.CloseWithError(err)
		done <- err
	}()
	_, err := e.run(ctx, reader, to...)
	reader.Close()
	if sourceErr := <-done; sourceErr != nil && err == nil {
		message := strings.TrimSpace(sourceOutput.String())
		if message == "" {
			message = sourceErr.Error()
		}
		return fmt.Errorf("%s %s: %s", filepath.Base(e.binary), from[0], message)
	}
	return err
}

// detectDevEngine finds a running Docker-compatible engine, preferring
// ORCH_DEV_ENGINE, then docker, then podman.
func detectDevEngine(ctx context.Context) (devEngine, error) {
	candidates := []string{"docker", "podman"}
	if configured := os.Getenv("ORCH_DEV_ENGINE"); configured != "" {
		candidates = []string{configured}
	}
	found := ""
	for _, candidate := range candidates {
		binary, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		found = candidate
		engine := devEngine{binary: binary}
		if _, err := engine.run(ctx, nil, "info", "--format", "{{.ServerVersion}}"); err == nil {
			return engine, nil
		}
	}
	if found != "" {
		return devEngine{}, fmt.Errorf("%s is installed but not running; start Docker Desktop, OrbStack, Colima or the Podman machine and try again", found)
	}
	return devEngine{}, errors.New("orch dev needs a container engine; install Docker Desktop, OrbStack, Colima or Podman")
}

// devImage is the node image matching this CLI. Development builds use the
// image built by make dev-image.
func devImage() string {
	if image := os.Getenv("ORCH_DEV_IMAGE"); image != "" {
		return image
	}
	if strings.Contains(Version, "-dev") || Version == "" {
		return "orch-dev:local"
	}
	return "ghcr.io/torstendittmann/orch-dev:" + strings.TrimPrefix(Version, "v")
}

// ensureDevNode starts the dev container, recreating it when the image or the
// published ports change. Ports only accumulate, so applications deployed
// earlier keep theirs.
func ensureDevNode(ctx context.Context, engine devEngine, image string, httpPort int, ports []devPort, progress io.Writer) (bool, error) {
	published := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d:%d/tcp", devAPIPort, devAPIPort): true,
		fmt.Sprintf("127.0.0.1:%d:80/tcp", httpPort):               true,
	}
	for _, port := range ports {
		published[port.publish()] = true
	}
	if _, err := engine.run(ctx, nil, "image", "inspect", image); err != nil {
		if image == "orch-dev:local" {
			return false, errors.New("this development build of orch needs the orch-dev:local image; build it with make dev-image")
		}
		fmt.Fprintf(progress, "Pulling %s\n", image)
		if _, err := engine.run(ctx, nil, "pull", image); err != nil {
			return false, err
		}
	}
	imageID, err := engine.run(ctx, nil, "image", "inspect", "--format", "{{.Id}}", image)
	if err != nil {
		return false, err
	}
	state, err := engine.run(ctx, nil, "container", "inspect", "--format",
		`{{.State.Running}}|{{.Image}}|{{index .Config.Labels "`+devHTTPLabel+`"}}|{{index .Config.Labels "`+devPortsLabel+`"}}`, devContainer)
	if err == nil {
		fields := strings.SplitN(state, "|", 4)
		if len(fields) != 4 {
			return false, fmt.Errorf("unexpected %s inspect output %q", devContainer, state)
		}
		existing := map[string]bool{}
		previousHTTP := fmt.Sprintf("127.0.0.1:%s:80/tcp", fields[2])
		for _, value := range strings.Split(fields[3], ",") {
			existing[value] = true
			// Keep ports of applications deployed earlier; the HTTP port follows --http-port.
			if value != "" && value != previousHTTP {
				published[value] = true
			}
		}
		current := strings.TrimPrefix(fields[1], "sha256:") == strings.TrimPrefix(imageID, "sha256:")
		for value := range published {
			current = current && existing[value]
		}
		if current {
			if fields[0] == "true" {
				return false, waitDevContainer(ctx, engine)
			}
			fmt.Fprintln(progress, "Starting the dev node")
			if _, err := engine.run(ctx, nil, "start", devContainer); err != nil {
				return false, err
			}
			return true, waitDevContainer(ctx, engine)
		}
		fmt.Fprintln(progress, "Recreating the dev node for new ports or a new image; its data is kept")
		if _, err := engine.run(ctx, nil, "rm", "-f", devContainer); err != nil {
			return false, err
		}
	} else if !strings.Contains(strings.ToLower(err.Error()), "no such") {
		return false, err
	}
	values := make([]string, 0, len(published))
	for value := range published {
		values = append(values, value)
	}
	sort.Strings(values)
	args := []string{"run", "--detach", "--name", devContainer, "--hostname", devContainer,
		"--privileged", "--cgroupns", "private", "--restart", "unless-stopped",
		"--volume", devVolume + ":/var/lib/orch",
		"--label", devHTTPLabel + "=" + strconv.Itoa(httpPort), "--label", devPortsLabel + "=" + strings.Join(values, ",")}
	for _, value := range values {
		args = append(args, "--publish", value)
	}
	fmt.Fprintf(progress, "Starting the dev node from %s\n", image)
	if _, err := engine.run(ctx, nil, append(args, image)...); err != nil {
		_, _ = engine.run(ctx, nil, "rm", "-f", devContainer)
		if strings.Contains(err.Error(), "address already in use") || strings.Contains(err.Error(), "port is already allocated") {
			return false, fmt.Errorf("a port the dev node publishes is already in use on this machine: %w", err)
		}
		return false, err
	}
	return true, waitDevContainer(ctx, engine)
}

func waitDevContainer(ctx context.Context, engine devEngine) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, err := engine.run(ctx, nil, "exec", devContainer, "test", "-f", devReadyFile); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			logs, _ := engine.run(ctx, nil, "logs", "--tail", "20", devContainer)
			return fmt.Errorf("the dev node did not start; last log lines:\n%s", logs)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// installDevTarget copies the dev cluster's administrator identity into the
// local target "dev", replacing a target left over from a deleted dev node.
func installDevTarget(ctx context.Context, engine devEngine) (string, error) {
	staging, err := os.MkdirTemp("", "orch-dev-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, 0o700); err != nil {
		return "", err
	}
	if _, err := engine.run(ctx, nil, "cp", devContainer+":"+devAdminDir+"/.", staging); err != nil {
		return "", err
	}
	contextPath := filepath.Join(staging, "context.json")
	fresh, err := clusterctl.LoadContext(contextPath)
	if err != nil {
		return "", fmt.Errorf("read the dev node's administrator identity: %w", err)
	}
	store, err := target.Default()
	if err != nil {
		return "", err
	}
	if existing, err := store.Get(devTarget); err == nil {
		if current, err := clusterctl.LoadContext(existing.ContextPath); err == nil && current.Cluster.ID == fresh.Cluster.ID {
			return fresh.Cluster.ID, nil
		}
		if err := store.Remove(devTarget); err != nil {
			return "", err
		}
	} else if !errors.Is(err, target.ErrNotFound) {
		if err := store.Remove(devTarget); err != nil {
			return "", err
		}
	}
	if _, err := store.Import(devTarget, contextPath); err != nil {
		return "", err
	}
	return fresh.Cluster.ID, nil
}

func waitDevNode(ctx context.Context, remote *controlclient.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		nodes, err := remote.ListNodes(ctx)
		if err == nil {
			for _, node := range nodes {
				if node.GetConnectionStatus() == orchv1alpha1.NodeConnectionStatus_NODE_CONNECTION_STATUS_CONNECTED {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("the dev node's controller is unreachable: %w", err)
			}
			return errors.New("the dev node's agent did not connect; see docker logs orch-dev")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// importDevImages copies images that exist in the local engine into the dev
// node and pins them by digest, so locally built images work without a
// registry. Images the engine does not have are pulled by the node as usual.
func importDevImages(ctx context.Context, engine devEngine, manifest *config.Manifest, clusterID string, progress io.Writer) error {
	namespace := "orch-" + strings.ReplaceAll(clusterID, "_", "-")
	pinned := map[string]string{}
	pin := func(image string) (string, error) {
		if value, ok := pinned[image]; ok {
			return value, nil
		}
		pinned[image] = image
		named, err := reference.ParseNormalizedNamed(image)
		if err != nil {
			return image, nil
		}
		if _, ok := named.(reference.Digested); ok {
			return image, nil
		}
		localID, err := engine.run(ctx, nil, "image", "inspect", "--format", "{{.Id}}", image)
		if err != nil {
			return image, nil
		}
		fmt.Fprintf(progress, "Using local image %s\n", image)
		name := reference.TagNameOnly(named).String()
		ctr := []string{"exec", "-i", devContainer, "ctr", "--address", devSocket, "--namespace", namespace}
		// The source label records which local image was copied last, so an
		// unchanged image is not copied again.
		source := devImageSourceLabel + "=" + localID
		digest, labels, err := devNodeImage(ctx, engine, ctr, name)
		if err != nil {
			return "", err
		}
		if digest == "" || !strings.Contains(","+labels+",", ","+source+",") {
			if err := engine.pipe(ctx, []string{"image", "save", image}, append(ctr, "images", "import", "--no-unpack", "-")); err != nil {
				return "", fmt.Errorf("copy local image %s to the dev node: %w", image, err)
			}
			if _, err := engine.run(ctx, nil, append(ctr, "images", "label", name, source)...); err != nil {
				return "", err
			}
			if digest, _, err = devNodeImage(ctx, engine, ctr, name); err != nil {
				return "", err
			}
		}
		if !strings.HasPrefix(digest, "sha256:") {
			return "", fmt.Errorf("import local image %s: the dev node did not report its digest", image)
		}
		pinnedName := named.Name() + "@" + digest
		if _, err := engine.run(ctx, nil, append(ctr, "images", "tag", "--force", name, pinnedName)...); err != nil {
			return "", err
		}
		pinned[image] = pinnedName
		return pinnedName, nil
	}
	for name, service := range manifest.Services {
		image, err := pin(service.Image)
		if err != nil {
			return err
		}
		service.Image = image
		manifest.Services[name] = service
	}
	for name, job := range manifest.CronJobs {
		image, err := pin(job.Image)
		if err != nil {
			return err
		}
		job.Image = image
		manifest.CronJobs[name] = job
	}
	return nil
}

// devNodeImage returns the digest and labels of an image in the dev node, or
// an empty digest when the node does not have it.
func devNodeImage(ctx context.Context, engine devEngine, ctr []string, name string) (string, string, error) {
	listing, err := engine.run(ctx, nil, append(ctr, "images", "list", "name=="+name)...)
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(listing, "\n") {
		// REF TYPE DIGEST SIZE PLATFORMS LABELS, where SIZE is "N unit".
		if fields := strings.Fields(line); len(fields) >= 3 && fields[0] == name {
			return fields[2], fields[len(fields)-1], nil
		}
	}
	return "", "", nil
}

// syncDevSecrets sets the secrets from the env file that are new or changed
// since the last run, then reports any secret the manifest still lacks.
func syncDevSecrets(ctx context.Context, remote *controlclient.Client, manifest config.Manifest, clusterID, envFile string) error {
	values, err := readDevEnvFile(envFile)
	if err != nil {
		return err
	}
	cachePath := ""
	cache := map[string]string{}
	if store, err := target.Default(); err == nil {
		if resolved, err := store.Get(devTarget); err == nil {
			cachePath = filepath.Join(filepath.Dir(resolved.ContextPath), devSecretsCache)
			if data, err := os.ReadFile(cachePath); err == nil {
				_ = json.Unmarshal(data, &cache)
			}
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	changed := false
	for _, name := range names {
		sum := sha256.Sum256([]byte(clusterID + "\x00" + manifest.Name + "\x00" + name + "\x00" + values[name]))
		key, digest := manifest.Name+"/"+name, hex.EncodeToString(sum[:])
		if cache[key] == digest {
			continue
		}
		idempotencyKey, err := newIdempotencyKey(rand.Reader)
		if err != nil {
			return err
		}
		if _, err := remote.SetSecret(ctx, manifest.Name, name, []byte(values[name]), idempotencyKey); err != nil {
			return fmt.Errorf("set secret %s from %s: %w", name, filepath.Base(envFile), err)
		}
		cache[key], changed = digest, true
	}
	if changed && cachePath != "" {
		if data, err := json.Marshal(cache); err == nil {
			_ = os.WriteFile(cachePath, data, 0o600)
		}
	}
	existing, err := remote.ListSecrets(ctx, manifest.Name)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, secret := range existing {
		have[secret.GetName()] = true
	}
	var missing []string
	need := func(name string) {
		if !have[name] {
			have[name] = true
			missing = append(missing, name)
		}
	}
	for _, service := range manifest.Services {
		for _, name := range service.SecretEnv {
			need(name)
		}
		for _, file := range service.SecretFiles {
			need(file.Source)
		}
	}
	for _, job := range manifest.CronJobs {
		for _, name := range job.SecretEnv {
			need(name)
		}
		for _, file := range job.SecretFiles {
			need(file.Source)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	var message strings.Builder
	fmt.Fprintf(&message, "%s needs secrets that are not set: %s\nAdd them to %s as NAME=VALUE lines, for example:\n", manifest.Name, strings.Join(missing, ", "), envFile)
	for _, name := range missing {
		fmt.Fprintf(&message, "  %s=...\n", name)
	}
	return errors.New(strings.TrimSuffix(message.String(), "\n"))
}

func readDevEnvFile(path string) (map[string]string, error) {
	values := map[string]string{}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return values, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		name, value, ok := strings.Cut(text, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if strings.HasPrefix(value, `"`) {
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			}
		}
		if !ok || name == "" || value == "" {
			return nil, fmt.Errorf("%s:%d: expected NAME=VALUE with a nonempty value", path, line)
		}
		values[name] = value
	}
	return values, scanner.Err()
}
