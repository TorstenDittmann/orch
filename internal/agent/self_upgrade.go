package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/gen/orch/v1alpha1/orchv1alpha1connect"
	"github.com/TorstenDittmann/orch/internal/buildinfo"
)

const (
	// managedBinary is where orch node install puts orch. Only an agent
	// running from it upgrades itself, through orch node upgrade.
	managedBinary = "/opt/orch/bin/orch"
	// upgradeStagingDir holds the downloaded candidate below the node
	// installation's root-owned state directory.
	upgradeStagingDir   = "/var/lib/orch-install/upgrades/staged"
	maxUpgradeBinary    = 512 << 20
	upgradeUnit         = "orch-agent-upgrade"
	upgradeDownloadTime = 10 * time.Minute
)

// startNodeUpgrade runs orch node upgrade with the candidate in a transient
// systemd unit, so it outlives this agent when the upgrade stops it. It is a
// variable so tests can replace it.
var startNodeUpgrade = func(ctx context.Context, candidate string) error {
	output, err := exec.CommandContext(ctx, "systemd-run", "--unit", upgradeUnit, "--collect", "--quiet",
		"--description", "orch agent upgrade", candidate, "node", "upgrade", "--binary", candidate).CombinedOutput()
	if err != nil {
		return fmt.Errorf("start %s: %v: %s", upgradeUnit, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// selfUpgradeAvailable reports whether this agent is a managed node install
// that can run orch node upgrade on itself. It is a variable so tests can
// replace it.
var selfUpgradeAvailable = func() bool {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return false
	}
	executable, err := os.Executable()
	if err != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(executable); err != nil || resolved != managedBinary {
		return false
	}
	_, err = exec.LookPath("systemd-run")
	return err == nil
}

func validUpgradeCommand(command *orchv1alpha1.UpgradeCommand) bool {
	if command == nil || len(command.ProtoReflect().GetUnknown()) != 0 || command.GetVersion() == "" ||
		len(command.GetVersion()) > 128 || len(command.GetBuildId()) > 128 ||
		command.GetSizeBytes() == 0 || command.GetSizeBytes() > maxUpgradeBinary {
		return false
	}
	decoded, err := hex.DecodeString(command.GetSha256())
	return err == nil && len(decoded) == sha256.Size && command.GetSha256() == strings.ToLower(command.GetSha256())
}

// handleUpgrade starts one upgrade in the background. Commands for the
// running build, or while an upgrade is already starting, are ignored.
func (r *sessionRunner) handleUpgrade(ctx context.Context, command *orchv1alpha1.UpgradeCommand) error {
	if !validUpgradeCommand(command) {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid upgrade command"))
	}
	if !r.selfUpgrade || (command.GetVersion() == buildinfo.Version && command.GetBuildId() == buildinfo.BuildID) ||
		!r.upgrading.CompareAndSwap(false, true) {
		return nil
	}
	go func() {
		defer r.upgrading.Store(false)
		if err := r.upgrade(ctx, command); err != nil {
			log.Printf("agent: upgrade to %s: %v", command.GetVersion(), err)
		}
	}()
	return nil
}

func (r *sessionRunner) upgrade(ctx context.Context, command *orchv1alpha1.UpgradeCommand) error {
	log.Printf("agent: upgrading to %s (%s)", command.GetVersion(), command.GetBuildId())
	downloadContext, cancel := context.WithTimeout(ctx, upgradeDownloadTime)
	defer cancel()
	transport := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		DialContext:       (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, TLSClientConfig: r.agentTLSConfig(), TLSHandshakeTimeout: 5 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := orchv1alpha1connect.NewAgentServiceClient(&http.Client{Transport: transport}, r.credentials.Config.Server,
		connect.WithReadMaxBytes(4<<20))
	stream, err := client.DownloadBinary(downloadContext, connect.NewRequest(&orchv1alpha1.DownloadBinaryRequest{Sha256: command.GetSha256()}))
	if err != nil {
		return fmt.Errorf("download binary: %w", err)
	}
	defer stream.Close()
	candidate, err := stageBinary(r.upgradeStagingDir, command.GetSha256(), command.GetSizeBytes(), func(write func([]byte) error) error {
		for stream.Receive() {
			if err := write(stream.Msg().GetData()); err != nil {
				return err
			}
		}
		return stream.Err()
	})
	if err != nil {
		return err
	}
	return startNodeUpgrade(ctx, candidate)
}

// stageBinary writes a fresh copy of the candidate into directory, replacing
// any earlier one, and checks its size and SHA-256 before naming it.
func stageBinary(directory, digest string, size uint64, receive func(write func([]byte) error) error) (string, error) {
	if err := os.RemoveAll(directory); err != nil {
		return "", err
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(directory, "orch-"+digest)
	file, err := os.OpenFile(path+".partial", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		return "", err
	}
	defer os.Remove(path + ".partial")
	hash := sha256.New()
	written := uint64(0)
	err = receive(func(data []byte) error {
		written += uint64(len(data))
		if written > size {
			return errors.New("binary is larger than announced")
		}
		if _, err := file.Write(data); err != nil {
			return err
		}
		_, err := hash.Write(data)
		return err
	})
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("download binary: %w", err)
	}
	if written != size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return "", errors.New("downloaded binary does not match its announced size and SHA-256")
	}
	if err := os.Rename(path+".partial", path); err != nil {
		return "", err
	}
	return path, nil
}
