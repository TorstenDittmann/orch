package runtime

import (
	"encoding/hex"
	"fmt"
	"path"
	"strings"

	"change.codes/torsten/better-swarm/internal/model"
	"github.com/distribution/reference"
)

// ValidateAllocation checks the complete portable runtime contract before an
// assignment can be cached or translated into platform-specific objects.
func ValidateAllocation(allocation model.AllocationDesired) error {
	if !validOpaqueID(allocation.ID) || !validDNSLabel(allocation.Application) || !validDNSLabel(allocation.Service) ||
		allocation.Replica == 0 || allocation.ApplicationGeneration == 0 || allocation.RevisionID == "" || len(allocation.RevisionID) > 128 ||
		!validSHA256Digest(allocation.SpecDigest) || allocation.CPUMillis == 0 || allocation.CPUMillis > uint64(1<<63-1)/100 ||
		allocation.MemoryBytes == 0 || allocation.MemoryBytes > uint64(1<<63-1) || allocation.StartDeadlineSeconds == 0 {
		return fmt.Errorf("runtime allocation identity, resources, or deadline is incomplete or invalid")
	}
	switch allocation.RestartPolicy {
	case "always", "on-failure", "never":
	default:
		return fmt.Errorf("runtime restart policy is invalid")
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
	for _, value := range append(append([]string(nil), allocation.Command...), allocation.Args...) {
		if value == "" || strings.ContainsRune(value, 0) {
			return fmt.Errorf("runtime command and arguments must be nonempty and contain no NUL bytes")
		}
	}
	for key, value := range allocation.Environment {
		if !validEnvironmentName(key) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("runtime environment contains an invalid entry")
		}
	}
	mountTargets := make(map[string]struct{}, len(allocation.Mounts))
	for _, mount := range allocation.Mounts {
		if !validDNSLabel(mount.Name) || !safeMountTarget(mount.Target) {
			return fmt.Errorf("runtime volume mount is invalid")
		}
		if _, exists := mountTargets[mount.Target]; exists {
			return fmt.Errorf("runtime volume mount target is duplicated")
		}
		mountTargets[mount.Target] = struct{}{}
	}
	digest, err := model.AllocationSpecDigest(allocation)
	if err != nil || digest != allocation.SpecDigest {
		return fmt.Errorf("runtime allocation spec digest does not match")
	}
	return nil
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
	for _, protected := range []string{"/proc", "/sys", "/dev", "/run/orch"} {
		if target == protected || strings.HasPrefix(target, protected+"/") {
			return false
		}
	}
	return true
}
