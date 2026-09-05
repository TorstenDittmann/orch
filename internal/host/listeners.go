package host

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	agentctl "change.codes/torsten/better-swarm/internal/agent"
	"change.codes/torsten/better-swarm/internal/network"
)

var listenerPID = regexp.MustCompile(`pid=([0-9]+),`)

// Inspect existing process/link identity only; do not initialize a runtime or
// WireGuard manager from the read-only diagnostic path.
func ownedListener(port, line string) bool {
	if err := rootOwned(AgentDir, true); err != nil {
		return false
	}
	if port == "51820" {
		path := filepath.Join(AgentDir, agentctl.ConfigFilename)
		if err := rootOwned(path, false); err != nil {
			return false
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		var config agentctl.Config
		if json.Unmarshal(data, &config) != nil || config.ClusterID == "" || config.NodeID == "" {
			return false
		}
		name := network.WireGuardInterfaceName(config.ClusterID, config.NodeID)
		alias, err := os.ReadFile("/sys/class/net/" + name + "/ifalias")
		if err != nil {
			return false
		}
		sum := sha256.Sum256([]byte(config.ClusterID + "\x00" + config.NodeID))
		return strings.TrimSpace(string(alias)) == "orch-managed-wireguard-"+hex.EncodeToString(sum[:])
	}
	if port != "80" && port != "443" {
		return false
	}
	if err := trustedInstalledFile(Prefix + "/bin/caddy"); err != nil {
		return false
	}
	pid, err := os.ReadFile("/var/lib/orch-ingress/caddy.pid")
	if err != nil {
		return false
	}
	for _, match := range listenerPID.FindAllStringSubmatch(line, -1) {
		if match[1] != strings.TrimSpace(string(pid)) {
			continue
		}
		executable, err := os.Readlink("/proc/" + match[1] + "/exe")
		if err == nil && executable == Prefix+"/bin/caddy" {
			return true
		}
	}
	return false
}
