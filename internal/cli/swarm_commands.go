package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	agentctl "change.codes/torsten/better-swarm/internal/agent"
	controlclient "change.codes/torsten/better-swarm/internal/client"
	"change.codes/torsten/better-swarm/internal/config"
	"change.codes/torsten/better-swarm/internal/host"
	"change.codes/torsten/better-swarm/internal/target"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// initAdminDir keeps the break-glass mTLS administrator context on the
// controller. Day-to-day access uses the administrator socket instead.
const initAdminDir = "/etc/orch/admin"

func runInit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var name, address, recoveryDir string
	var port int
	var wait time.Duration
	var jsonOutput bool
	flags.StringVar(&name, "name", "orch", "cluster name")
	flags.StringVar(&address, "advertise-address", "", "address workers and clients use to reach this host (default: detected)")
	flags.IntVar(&port, "port", 9443, "controller port")
	flags.StringVar(&recoveryDir, "recovery-dir", "", "also write a day-zero recovery set to this new directory")
	flags.DurationVar(&wait, "wait-timeout", 2*time.Minute, "maximum wait for controller readiness")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 0 || port < 1024 || port > 65535 || wait <= 0 {
		writeError(stderr, errors.New("usage: orch init [--name NAME] [--advertise-address IP] [--port 9443] [--recovery-dir DIR]"), jsonOutput)
		return ExitInvalid
	}
	if err := requireRootHint(); err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if address == "" {
		detected, err := host.DetectAdvertiseAddress()
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitInvalid
		}
		address = detected
	}
	portText := strconv.Itoa(port)
	options := host.ControllerOptions{
		Name: name, Server: "https://" + net.JoinHostPort(address, portText), Listen: net.JoinHostPort("0.0.0.0", portText),
		AdminDir: initAdminDir, RecoveryDir: recoveryDir,
	}
	result, err := host.InstallController(ctx, host.ControllerInstallOptions{ControllerOptions: options, WaitTimeout: wait, Progress: stderr})
	if jsonOutput {
		if code := writeJSON(stdout, stderr, result); code != ExitOK {
			return code
		}
	} else if err == nil && result.Ready {
		fmt.Fprintf(stdout, "Cluster %s is ready at %s\n\n", result.Name, result.Server)
		fmt.Fprintln(stdout, "Add a worker: create a join token here, then run the printed command on the worker as root:")
		fmt.Fprintln(stdout, "  orch join-token worker-1")
		fmt.Fprintln(stdout, "\nManage the cluster from your workstation over SSH:")
		fmt.Fprintf(stdout, "  orch target add %s ssh://root@%s\n", result.Name, address)
		if result.RecoveryDir == "" {
			fmt.Fprintf(stdout, "\nBack up the controller (keep the output off this host):\n  orch cluster backup --data-dir %s --output FILE --secret-key-output KEY_FILE\n", result.DataDir)
		}
	}
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	return ExitOK
}

func runJoinToken(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("join-token", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var contextPath string
	var ttl time.Duration
	var jsonOutput bool
	flags.StringVar(&contextPath, "context", "", "administrator context path")
	flags.DurationVar(&ttl, "ttl", time.Hour, "token lifetime from 1m through 24h")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	reordered := reorderInterspersed(args, map[string]bool{"-context": true, "--context": true, "-ttl": true, "--ttl": true})
	if err := flags.Parse(reordered); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 1 || contextPath == "" || ttl < time.Minute || ttl > 24*time.Hour {
		writeError(stderr, errors.New("usage: orch join-token NODE [--ttl 1h] [--target NAME]"), jsonOutput)
		return ExitInvalid
	}
	nodeName := flags.Arg(0)
	if err := config.ValidateApplicationName(nodeName); err != nil {
		writeError(stderr, fmt.Errorf("invalid node name: %w", err), jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.New(contextPath)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	defer remote.CloseIdleConnections()
	cluster, caPEM, err := joinClusterIdentity(ctx, remote)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	secret := make([]byte, 32)
	joinIDBytes := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, secret); err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if _, err := io.ReadFull(rand.Reader, joinIDBytes); err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	idempotencyKey, err := newIdempotencyKey(rand.Reader)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	artifact := agentctl.JoinArtifact{
		Version: agentctl.JoinArtifactVersion, JoinID: "join_" + hex.EncodeToString(joinIDBytes), Secret: secret,
		NodeName: nodeName, ClusterID: cluster.GetId(), ControllerEndpoint: cluster.GetEndpoint(),
		CACertificatePEM: caPEM, ExpiresAt: time.Now().UTC().Add(ttl).Truncate(time.Second),
	}
	token, err := agentctl.EncodeJoinToken(artifact)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	secretHash := sha256.Sum256(secret)
	if _, err := remote.CreateJoinToken(ctx, &orchv1alpha1.CreateJoinTokenRequest{Id: artifact.JoinID, NodeName: nodeName,
		SecretHash: secretHash[:], ExpiresAt: timestamppb.New(artifact.ExpiresAt), IdempotencyKey: idempotencyKey}); err != nil {
		writeError(stderr, fmt.Errorf("create join token: %w", err), jsonOutput)
		return ExitRuntime
	}
	command := "orch join " + token
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			ID        string    `json:"id"`
			NodeName  string    `json:"node_name"`
			ExpiresAt time.Time `json:"expires_at"`
			Token     string    `json:"token"`
			Command   string    `json:"command"`
		}{ID: artifact.JoinID, NodeName: nodeName, ExpiresAt: artifact.ExpiresAt, Token: token, Command: command})
	}
	fmt.Fprintf(stdout, "Run this on %s as root (single use, expires %s):\n\n  %s\n", nodeName, artifact.ExpiresAt.Format(time.RFC3339), command)
	return ExitOK
}

// joinClusterIdentity returns the cluster and CA a join credential must name.
func joinClusterIdentity(ctx context.Context, remote *controlclient.Client) (*orchv1alpha1.Cluster, []byte, error) {
	cluster, caPEM, err := remote.GetClusterIdentity(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(caPEM) == 0 {
		return nil, nil, errors.New("controller did not return its CA certificate; upgrade the controller")
	}
	return cluster, caPEM, nil
}

func runJoin(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("join", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var address string
	var wait time.Duration
	var jsonOutput bool
	flags.StringVar(&address, "advertise-address", "", "IPv4 address other nodes use to reach this host (default: detected)")
	flags.DurationVar(&wait, "wait-timeout", 5*time.Minute, "maximum wait for agent readiness")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	reordered := reorderInterspersed(args, map[string]bool{"-advertise-address": true, "--advertise-address": true, "-wait-timeout": true, "--wait-timeout": true})
	if err := flags.Parse(reordered); err != nil {
		return ExitInvalid
	}
	if flags.NArg() != 1 || wait <= 0 {
		writeError(stderr, errors.New("usage: orch join TOKEN|- [--advertise-address IP]"), jsonOutput)
		return ExitInvalid
	}
	if err := requireRootHint(); err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if address == "" {
		detected, err := host.DetectAdvertiseAddress()
		if err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitInvalid
		}
		address = detected
	}
	token := flags.Arg(0)
	if token == "-" {
		// Reading from stdin keeps the token out of the process list.
		line, err := io.ReadAll(io.LimitReader(os.Stdin, 4097))
		if err != nil {
			writeError(stderr, fmt.Errorf("read join token from stdin: %w", err), jsonOutput)
			return ExitInvalid
		}
		token = strings.TrimSpace(string(line))
	}
	resolveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	artifact, err := agentctl.ResolveJoinToken(resolveCtx, token, time.Now())
	cancel()
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	encoded, err := agentctl.EncodeJoinArtifact(artifact)
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	result, err := host.Install(ctx, host.InstallOptions{Address: address, JoinFile: "-", JoinInput: bytes.NewReader(encoded), WaitTimeout: wait, Progress: stderr})
	if jsonOutput {
		if code := writeJSON(stdout, stderr, result); code != ExitOK {
			return code
		}
	} else if err == nil && result.Ready {
		fmt.Fprintf(stdout, "Node %s joined cluster at %s (advertising %s)\n", artifact.NodeName, artifact.ControllerEndpoint, address)
		if result.Ingress != nil && *result.Ingress {
			fmt.Fprintln(stdout, "This node accepts ingress traffic on ports 80 and 443.")
		}
	}
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	return ExitOK
}

func runTargetAdd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("target add", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var use, jsonOutput bool
	flags.BoolVar(&use, "use", false, "select the new target (default when none is selected)")
	flags.BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	reordered := reorderInterspersed(args, map[string]bool{})
	if err := flags.Parse(reordered); err != nil || flags.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: orch target add NAME ssh://[USER@]HOST[:PORT] [--use] [--json]")
		return ExitInvalid
	}
	name := flags.Arg(0)
	if err := target.ValidateName(name); err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitInvalid
	}
	transport, err := parseSocketLocation(flags.Arg(1))
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitInvalid
	}
	store, err := target.Default()
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if _, err := store.Get(name); err == nil {
		writeError(stderr, fmt.Errorf("target %q already exists", name), jsonOutput)
		return ExitInvalid
	}
	remote, err := controlclient.NewEndpoint(controlclient.Endpoint{Target: name, Transport: transport})
	if err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	testCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	cluster, err := remote.GetCluster(testCtx)
	cancel()
	_ = remote.Close()
	if err != nil {
		if transport.SSHHost != "" {
			err = fmt.Errorf("no controller answered through %s (run orch init there and log in as root): %w", transport.SSHHost, err)
		}
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if _, err := store.AddSocket(name, transport, target.Cluster{ID: cluster.GetId(), Name: cluster.GetName()}); err != nil {
		writeError(stderr, err, jsonOutput)
		return ExitRuntime
	}
	if !use {
		_, currentErr := store.Current()
		use = errors.Is(currentErr, target.ErrNoCurrent)
	}
	if use {
		if err := store.Use(name); err != nil {
			writeError(stderr, err, jsonOutput)
			return ExitRuntime
		}
	}
	if jsonOutput {
		return writeJSON(stdout, stderr, struct {
			Name    string `json:"name"`
			Cluster string `json:"cluster"`
			Current bool   `json:"current"`
		}{Name: name, Cluster: cluster.GetName(), Current: use})
	}
	fmt.Fprintf(stdout, "Added target %s for cluster %s\n", name, cluster.GetName())
	if use {
		fmt.Fprintf(stdout, "Using target %s\n", name)
	} else {
		fmt.Fprintf(stdout, "Select it with: orch target use %s\n", name)
	}
	return ExitOK
}

// parseSocketLocation accepts ssh://[user@]host[:port][/socket] and
// unix:///socket locations for an administrator socket.
func parseSocketLocation(location string) (target.Transport, error) {
	parsed, err := url.Parse(location)
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return target.Transport{}, fmt.Errorf("location must be ssh://[USER@]HOST[:PORT] or unix:///PATH")
	}
	transport := target.Transport{Type: target.TransportSocket, Socket: target.DefaultAdminSocket}
	switch parsed.Scheme {
	case "ssh":
		if parsed.Hostname() == "" {
			return target.Transport{}, fmt.Errorf("ssh location needs a host")
		}
		if _, hasPassword := parsed.User.Password(); hasPassword {
			return target.Transport{}, fmt.Errorf("ssh location must not contain a password; use SSH keys")
		}
		transport.SSHHost = parsed.Hostname()
		if user := parsed.User.Username(); user != "" {
			transport.SSHHost = user + "@" + transport.SSHHost
		}
		if parsed.Port() != "" {
			port, err := strconv.Atoi(parsed.Port())
			if err != nil || port < 1 || port > 65535 {
				return target.Transport{}, fmt.Errorf("ssh port must be from 1 through 65535")
			}
			transport.SSHPort = port
		}
		if parsed.Path != "" && parsed.Path != "/" {
			transport.Socket = parsed.Path
		}
	case "unix":
		if parsed.Host != "" || parsed.User != nil || parsed.Path == "" {
			return target.Transport{}, fmt.Errorf("unix location must be unix:///PATH")
		}
		transport.Socket = parsed.Path
	default:
		return target.Transport{}, fmt.Errorf("location must be ssh://[USER@]HOST[:PORT] or unix:///PATH")
	}
	if err := target.ValidateTransport(transport); err != nil {
		return target.Transport{}, err
	}
	if strings.HasPrefix(transport.SSHHost, "-") {
		return target.Transport{}, fmt.Errorf("SSH host must not start with -")
	}
	return transport, nil
}

// requireRootHint explains why host commands need root before doing work.
func requireRootHint() error {
	if os.Geteuid() != 0 {
		return errors.New("run this command as root (for example with sudo)")
	}
	return nil
}
