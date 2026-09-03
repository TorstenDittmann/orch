package runtime

import (
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"

	"change.codes/torsten/better-swarm/internal/config"
	"change.codes/torsten/better-swarm/internal/model"
	"github.com/distribution/reference"
)

const (
	maxRuntimeCommandEntries     = 256
	maxRuntimeEnvironmentEntries = 256
	maxRuntimeMounts             = 64
	maxRuntimeDomains            = 100
	maxRuntimeStringBytes        = 4 << 10
	maxRuntimeAggregateBytes     = 128 << 10
)

// ValidateAllocation checks the complete portable runtime contract before an
// assignment can be cached or translated into platform-specific objects.
func ValidateAllocation(allocation model.AllocationDesired) error {
	if !validOpaqueID(allocation.ID) || !validDNSLabel(allocation.Application) || !validDNSLabel(allocation.Service) ||
		allocation.Replica == 0 || allocation.ApplicationGeneration == 0 || allocation.RevisionID == "" || len(allocation.RevisionID) > 128 ||
		!validSHA256Digest(allocation.SpecDigest) || allocation.CPUMillis == 0 || allocation.CPUMillis > uint64(1<<63-1)/100 ||
		allocation.MemoryBytes == 0 || allocation.MemoryBytes > uint64(1<<63-1) || allocation.StartDeadlineSeconds == 0 ||
		allocation.StartDeadlineSeconds > 3600 || allocation.MaxSurge > 100 || allocation.MaxUnavailable > 100 ||
		strings.ContainsRune(allocation.RevisionID, 0) {
		return fmt.Errorf("runtime allocation identity, resources, or deadline is incomplete or invalid")
	}
	switch allocation.RestartPolicy {
	case "always", "on-failure", "never":
	default:
		return fmt.Errorf("runtime restart policy is invalid")
	}
	if len(allocation.Image) > 1024 || strings.ContainsRune(allocation.Image, 0) {
		return fmt.Errorf("runtime image reference is too large or contains a NUL byte")
	}
	named, err := reference.ParseNormalizedNamed(allocation.Image)
	if err != nil {
		return err
	}
	if _, ok := named.(reference.Digested); !ok {
		return fmt.Errorf("image must be digest-qualified")
	}
	domain := reference.Domain(named)
	if domain == "localhost" || strings.HasPrefix(domain, "localhost:") {
		return fmt.Errorf("private registry references are unsupported")
	}
	if len(allocation.Command) > maxRuntimeCommandEntries || len(allocation.Args) > maxRuntimeCommandEntries {
		return fmt.Errorf("runtime command or argument list is too large")
	}
	totalBytes := len(allocation.Image) + len(allocation.RevisionID)
	for _, value := range append(append([]string(nil), allocation.Command...), allocation.Args...) {
		if value == "" || len(value) > maxRuntimeStringBytes || strings.ContainsRune(value, 0) {
			return fmt.Errorf("runtime command and arguments must be nonempty, bounded, and contain no NUL bytes")
		}
		totalBytes += len(value)
	}
	if len(allocation.Environment) > maxRuntimeEnvironmentEntries {
		return fmt.Errorf("runtime environment contains too many entries")
	}
	for key, value := range allocation.Environment {
		if !validEnvironmentName(key) || len(key) > 256 || len(value) > maxRuntimeStringBytes || strings.ContainsRune(value, 0) {
			return fmt.Errorf("runtime environment contains an invalid entry")
		}
		totalBytes += len(key) + len(value)
	}
	if len(allocation.Mounts) > maxRuntimeMounts {
		return fmt.Errorf("runtime contains too many volume mounts")
	}
	mountTargets := make(map[string]struct{}, len(allocation.Mounts))
	for _, mount := range allocation.Mounts {
		if !validDNSLabel(mount.Name) || len(mount.Target) > maxRuntimeStringBytes || !safeMountTarget(mount.Target) {
			return fmt.Errorf("runtime volume mount is invalid")
		}
		if _, exists := mountTargets[mount.Target]; exists {
			return fmt.Errorf("runtime volume mount target is duplicated")
		}
		mountTargets[mount.Target] = struct{}{}
		totalBytes += len(mount.Name) + len(mount.Target)
	}
	if allocation.HTTP != nil {
		if allocation.HTTP.Port == 0 || allocation.HTTP.Port > 65535 ||
			(allocation.HTTP.HealthPath != "" && (!strings.HasPrefix(allocation.HTTP.HealthPath, "/") ||
				len(allocation.HTTP.HealthPath) > 2048 || strings.ContainsAny(allocation.HTTP.HealthPath, "\x00\r\n"))) ||
			allocation.NetworkGeneration == 0 {
			return fmt.Errorf("runtime HTTP intent is invalid")
		}
		if len(allocation.HTTP.Domains) > maxRuntimeDomains {
			return fmt.Errorf("runtime HTTP intent contains too many domains")
		}
		if !sort.StringsAreSorted(allocation.HTTP.Domains) {
			return fmt.Errorf("runtime HTTP domains are not in canonical order")
		}
		previous := ""
		for _, domain := range allocation.HTTP.Domains {
			if domain == previous {
				return fmt.Errorf("runtime HTTP domain is duplicated")
			}
			if err := config.ValidateDomain(domain); err != nil {
				return fmt.Errorf("runtime HTTP domain is invalid: %w", err)
			}
			previous = domain
			totalBytes += len(domain)
		}
		totalBytes += len(allocation.HTTP.HealthPath)
	}
	if totalBytes > maxRuntimeAggregateBytes {
		return fmt.Errorf("runtime allocation text exceeds the aggregate size limit")
	}
	digestMatches, err := model.AllocationSpecDigestMatches(allocation)
	if err != nil || !digestMatches {
		return fmt.Errorf("runtime allocation spec digest does not match")
	}
	if allocation.Rollback != nil {
		if allocation.Rollback.ApplicationGeneration >= allocation.ApplicationGeneration ||
			allocation.Rollback.RevisionID == "" {
			return fmt.Errorf("runtime rollback authorization is invalid")
		}
		fallback := rollbackAllocation(allocation)
		if err := ValidateAllocation(fallback); err != nil {
			return fmt.Errorf("runtime rollback authorization is invalid: %w", err)
		}
	}
	return nil
}

func rollbackAllocation(allocation model.AllocationDesired) model.AllocationDesired {
	rollback := allocation.Rollback
	return model.AllocationDesired{
		ID: allocation.ID, Application: allocation.Application, Service: allocation.Service, Replica: allocation.Replica,
		ApplicationGeneration: rollback.ApplicationGeneration, RevisionID: rollback.RevisionID,
		SpecDigest: rollback.SpecDigest, Image: rollback.Image, Command: rollback.Command, Args: rollback.Args,
		Environment: rollback.Environment, RestartPolicy: rollback.RestartPolicy,
		CPUMillis: rollback.CPUMillis, MemoryBytes: rollback.MemoryBytes, Mounts: rollback.Mounts,
		StartDeadlineSeconds: rollback.StartDeadlineSeconds, AutoRollback: rollback.AutoRollback,
		StartBeforeStop: rollback.StartBeforeStop, HTTP: rollback.HTTP, NetworkGeneration: rollback.NetworkGeneration,
	}
}

func validOpaqueID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' && character != '_' {
			return false
		}
	}
	return true
}

func validSHA256Digest(value string) bool {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == 32
}

func validDNSLabel(value string) bool {
	if len(value) == 0 || len(value) > 63 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

func validEnvironmentName(value string) bool {
	if value == "" || !((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z') || value[0] == '_') {
		return false
	}
	for _, character := range value[1:] {
		if (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') &&
			(character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func safeMountTarget(target string) bool {
	if !path.IsAbs(target) || path.Clean(target) != target || target == "/" {
		return false
	}
	for _, protected := range []string{"/proc", "/sys", "/dev", "/run/orch", "/etc/resolv.conf"} {
		if target == protected || strings.HasPrefix(target, protected+"/") {
			return false
		}
	}
	return true
}
