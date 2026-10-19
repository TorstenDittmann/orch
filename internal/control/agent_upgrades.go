package control

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/buildinfo"
)

const (
	agentUpgradeInterval = 15 * time.Second
	// agentUpgradeTimeout covers the agent's download, its state backup and
	// orch node upgrade waiting for the new agent to reconnect.
	agentUpgradeTimeout = 15 * time.Minute
	agentUpgradeRetry   = time.Hour
	binaryChunkBytes    = 1 << 20
	maxAgentBinaryBytes = 512 << 20
)

// releaseDownloadURL is where agents of another architecture get their
// binary. It is a variable so tests can serve releases locally.
var releaseDownloadURL = "https://github.com/TorstenDittmann/orch/releases/download"

// agentUpgrades rolls the controller's own build out to agents that can
// upgrade themselves, one node at a time: the next node starts only after the
// previous one reconnected with the new build, failed, or timed out. A node
// that failed is retried an hour later; orch node upgrade has already rolled
// it back to its previous binary.
type agentUpgrades struct {
	sessions *sessionManager
	dataDir  string
	now      func() time.Time
	version  string
	buildID  string
	client   *http.Client
	kicks    chan struct{}

	// active, retryAt and binaries belong to the run loop.
	active   *agentUpgradeAttempt
	retryAt  map[string]time.Time
	binaries map[string]agentBinary

	mu      sync.Mutex
	byHash  map[string]agentBinary
	upgrade map[string]string
}

type agentUpgradeAttempt struct {
	nodeID   string
	session  *agentSession
	deadline time.Time
}

type agentBinary struct {
	path   string
	sha256 string
	size   uint64
}

func newAgentUpgrades(sessions *sessionManager, dataDir string, now func() time.Time) *agentUpgrades {
	return &agentUpgrades{
		sessions: sessions, dataDir: dataDir, now: now, version: buildinfo.Version, buildID: buildinfo.BuildID,
		client: &http.Client{Timeout: 10 * time.Minute}, kicks: make(chan struct{}, 1),
		retryAt: make(map[string]time.Time), binaries: make(map[string]agentBinary),
		byHash: make(map[string]agentBinary), upgrade: make(map[string]string),
	}
}

func (u *agentUpgrades) run(ctx context.Context) {
	ticker := time.NewTicker(agentUpgradeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			u.step(ctx)
		case <-u.kicks:
			u.step(ctx)
		}
	}
}

// kick runs a step soon, so an agent that just connected is upgraded, or
// completes an upgrade, without waiting for the next interval.
func (u *agentUpgrades) kick() {
	if u == nil {
		return
	}
	select {
	case u.kicks <- struct{}{}:
	default:
	}
}

// step finishes the active attempt when its outcome is known, then starts the
// next one when no attempt is active.
func (u *agentUpgrades) step(ctx context.Context) {
	now := u.now()
	if attempt := u.active; attempt != nil {
		current := u.sessions.session(attempt.nodeID)
		version, buildID, _, _ := current.agent()
		switch {
		case current != nil && version == u.version && buildID == u.buildID:
			slog.Info("agent upgraded", "node", attempt.nodeID, "version", version)
			u.finish(attempt.nodeID, "")
		case current != nil && current != attempt.session:
			u.fail(attempt.nodeID, fmt.Sprintf("failed: agent reconnected on %s", version), now)
		case now.After(attempt.deadline):
			u.fail(attempt.nodeID, "failed: timed out", now)
		default:
			return
		}
	}
	for _, session := range u.sessions.connected() {
		version, buildID, architecture, selfUpgrade := session.agent()
		if version == u.version && buildID == u.buildID {
			// An upgrade that was counted as failed can still finish later.
			u.mu.Lock()
			delete(u.upgrade, session.nodeID)
			u.mu.Unlock()
			continue
		}
		if !selfUpgrade || compareVersions(version, u.version) >= 0 || now.Before(u.retryAt[session.nodeID]) {
			continue
		}
		binary, err := u.binary(ctx, architecture)
		if err != nil {
			u.fail(session.nodeID, "failed: "+err.Error(), now)
			continue
		}
		command := &orchv1alpha1.ConnectResponse{Message: &orchv1alpha1.ConnectResponse_Upgrade{Upgrade: &orchv1alpha1.UpgradeCommand{
			Version: u.version, BuildId: u.buildID, Sha256: binary.sha256, SizeBytes: binary.size,
		}}}
		if tryEnqueueSession(session, command) != nil {
			continue
		}
		slog.Info("upgrading agent", "node", session.nodeID, "from", version, "to", u.version)
		u.active = &agentUpgradeAttempt{nodeID: session.nodeID, session: session, deadline: now.Add(agentUpgradeTimeout)}
		u.mu.Lock()
		u.upgrade[session.nodeID] = "upgrading to " + u.version
		u.mu.Unlock()
		return
	}
}

func (u *agentUpgrades) finish(nodeID, state string) {
	u.active = nil
	u.mu.Lock()
	defer u.mu.Unlock()
	if state == "" {
		delete(u.upgrade, nodeID)
	} else {
		u.upgrade[nodeID] = state
	}
}

func (u *agentUpgrades) fail(nodeID, state string, now time.Time) {
	slog.Warn("agent upgrade did not complete; retrying later", "node", nodeID, "state", state, "retry_after", agentUpgradeRetry)
	u.retryAt[nodeID] = now.Add(agentUpgradeRetry)
	if u.active != nil && u.active.nodeID == nodeID {
		u.finish(nodeID, state)
		return
	}
	u.mu.Lock()
	u.upgrade[nodeID] = state
	u.mu.Unlock()
}

// state reports a node's upgrade in progress or its last failure.
func (u *agentUpgrades) state(nodeID string) string {
	if u == nil {
		return ""
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.upgrade[nodeID]
}

// binary returns this build for an architecture: the controller's own
// executable, or the matching release asset, downloaded once into the data
// directory and checked against the release's SHA256SUMS.
func (u *agentUpgrades) binary(ctx context.Context, architecture string) (agentBinary, error) {
	if binary, ok := u.binaries[architecture]; ok {
		return binary, nil
	}
	var path string
	if architecture == runtime.GOARCH && runtime.GOOS == "linux" {
		executable, err := os.Executable()
		if err != nil {
			return agentBinary{}, fmt.Errorf("locate controller binary: %w", err)
		}
		path = executable
	} else {
		if strings.Contains(u.version, "-") {
			return agentBinary{}, fmt.Errorf("no %s binary for development build %s", architecture, u.version)
		}
		downloaded, err := u.download(ctx, architecture)
		if err != nil {
			return agentBinary{}, err
		}
		path = downloaded
	}
	digest, size, err := hashBinary(path)
	if err != nil {
		return agentBinary{}, err
	}
	binary := agentBinary{path: path, sha256: digest, size: size}
	u.binaries[architecture] = binary
	u.mu.Lock()
	u.byHash[digest] = binary
	u.mu.Unlock()
	return binary, nil
}

func (u *agentUpgrades) download(ctx context.Context, architecture string) (string, error) {
	switch architecture {
	case "amd64", "arm64":
	default:
		return "", fmt.Errorf("unsupported agent architecture %q", architecture)
	}
	asset := "orch-linux-" + architecture
	directory := filepath.Join(u.dataDir, "agent-binaries", u.version)
	path := filepath.Join(directory, asset)
	base := releaseDownloadURL + "/v" + u.version + "/"
	sums, err := u.fetch(ctx, base+"SHA256SUMS", 1<<20)
	if err != nil {
		return "", err
	}
	defer sums.Close()
	expected := ""
	scanner := bufio.NewScanner(sums)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset {
			expected = fields[0]
		}
	}
	if expected == "" {
		return "", fmt.Errorf("release v%s has no checksum for %s", u.version, asset)
	}
	if digest, _, err := hashBinary(path); err == nil && digest == expected {
		return path, nil
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	body, err := u.fetch(ctx, base+asset, maxAgentBinaryBytes)
	if err != nil {
		return "", err
	}
	defer body.Close()
	staged, err := os.CreateTemp(directory, asset+".*")
	if err != nil {
		return "", err
	}
	defer os.Remove(staged.Name())
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(staged, hash), body)
	if closeErr := staged.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return "", fmt.Errorf("download %s: %w", asset, copyErr)
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return "", fmt.Errorf("checksum mismatch for %s", asset)
	}
	if err := os.Rename(staged.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

func (u *agentUpgrades) fetch(ctx context.Context, url string, limit int64) (io.ReadCloser, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := u.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("download %s: %s", url, response.Status)
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(response.Body, limit), response.Body}, nil
}

func hashBinary(path string) (string, uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), uint64(size), nil
}

// serve streams a binary the controller offered in an UpgradeCommand.
func (u *agentUpgrades) serve(sha string, send func(*orchv1alpha1.DownloadBinaryResponse) error) error {
	if u == nil {
		return connect.NewError(connect.CodeNotFound, errors.New("no agent binary is offered"))
	}
	u.mu.Lock()
	binary, ok := u.byHash[sha]
	u.mu.Unlock()
	if !ok {
		return connect.NewError(connect.CodeNotFound, errors.New("no agent binary is offered with that SHA-256"))
	}
	file, err := os.Open(binary.path)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, errors.New("agent binary is unavailable"))
	}
	defer file.Close()
	buffer := make([]byte, binaryChunkBytes)
	for {
		n, err := file.Read(buffer)
		if n > 0 {
			if sendErr := send(&orchv1alpha1.DownloadBinaryResponse{Data: buffer[:n]}); sendErr != nil {
				return sendErr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return connect.NewError(connect.CodeUnavailable, errors.New("read agent binary"))
		}
	}
}

func (s *agentService) DownloadBinary(ctx context.Context, request *connect.Request[orchv1alpha1.DownloadBinaryRequest], stream *connect.ServerStream[orchv1alpha1.DownloadBinaryResponse]) error {
	if _, err := requireAgentCredential(ctx); err != nil {
		return err
	}
	if err := rejectUnknownMessage(request.Msg); err != nil {
		return err
	}
	return s.upgrades.serve(request.Msg.GetSha256(), stream.Send)
}

// compareVersions orders release versions such as 0.2.0 and 0.2.0-rc.1.
// Versions that do not parse compare as equal, so an agent or controller with
// an unknown version is never upgraded or downgraded.
func compareVersions(a, b string) int {
	coreA, preA, okA := splitVersion(a)
	coreB, preB, okB := splitVersion(b)
	if !okA || !okB {
		return 0
	}
	for i := range coreA {
		if coreA[i] != coreB[i] {
			if coreA[i] < coreB[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case preA == preB:
		return 0
	case preA == "":
		return 1
	case preB == "":
		return -1
	case preA < preB:
		return -1
	default:
		return 1
	}
}

func splitVersion(version string) ([3]int, string, bool) {
	var core [3]int
	main, pre, _ := strings.Cut(strings.TrimPrefix(version, "v"), "-")
	parts := strings.Split(main, ".")
	if len(parts) != 3 {
		return core, "", false
	}
	for i, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return core, "", false
		}
		core[i] = value
	}
	return core, pre, true
}
