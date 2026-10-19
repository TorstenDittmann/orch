package ingress

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

type CaddyOptions struct {
	Binary      string
	AdminSocket string
	DataDir     string
	StateDir    string
	User        string
	Probe       func(context.Context, Route) error
	Now         func() time.Time
	testIssuer  *testACMEIssuer
}

type processIdentity struct {
	uid int
	gid int
}

type runningProcessIdentity struct {
	PID          int    `json:"pid"`
	StartTime    uint64 `json:"start_time"`
	Device       uint64 `json:"device"`
	Inode        uint64 `json:"inode"`
	SocketDevice uint64 `json:"socket_device"`
	SocketInode  uint64 `json:"socket_inode"`
}

type Caddy struct {
	mu        sync.Mutex
	options   CaddyOptions
	identity  processIdentity
	client    *http.Client
	process   *exec.Cmd
	now       func() time.Time
	failures  uint32
	nextStart time.Time
}

func NewCaddy(options CaddyOptions) (*Caddy, error) {
	for name, value := range map[string]string{
		"Caddy binary": options.Binary, "Caddy admin socket": options.AdminSocket,
		"Caddy data directory": options.DataDir, "Caddy state directory": options.StateDir,
		"Caddy user": options.User,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%s is required", name)
		}
	}
	var err error
	options.Binary, err = filepath.Abs(options.Binary)
	if err != nil {
		return nil, err
	}
	options.AdminSocket, err = filepath.Abs(options.AdminSocket)
	if err != nil {
		return nil, err
	}
	options.DataDir, err = filepath.Abs(options.DataDir)
	if err != nil {
		return nil, err
	}
	if filepath.Dir(options.AdminSocket) != options.DataDir {
		return nil, fmt.Errorf("Caddy admin socket must be directly beneath its data directory")
	}
	options.StateDir, err = filepath.Abs(options.StateDir)
	if err != nil {
		return nil, err
	}
	if pathContains(options.DataDir, options.StateDir) || pathContains(options.StateDir, options.DataDir) {
		return nil, fmt.Errorf("Caddy agent state and process data directories must be separate")
	}
	if err := ensureAgentStateDirectory(options.StateDir); err != nil {
		return nil, err
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", options.AdminSocket)
		},
		DisableKeepAlives: true,
	}
	return &Caddy{options: options, client: &http.Client{Transport: transport, Timeout: 5 * time.Second}, now: now}, nil
}

func (c *Caddy) Preflight(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	identity, err := validateProcessOptions(c.options)
	if err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, c.options.Binary, "version")
	command.Env = caddyCommandEnvironment(c.options)
	if err := prepareProcessCommand(command, identity); err != nil {
		return "", err
	}
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("read Caddy version: %w", err)
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 || fields[0] != RequiredCaddyVersion {
		return "", fmt.Errorf("Caddy %s is required, found %q", RequiredCaddyVersion, strings.TrimSpace(string(output)))
	}
	c.identity = identity
	return fields[0], nil
}

func (c *Caddy) Apply(ctx context.Context, routes []Route, now time.Time) ([]model.RouteStatus, error) {
	// Reconciliation also services log/control requests. Never let certificate
	// provisioning or a stuck validation process monopolize that pass.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.identity.uid == 0 {
		return nil, fmt.Errorf("Caddy preflight has not completed")
	}
	if err := c.ensureRunning(ctx); err != nil {
		return applyFailure(routes, now, "caddy_unavailable", err)
	}
	configuration, digest, err := render(routes, c.options.AdminSocket, c.options.testIssuer)
	if err != nil {
		return nil, err
	}
	acceptedConfiguration, acceptedDigest, acceptedErr := c.acceptedConfiguration()
	if acceptedErr != nil && !errors.Is(acceptedErr, os.ErrNotExist) {
		return nil, acceptedErr
	}
	if acceptedErr == nil && acceptedDigest == digest {
		return c.probeRoutes(ctx, routes, now), nil
	}
	candidatePath := filepath.Join(c.options.StateDir, "candidate-config.json")
	if err := publishAgentFile(candidatePath, configuration, 0o600); err != nil {
		return nil, fmt.Errorf("persist candidate Caddy fingerprint: %w", err)
	}
	staged, err := c.stageConfiguration(configuration)
	if err != nil {
		_ = os.Remove(candidatePath)
		return nil, err
	}
	defer os.Remove(staged)
	if err := c.validateConfiguration(ctx, staged); err != nil {
		_ = os.Remove(candidatePath)
		return applyFailure(routes, now, "caddy_validation_failed", err)
	}
	if reason, err := c.loadCandidate(ctx, configuration); err != nil {
		// A timeout is ambiguous: Caddy may already have applied the config.
		// Keep its fingerprint so the next pass can authenticate and adopt it.
		return applyFailure(routes, now, reason, err)
	}
	// Caddy accepted the configuration, so it is kept even when individual
	// routes do not probe healthy yet: reverting would take every other route
	// on the node down with it and restart pending ACME issuance. Only a Caddy
	// that stops serving is rolled back, by restarting from the accepted file.
	if err := publishProcessFile(filepath.Join(c.options.DataDir, "config.json"), configuration, c.identity, 0o600); err != nil {
		if restoreErr := c.restoreAcceptedConfiguration(acceptedConfiguration); restoreErr != nil {
			return nil, fmt.Errorf("persist accepted Caddy configuration: %w; restore prior configuration: %v", err, restoreErr)
		}
		_ = os.Remove(candidatePath)
		return nil, fmt.Errorf("persist accepted Caddy configuration: %w", err)
	}
	if err := promoteAgentFile(candidatePath, filepath.Join(c.options.StateDir, "accepted-config.json")); err != nil {
		if restoreErr := c.restoreAcceptedConfiguration(acceptedConfiguration); restoreErr != nil {
			return nil, fmt.Errorf("accept Caddy configuration: %w; restore prior configuration: %v", err, restoreErr)
		}
		_ = os.Remove(candidatePath)
		return nil, fmt.Errorf("accept Caddy configuration: %w", err)
	}
	return c.probeRoutes(ctx, routes, now), nil
}

func (c *Caddy) ensureRunning(ctx context.Context) error {
	if c.ping(ctx) == nil {
		if err := c.validateManagedProcess(ctx); err != nil {
			return err
		}
		c.failures = 0
		c.nextStart = time.Time{}
		return nil
	}
	if c.now().UTC().Before(c.nextStart) {
		return fmt.Errorf("Caddy restart is backing off until %s", c.nextStart.Format(time.RFC3339Nano))
	}
	metadata, metadataErr := c.readProcessMetadata("process.json")
	metadataName := "process.json"
	if errors.Is(metadataErr, os.ErrNotExist) {
		metadata, metadataErr = c.readProcessMetadata("starting-process.json")
		metadataName = "starting-process.json"
	}
	if metadataErr == nil {
		if running, err := inspectProcessPID(metadata.PID, c.options, c.identity); err == nil && sameProcess(running, metadata) {
			return fmt.Errorf("managed Caddy process %d is alive but its admin socket is unavailable", metadata.PID)
		}
		if err := removeOwnedStaleSocket(c.options, c.identity, metadata); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(c.options.StateDir, metadataName)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if !errors.Is(metadataErr, os.ErrNotExist) {
		return metadataErr
	} else if _, err := os.Lstat(c.options.AdminSocket); err == nil {
		return fmt.Errorf("refusing to replace a Caddy admin socket without durable process identity")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	configuration, _, err := c.acceptedConfiguration()
	if errors.Is(err, os.ErrNotExist) {
		configuration, _, err = render(nil, c.options.AdminSocket, c.options.testIssuer)
		if err == nil {
			err = publishAgentFile(filepath.Join(c.options.StateDir, "accepted-config.json"), configuration, 0o600)
		}
	}
	if err != nil {
		return c.startFailure(err)
	}
	configPath := filepath.Join(c.options.DataDir, "config.json")
	if err := publishProcessFile(configPath, configuration, c.identity, 0o600); err != nil {
		return c.startFailure(err)
	}
	logPath := filepath.Join(c.options.DataDir, "bootstrap.log")
	logFile, err := openProcessLog(logPath, c.identity)
	if err != nil {
		return err
	}
	command := exec.Command(c.options.Binary, "run", "--config", configPath,
		"--pidfile", filepath.Join(c.options.DataDir, "caddy.pid"))
	command.Stdout = logFile
	command.Stderr = logFile
	command.Env = caddyCommandEnvironment(c.options)
	if err := prepareProcessCommand(command, c.identity); err != nil {
		_ = logFile.Close()
		return c.startFailure(err)
	}
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		return c.startFailure(fmt.Errorf("start Caddy: %w", err))
	}
	running, err := inspectProcessPID(command.Process.Pid, c.options, c.identity)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = logFile.Close()
		return c.startFailure(fmt.Errorf("inspect started Caddy process: %w", err))
	}
	encoded, err := json.Marshal(running)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = logFile.Close()
		return c.startFailure(err)
	}
	if err := publishAgentFile(filepath.Join(c.options.StateDir, "starting-process.json"), append(encoded, '\n'), 0o600); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = logFile.Close()
		return c.startFailure(fmt.Errorf("persist Caddy process identity: %w", err))
	}
	c.process = command
	go func() {
		_ = command.Wait()
		_ = logFile.Close()
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := c.ping(ctx); err == nil {
			if err := c.validateManagedProcess(ctx); err == nil {
				c.failures = 0
				c.nextStart = time.Time{}
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return c.startFailure(ctx.Err())
		case <-deadline.C:
			return c.startFailure(fmt.Errorf("Caddy admin socket did not become ready"))
		case <-ticker.C:
		}
	}
}

func (c *Caddy) ping(ctx context.Context) error {
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://caddy/config/", nil)
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.CopyN(io.Discard, response.Body, 4<<10)
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("Caddy admin API returned %s", response.Status)
	}
	return nil
}

func (c *Caddy) validateManagedProcess(ctx context.Context) error {
	running, err := inspectRunningProcess(c.options, c.identity)
	if err != nil {
		return err
	}
	metadata, err := c.readProcessMetadata("process.json")
	if errors.Is(err, os.ErrNotExist) {
		starting, startErr := c.readProcessMetadata("starting-process.json")
		if startErr != nil || !sameProcess(running, starting) {
			return fmt.Errorf("running Caddy process has no matching durable start identity")
		}
		encoded, encodeErr := json.Marshal(running)
		if encodeErr != nil {
			return encodeErr
		}
		if err := publishAgentFile(filepath.Join(c.options.StateDir, "process.json"), append(encoded, '\n'), 0o600); err != nil {
			return fmt.Errorf("finish Caddy process adoption: %w", err)
		}
		if err := os.Remove(filepath.Join(c.options.StateDir, "starting-process.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		metadata = running
	} else if err != nil {
		return fmt.Errorf("read durable Caddy process identity: %w", err)
	}
	if running != metadata {
		return fmt.Errorf("running Caddy process does not match its durable start identity")
	}
	return c.validateLiveConfiguration(ctx)
}

func (c *Caddy) readProcessMetadata(name string) (runningProcessIdentity, error) {
	contents, err := readAgentFile(filepath.Join(c.options.StateDir, name), 4<<10)
	if err != nil {
		return runningProcessIdentity{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var metadata runningProcessIdentity
	if err := decoder.Decode(&metadata); err != nil {
		return runningProcessIdentity{}, fmt.Errorf("decode Caddy process identity: %w", err)
	}
	if metadata.PID < 2 || metadata.StartTime == 0 || metadata.Inode == 0 {
		return runningProcessIdentity{}, fmt.Errorf("Caddy process identity is incomplete")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return runningProcessIdentity{}, fmt.Errorf("Caddy process identity contains trailing data")
	}
	return metadata, nil
}

func sameProcess(left, right runningProcessIdentity) bool {
	return left.PID == right.PID && left.StartTime == right.StartTime &&
		left.Device == right.Device && left.Inode == right.Inode
}

func (c *Caddy) acceptedConfiguration() ([]byte, string, error) {
	configuration, err := readAgentFile(filepath.Join(c.options.StateDir, "accepted-config.json"), 1<<20)
	if err != nil {
		return nil, "", err
	}
	if !json.Valid(configuration) {
		return nil, "", fmt.Errorf("accepted Caddy configuration is invalid")
	}
	sum := sha256.Sum256(configuration)
	return configuration, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (c *Caddy) validateLiveConfiguration(ctx context.Context) error {
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://caddy/config/", nil)
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("read live Caddy configuration: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("read live Caddy configuration: admin API returned %s", response.Status)
	}
	live, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(live) > 1<<20 || !json.Valid(live) {
		return fmt.Errorf("live Caddy configuration is invalid or too large")
	}
	canonicalLive, err := canonicalJSON(live)
	if err != nil {
		return err
	}
	for _, name := range []string{"accepted-config.json", "candidate-config.json"} {
		durable, err := readAgentFile(filepath.Join(c.options.StateDir, name), 1<<20)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		canonicalDurable, err := canonicalJSON(durable)
		if err != nil {
			return fmt.Errorf("durable Caddy configuration %q is invalid: %w", name, err)
		}
		if bytes.Equal(canonicalLive, canonicalDurable) {
			if name == "candidate-config.json" {
				// Finish an interrupted load before another candidate can replace
				// its only durable fingerprint. The live config has been matched
				// against root-owned state, not adopted from arbitrary admin data.
				if err := publishProcessFile(filepath.Join(c.options.DataDir, "config.json"), durable, c.identity, 0o600); err != nil {
					return fmt.Errorf("persist recovered Caddy configuration: %w", err)
				}
				if err := promoteAgentFile(filepath.Join(c.options.StateDir, name), filepath.Join(c.options.StateDir, "accepted-config.json")); err != nil {
					return fmt.Errorf("accept recovered Caddy configuration: %w", err)
				}
			}
			return nil
		}
	}
	return fmt.Errorf("live Caddy configuration has no matching durable fingerprint")
}

func canonicalJSON(contents []byte) ([]byte, error) {
	var value any
	if err := json.Unmarshal(contents, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func (c *Caddy) restoreAcceptedConfiguration(configuration []byte) error {
	if len(configuration) == 0 {
		var err error
		configuration, _, err = render(nil, c.options.AdminSocket, c.options.testIssuer)
		if err != nil {
			return err
		}
	}
	restoreCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.load(restoreCtx, configuration); err != nil {
		return err
	}
	return publishProcessFile(filepath.Join(c.options.DataDir, "config.json"), configuration, c.identity, 0o600)
}

func (c *Caddy) startFailure(err error) error {
	if c.failures < 6 {
		c.failures++
	}
	exponent := c.failures - 1
	if exponent > 5 {
		exponent = 5
	}
	c.nextStart = c.now().UTC().Add(time.Second * time.Duration(1<<exponent))
	return err
}

func (c *Caddy) loadCandidate(ctx context.Context, configuration []byte) (string, error) {
	if err := c.load(ctx, configuration); err != nil {
		return "caddy_load_failed", err
	}
	if err := c.ping(ctx); err != nil {
		return "caddy_unavailable", err
	}
	return "", nil
}

func (c *Caddy) load(ctx context.Context, configuration []byte) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://caddy/load", bytes.NewReader(configuration))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Cache-Control", "must-revalidate")
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("Caddy rejected configuration with %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func (c *Caddy) stageConfiguration(configuration []byte) (string, error) {
	file, err := os.CreateTemp(c.options.DataDir, ".config-*.json")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if _, err := file.Write(configuration); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := protectProcessFile(path, c.identity, 0o600); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func (c *Caddy) validateConfiguration(ctx context.Context, path string) error {
	command := exec.CommandContext(ctx, c.options.Binary, "validate", "--config", path)
	command.Env = caddyCommandEnvironment(c.options)
	if err := prepareProcessCommand(command, c.identity); err != nil {
		return err
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("validate Caddy config: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func caddyCommandEnvironment(options CaddyOptions) []string {
	return []string{
		"HOME=" + options.DataDir,
		"PATH=/usr/bin:/bin",
		"XDG_CONFIG_HOME=" + filepath.Join(options.DataDir, "config"),
		"XDG_DATA_HOME=" + filepath.Join(options.DataDir, "data"),
	}
}

// probeRoutes reports every route on its own, so one failing upstream or a
// certificate that is still being issued never affects the other routes.
func (c *Caddy) probeRoutes(ctx context.Context, routes []Route, now time.Time) []model.RouteStatus {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return probedRoutes(routes, c.probeRoutesOnce(probeCtx, routes), now)
}

// probeRoutesOnce samples each route once. Pending certificates are retried by
// the next reconciliation, not by sleeping while the supervisor is locked.
func (c *Caddy) probeRoutesOnce(ctx context.Context, routes []Route) []error {
	probe := c.options.Probe
	if probe == nil {
		probe = probeRoute
	}
	failures := make([]error, len(routes))
	// Probe independently so a slow first hostname cannot starve healthy
	// routes later in canonical order. Each writer owns one result slot.
	var probes sync.WaitGroup
	for index, route := range routes {
		probes.Add(1)
		go func() {
			defer probes.Done()
			if err := ctx.Err(); err != nil {
				failures[index] = err
				return
			}
			failures[index] = probe(ctx, route)
		}()
	}
	probes.Wait()
	return failures
}

// routeStatusError is a probe that reached the route but got a server error.
type routeStatusError struct {
	domain string
	status int
}

func (e *routeStatusError) Error() string {
	return fmt.Sprintf("route %s returned status %d", e.domain, e.status)
}

func probeRoute(ctx context.Context, route Route) error {
	port := "80"
	scheme := "http"
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	if route.HTTPS {
		port = "443"
		scheme = "https"
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: route.Domain}
	}
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", port))
	}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	host := route.Domain
	if host == model.PublicHTTPRoute {
		host = "127.0.0.1"
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+host+"/", nil)
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.CopyN(io.Discard, response.Body, 4<<10)
	if response.StatusCode >= 500 {
		return &routeStatusError{domain: route.Domain, status: response.StatusCode}
	}
	return nil
}

func activeRoutes(routes []Route, now time.Time) []model.RouteStatus {
	result := make([]model.RouteStatus, 0, len(routes))
	for _, route := range routes {
		certificate := "disabled"
		if route.HTTPS {
			certificate = "managed"
		}
		result = append(result, model.RouteStatus{Application: route.Application, Service: route.Service,
			Domain: route.Domain, HTTPS: route.HTTPS, State: model.RouteActive,
			CertificateState: certificate, UpstreamCount: uint32(len(route.Upstreams)), UpdatedAt: now.UTC()})
	}
	return result
}

func failedRoutes(routes []Route, now time.Time, reason, message string) []model.RouteStatus {
	if len(message) > 512 {
		message = message[:512]
	}
	result := activeRoutes(routes, now)
	for index := range result {
		result[index].State = model.RouteFailed
		result[index].CertificateState = "pending"
		result[index].Reason = reason
		result[index].Message = message
	}
	return result
}

// probedRoutes marks routes whose probe failed. An HTTPS route that cannot be
// reached at all is most likely still waiting for its certificate, so it is
// pending rather than failed.
func probedRoutes(routes []Route, failures []error, now time.Time) []model.RouteStatus {
	result := activeRoutes(routes, now)
	for index, failure := range failures {
		if failure == nil {
			continue
		}
		message := failure.Error()
		if len(message) > 512 {
			message = message[:512]
		}
		var status *routeStatusError
		result[index].State, result[index].Reason = model.RouteFailed, "route_probe_failed"
		if routes[index].HTTPS && !errors.As(failure, &status) {
			result[index].State, result[index].Reason = model.RoutePending, "certificate_pending"
		}
		result[index].CertificateState = "pending"
		result[index].Message = message
	}
	return result
}

func applyFailure(routes []Route, now time.Time, reason string, failure error) ([]model.RouteStatus, error) {
	if len(routes) == 0 {
		return nil, fmt.Errorf("%s: %w", reason, failure)
	}
	return failedRoutes(routes, now, reason, failure.Error()), nil
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

func ensureAgentStateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create Caddy agent state directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("Caddy agent state directory must be a private non-symlink directory")
	}
	return nil
}

func readAgentFile(path string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maximum {
		return nil, fmt.Errorf("Caddy agent state file %q is unsafe or too large", filepath.Base(path))
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(contents)) > maximum {
		return nil, fmt.Errorf("read bounded Caddy agent state file %q", filepath.Base(path))
	}
	return contents, nil
}

func publishAgentFile(path string, contents []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".agent-state-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func promoteAgentFile(source, target string) error {
	if _, err := readAgentFile(source, 1<<20); err != nil {
		return err
	}
	if err := os.Rename(source, target); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(target))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (c *Caddy) Close() error {
	if transport, ok := c.client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
	return nil
}

var _ Controller = (*Caddy)(nil)
