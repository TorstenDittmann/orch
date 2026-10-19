package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
)

func TestCompareVersions(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.2.0", -1},
		{"0.10.0", "0.9.1", 1},
		{"v1.2.3", "1.2.3", 0},
		{"0.2.0-rc.1", "0.2.0", -1},
		{"0.2.0", "0.2.0-rc.1", 1},
		{"0.2.0-rc.1", "0.2.0-rc.2", -1},
		{"", "0.2.0", 0},
		{"0.2.0", "development", 0},
	} {
		if got := compareVersions(test.a, test.b); got != test.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", test.a, test.b, got, test.want)
		}
	}
}

type upgradeFixture struct {
	t        *testing.T
	now      time.Time
	manager  *sessionManager
	upgrades *agentUpgrades
}

func newUpgradeFixture(t *testing.T) *upgradeFixture {
	f := &upgradeFixture{t: t, now: time.Date(2026, time.October, 19, 3, 0, 0, 0, time.UTC)}
	f.manager = newSessionManager(func() time.Time { return f.now })
	f.upgrades = newAgentUpgrades(f.manager, t.TempDir(), func() time.Time { return f.now })
	f.upgrades.version, f.upgrades.buildID = "0.2.0", "build-two"
	binary := agentBinary{path: "/controller/orch", sha256: "abc", size: 3}
	f.upgrades.binaries[runtime.GOARCH] = binary
	f.upgrades.byHash[binary.sha256] = binary
	return f
}

func (f *upgradeFixture) connect(nodeID, version, architecture string, selfUpgrade bool) *agentSession {
	f.t.Helper()
	if current := f.manager.session(nodeID); current != nil {
		f.manager.unregister(current)
	}
	session, err := f.manager.register(context.Background(), nodeID, "boot-"+version, func() {})
	if err != nil {
		f.t.Fatal(err)
	}
	session.version, session.buildID, session.architecture, session.selfUpgrade = version, "build-"+version, architecture, selfUpgrade
	if version == "0.2.0" {
		session.buildID = "build-two"
	}
	return session
}

func (f *upgradeFixture) command(session *agentSession) *orchv1alpha1.UpgradeCommand {
	select {
	case response := <-session.send:
		return response.GetUpgrade()
	default:
		return nil
	}
}

func TestAgentUpgradesRollOneNodeAtATime(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	first := f.connect("node-a", "0.1.0", runtime.GOARCH, true)
	second := f.connect("node-b", "0.1.0", runtime.GOARCH, true)
	f.connect("node-c", "0.1.0", runtime.GOARCH, false)
	f.connect("node-d", "0.3.0", runtime.GOARCH, true)

	f.upgrades.step(context.Background())
	command := f.command(first)
	if command == nil || command.GetVersion() != "0.2.0" || command.GetBuildId() != "build-two" || command.GetSha256() != "abc" || command.GetSizeBytes() != 3 {
		t.Fatalf("first node command = %v", command)
	}
	if f.command(second) != nil {
		t.Fatal("second node was upgraded while the first was in progress")
	}
	if state := f.upgrades.state("node-a"); state != "upgrading to 0.2.0" {
		t.Fatalf("state = %q", state)
	}
	f.upgrades.step(context.Background())
	if f.command(second) != nil {
		t.Fatal("second node was upgraded before the first reconnected")
	}

	f.connect("node-a", "0.2.0", runtime.GOARCH, true)
	f.upgrades.step(context.Background())
	if state := f.upgrades.state("node-a"); state != "" {
		t.Fatalf("state after success = %q", state)
	}
	if f.command(second) == nil {
		t.Fatal("second node was not upgraded after the first succeeded")
	}
	f.connect("node-b", "0.2.0", runtime.GOARCH, true)
	f.upgrades.step(context.Background())
	f.upgrades.step(context.Background())
	for _, nodeID := range []string{"node-c", "node-d"} {
		if command := f.command(f.manager.session(nodeID)); command != nil {
			t.Fatalf("%s received %v", nodeID, command)
		}
	}
}

func TestAgentUpgradesRetryFailedNodeLater(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	f.connect("node-a", "0.1.0", runtime.GOARCH, true)
	f.upgrades.step(context.Background())
	f.connect("node-a", "0.1.0", runtime.GOARCH, true)
	f.upgrades.step(context.Background())
	if state := f.upgrades.state("node-a"); state != "failed: agent reconnected on 0.1.0" {
		t.Fatalf("state = %q", state)
	}
	if f.command(f.manager.session("node-a")) != nil {
		t.Fatal("failed node was retried at once")
	}

	second := f.connect("node-b", "0.1.0", runtime.GOARCH, true)
	f.upgrades.step(context.Background())
	if f.command(second) == nil {
		t.Fatal("another node was not upgraded after a failure")
	}
	f.now = f.now.Add(agentUpgradeTimeout + time.Second)
	f.manager.touch(second)
	f.manager.touch(f.manager.session("node-a"))
	f.upgrades.step(context.Background())
	if state := f.upgrades.state("node-b"); state != "failed: timed out" {
		t.Fatalf("state = %q", state)
	}

	f.connect("node-b", "0.2.0", runtime.GOARCH, true)
	f.upgrades.step(context.Background())
	if state := f.upgrades.state("node-b"); state != "" {
		t.Fatalf("state after a late upgrade = %q", state)
	}

	f.now = f.now.Add(agentUpgradeRetry)
	second = f.manager.session("node-b")
	f.manager.touch(second)
	f.manager.touch(f.manager.session("node-a"))
	f.upgrades.step(context.Background())
	if f.command(f.manager.session("node-a")) == nil {
		t.Fatal("failed node was not retried after the backoff")
	}
}

func TestAgentUpgradesReportMissingBinary(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	f.upgrades.version = "0.2.0-dev"
	session := f.connect("node-a", "0.1.0", "riscv64", true)
	f.upgrades.step(context.Background())
	if f.command(session) != nil || f.upgrades.active != nil {
		t.Fatal("node without a binary was upgraded")
	}
	if state := f.upgrades.state("node-a"); state != "failed: no riscv64 binary for development build 0.2.0-dev" {
		t.Fatalf("state = %q", state)
	}
}

// otherArchitecture is a supported architecture the test host does not run.
func otherArchitecture() string {
	if runtime.GOARCH == "arm64" {
		return "amd64"
	}
	return "arm64"
}

func TestAgentUpgradesDownloadOtherArchitecture(t *testing.T) {
	other := otherArchitecture()
	asset := "orch-linux-" + other
	binary := []byte(other + " orch")
	digest := sha256.Sum256(binary)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		switch request.URL.Path {
		case "/v0.2.0/SHA256SUMS":
			fmt.Fprintf(response, "%s  %s\n%s  orch-linux-riscv64\n", hex.EncodeToString(digest[:]), asset, "00")
		case "/v0.2.0/" + asset:
			_, _ = response.Write(binary)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	previous := releaseDownloadURL
	releaseDownloadURL = server.URL
	defer func() { releaseDownloadURL = previous }()

	f := newUpgradeFixture(t)
	got, err := f.upgrades.binary(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	if got.sha256 != hex.EncodeToString(digest[:]) || got.size != uint64(len(binary)) {
		t.Fatalf("binary = %#v", got)
	}
	data, err := os.ReadFile(filepath.Join(f.upgrades.dataDir, "agent-binaries", "0.2.0", asset))
	if err != nil || !bytes.Equal(data, binary) {
		t.Fatalf("cached binary = %q, %v", data, err)
	}
	var streamed []byte
	if err := f.upgrades.serve(got.sha256, func(response *orchv1alpha1.DownloadBinaryResponse) error {
		streamed = append(streamed, response.GetData()...)
		return nil
	}); err != nil || !bytes.Equal(streamed, binary) {
		t.Fatalf("served %q, %v", streamed, err)
	}
	if err := f.upgrades.serve("unknown", func(*orchv1alpha1.DownloadBinaryResponse) error { return nil }); err == nil {
		t.Fatal("served a binary that was not offered")
	}

	// A tampered checksum file is refused.
	f2 := newUpgradeFixture(t)
	digest[0] ^= 0xff
	if _, err := f2.upgrades.binary(context.Background(), other); err == nil {
		t.Fatal("binary with a mismatched checksum was accepted")
	}
	if requests == 0 {
		t.Fatal("release server was not used")
	}
}
