package runtime

import (
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"

	"change.codes/torsten/better-swarm/internal/config"
	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/schedule"
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
	if err := validateSecretBindings(allocation.Application, domain, allocation.Environment, mountTargets,
		allocation.SecretEnvironment, allocation.SecretFiles, allocation.RegistryCredential); err != nil {
		return err
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
	if allocation.TCP != nil {
		if allocation.HTTP != nil || allocation.TCP.Port == 0 || allocation.TCP.Port > 65535 ||
			allocation.TCP.PublicPort > 65535 || allocation.TCP.PublicPort == 53 || allocation.TCP.PublicPort == 80 ||
			allocation.TCP.PublicPort == 443 || allocation.NetworkGeneration == 0 {
			return fmt.Errorf("runtime TCP intent is invalid")
		}
	}
	if len(allocation.DependsOn) > 64 || !sort.StringsAreSorted(allocation.DependsOn) {
		return fmt.Errorf("runtime dependencies are not in canonical order")
	}
	previousDependency := ""
	for _, dependency := range allocation.DependsOn {
		if !validDNSLabel(dependency) || dependency == allocation.Service || dependency == previousDependency {
			return fmt.Errorf("runtime dependency is invalid or duplicated")
		}
		previousDependency = dependency
		totalBytes += len(dependency)
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

func ValidateCronJob(job model.CronJobDesired) error {
	if !validDNSLabel(job.Application) || !validDNSLabel(job.Job) || job.ApplicationGeneration == 0 ||
		job.RevisionID == "" || len(job.RevisionID) > 128 || !validSHA256Digest(job.SpecDigest) ||
		job.CPUMillis == 0 || job.CPUMillis > uint64(1<<63-1)/100 || job.MemoryBytes == 0 ||
		job.MemoryBytes > uint64(1<<63-1) || job.StartDeadlineSeconds == 0 || job.StartDeadlineSeconds > 3600 ||
		job.TimeoutSeconds < 10 || job.TimeoutSeconds > 24*60*60 || job.NetworkGeneration == 0 {
		return fmt.Errorf("runtime cron job identity, resources, or deadlines are invalid")
	}
	if job.RunID != "" {
		if !validOpaqueID(job.RunID) || job.RunAt == nil || job.RunAt.IsZero() || job.Schedule != "" {
			return fmt.Errorf("one-off run identity is invalid")
		}
	} else {
		if job.RunAt != nil || job.CancelRequested {
			return fmt.Errorf("cron job cannot carry one-off controls")
		}
		if _, err := schedule.Parse(job.Schedule); err != nil {
			return err
		}
	}
	named, err := reference.ParseNormalizedNamed(job.Image)
	if err != nil {
		return err
	}
	if _, ok := named.(reference.Digested); !ok {
		return fmt.Errorf("cron image must be digest-qualified")
	}
	domain := reference.Domain(named)
	if len(job.Command) > maxRuntimeCommandEntries || len(job.Args) > maxRuntimeCommandEntries || len(job.Environment) > maxRuntimeEnvironmentEntries || len(job.DependsOn) > 64 {
		return fmt.Errorf("runtime cron job collections are too large")
	}
	totalBytes := len(job.Image) + len(job.RevisionID) + len(job.Schedule)
	for _, value := range append(append([]string(nil), job.Command...), job.Args...) {
		if value == "" || len(value) > maxRuntimeStringBytes || strings.ContainsRune(value, 0) {
			return fmt.Errorf("runtime cron command is invalid")
		}
		totalBytes += len(value)
	}
	for key, value := range job.Environment {
		if !validEnvironmentName(key) || len(value) > maxRuntimeStringBytes || strings.ContainsRune(value, 0) {
			return fmt.Errorf("runtime cron environment is invalid")
		}
		totalBytes += len(key) + len(value)
	}
	if err := validateSecretBindings(job.Application, domain, job.Environment, nil,
		job.SecretEnvironment, job.SecretFiles, job.RegistryCredential); err != nil {
		return err
	}
	if !sort.StringsAreSorted(job.DependsOn) {
		return fmt.Errorf("runtime cron dependencies are not canonical")
	}
	previous := ""
	for _, dependency := range job.DependsOn {
		if !validDNSLabel(dependency) || dependency == previous {
			return fmt.Errorf("runtime cron dependency is invalid or duplicated")
		}
		previous = dependency
		totalBytes += len(dependency)
	}
	if totalBytes > maxRuntimeAggregateBytes {
		return fmt.Errorf("runtime cron text exceeds the aggregate size limit")
	}
	digest, err := model.CronJobSpecDigest(job)
	if err != nil || digest != job.SpecDigest {
		return fmt.Errorf("runtime cron job spec digest does not match")
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
		StartBeforeStop: rollback.StartBeforeStop, HTTP: rollback.HTTP, TCP: rollback.TCP,
		DependsOn: rollback.DependsOn, SecretEnvironment: rollback.SecretEnvironment, SecretFiles: rollback.SecretFiles,
		RegistryCredential: rollback.RegistryCredential, NetworkGeneration: rollback.NetworkGeneration,
	}
}

func validateSecretBindings(application, imageDomain string, environment map[string]string, mountTargets map[string]struct{},
	secretEnvironment map[string]model.SecretReference, secretFiles []model.SecretFileDesired,
	registry *model.RegistryCredentialReference) error {
	if len(secretEnvironment) > maxRuntimeEnvironmentEntries || len(secretFiles) > maxRuntimeMounts {
		return fmt.Errorf("runtime secret bindings contain too many entries")
	}
	for name, item := range secretEnvironment {
		if !validEnvironmentName(name) || len(name) > 256 || !validDNSLabel(item.Name) || !validOpaqueID(item.VersionID) {
			return fmt.Errorf("runtime secret environment binding is invalid")
		}
		if _, exists := environment[name]; exists {
			return fmt.Errorf("runtime secret environment binding conflicts with a literal environment value")
		}
	}
	secretTargets := make(map[string]struct{}, len(secretFiles))
	for _, item := range secretFiles {
		if !validDNSLabel(item.Name) || !validOpaqueID(item.VersionID) || !safeMountTarget(item.Target) ||
			item.Target == "/run/orch" || strings.HasPrefix(item.Target, "/run/orch/") {
			return fmt.Errorf("runtime secret file binding is invalid")
		}
		if _, exists := secretTargets[item.Target]; exists {
			return fmt.Errorf("runtime secret file target is duplicated")
		}
		if _, exists := mountTargets[item.Target]; exists {
			return fmt.Errorf("runtime secret file target conflicts with a volume mount")
		}
		secretTargets[item.Target] = struct{}{}
	}
	if registry != nil && (registry.Host == "" || len(registry.Host) > 255 || registry.Host != imageDomain || !validOpaqueID(registry.VersionID) ||
		registry.Username == "" || strings.ContainsAny(registry.Username, "\x00\r\n") || len(registry.Username) > 1024) {
		return fmt.Errorf("runtime registry credential reference is invalid")
	}
	_ = application
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
	for _, protected := range []string{"/proc", "/sys", "/dev", "/run/orch", "/etc/resolv.conf"} {
		if target == protected || strings.HasPrefix(target, protected+"/") {
			return false
		}
	}
	return true
}
