package config

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"
)

var (
	dnsLabelPattern  = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	hostLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	envNamePattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	cpuPattern       = regexp.MustCompile(`^([1-9][0-9]*)(m)?$`)
	memoryPattern    = regexp.MustCompile(`^[1-9][0-9]*(Ki|Mi|Gi|Ti)$`)
)

type Problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

type ValidationError struct {
	Problems []Problem `json:"problems"`
}

func (e *ValidationError) Error() string {
	var builder strings.Builder
	builder.WriteString("manifest is invalid")
	for _, problem := range e.Problems {
		builder.WriteString("\n  ")
		builder.WriteString(problem.Path)
		builder.WriteString(": ")
		builder.WriteString(problem.Message)
	}
	return builder.String()
}

func Validate(manifest *Manifest) error {
	if manifest == nil {
		return &ValidationError{Problems: []Problem{{Path: "manifest", Message: "must not be nil"}}}
	}
	var problems []Problem
	add := func(field, message string) {
		problems = append(problems, Problem{Path: field, Message: message})
	}

	if manifest.Schema != SchemaVersion {
		add("schema", fmt.Sprintf("must be %q", SchemaVersion))
	}
	validateDNSLabel("name", manifest.Name, add)
	if len(manifest.Services) == 0 {
		add("services", "must define at least one service")
	}
	if manifest.Deploy.MaxSurge < 0 {
		add("deploy.max_surge", "must be zero or greater")
	}
	if manifest.Deploy.MaxUnavailable < 0 {
		add("deploy.max_unavailable", "must be zero or greater")
	}
	if manifest.Deploy.MaxSurge == 0 && manifest.Deploy.MaxUnavailable == 0 {
		add("deploy", "max_surge and max_unavailable cannot both be zero")
	}
	if duration, err := time.ParseDuration(manifest.Deploy.HealthDeadline); err != nil || duration < 10*time.Second || duration > time.Hour {
		add("deploy.health_deadline", "must be a duration between 10s and 1h")
	}

	domainOwners := make(map[string]string)
	volumeOwners := make(map[string]string)
	serviceNames := sortedKeys(manifest.Services)
	for _, serviceName := range serviceNames {
		service := manifest.Services[serviceName]
		base := "services." + serviceName
		validateDNSLabel(base, serviceName, add)
		if strings.TrimSpace(service.Image) == "" {
			add(base+".image", "is required")
		} else if _, err := reference.ParseNormalizedNamed(service.Image); err != nil {
			add(base+".image", "must be a valid OCI image reference")
		}
		if service.Replicas < 1 || service.Replicas > 100 {
			add(base+".replicas", "must be between 1 and 100")
		}
		switch service.Restart {
		case "always", "on-failure", "never":
		default:
			add(base+".restart", "must be one of always, on-failure, or never")
		}
		if !validCPU(service.Resources.CPU) {
			add(base+".resources.cpu", "must be a positive whole CPU count or millicpu value such as 500m")
		}
		if !validMemory(service.Resources.Memory) {
			add(base+".resources.memory", "must be a positive binary unit such as 128Mi or 2Gi")
		}
		validateStringList(base+".command", service.Command, add)
		validateStringList(base+".args", service.Args, add)
		validateEnvironment(base+".env", service.Env, false, add)
		validateEnvironment(base+".secret_env", service.SecretEnv, true, add)
		for key := range service.Env {
			if _, exists := service.SecretEnv[key]; exists {
				add(base+".secret_env."+key, "duplicates a plain environment variable")
			}
		}
		for key, value := range service.Constraints {
			if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
				add(base+".constraints", "keys and values must not be empty")
				break
			}
		}

		if service.HTTP != nil {
			if service.HTTP.Port < 1 || service.HTTP.Port > 65535 {
				add(base+".http.port", "must be between 1 and 65535")
			}
			seenDomains := make(map[string]struct{})
			for index, domain := range service.HTTP.Domains {
				normalized := strings.TrimSuffix(strings.ToLower(domain), ".")
				field := fmt.Sprintf("%s.http.domains[%d]", base, index)
				if !validHostname(normalized) {
					add(field, "must be a lowercase DNS hostname without a wildcard")
				}
				if _, exists := seenDomains[normalized]; exists {
					add(field, "duplicates another domain on this service")
				}
				seenDomains[normalized] = struct{}{}
				if owner, exists := domainOwners[normalized]; exists && owner != serviceName {
					add(field, fmt.Sprintf("is already routed by service %q", owner))
				} else {
					domainOwners[normalized] = serviceName
				}
			}
			if len(service.HTTP.Domains) > 0 && service.HTTP.HealthPath == "" {
				add(base+".http.health_path", "is required when domains are configured")
			}
			if service.HTTP.HealthPath != "" && (!strings.HasPrefix(service.HTTP.HealthPath, "/") || strings.ContainsAny(service.HTTP.HealthPath, "\r\n")) {
				add(base+".http.health_path", "must begin with / and contain no newlines")
			}
		}

		secretTargets := make(map[string]struct{})
		for index, secret := range service.SecretFiles {
			item := fmt.Sprintf("%s.secret_files[%d]", base, index)
			validateDNSLabel(item+".source", secret.Source, add)
			if !safeSecretTarget(secret.Target) {
				add(item+".target", "must be a clean absolute path below /run/secrets")
			}
			if _, exists := secretTargets[secret.Target]; exists {
				add(item+".target", "duplicates another secret file target")
			}
			secretTargets[secret.Target] = struct{}{}
		}

		mountTargets := make(map[string]struct{})
		for index, mount := range service.Mounts {
			item := fmt.Sprintf("%s.mounts[%d]", base, index)
			validateDNSLabel(item+".source", mount.Source, add)
			if _, exists := manifest.Volumes[mount.Source]; !exists {
				add(item+".source", "does not name a declared volume")
			}
			if !safeMountTarget(mount.Target) {
				add(item+".target", "must be a clean absolute path outside protected system paths")
			}
			if _, exists := mountTargets[mount.Target]; exists {
				add(item+".target", "duplicates another mount target")
			}
			mountTargets[mount.Target] = struct{}{}
			if owner, exists := volumeOwners[mount.Source]; exists && owner != serviceName {
				add(item+".source", fmt.Sprintf("local volume is already mounted by service %q", owner))
			} else {
				volumeOwners[mount.Source] = serviceName
			}
		}
		if len(service.Mounts) > 0 && service.Replicas != 1 {
			add(base+".replicas", "must be 1 when the service uses local volumes")
		}
	}

	for _, volumeName := range sortedKeys(manifest.Volumes) {
		volume := manifest.Volumes[volumeName]
		base := "volumes." + volumeName
		validateDNSLabel(base, volumeName, add)
		if volume.Driver != "local" {
			add(base+".driver", "only local is supported in v1alpha1")
		}
	}

	if len(problems) == 0 {
		return nil
	}
	sort.SliceStable(problems, func(i, j int) bool {
		if problems[i].Path == problems[j].Path {
			return problems[i].Message < problems[j].Message
		}
		return problems[i].Path < problems[j].Path
	})
	return &ValidationError{Problems: problems}
}

// ValidateApplicationName applies the public DNS-label rule used by manifests.
func ValidateApplicationName(name string) error {
	var problems []Problem
	validateDNSLabel("name", name, func(field, message string) {
		problems = append(problems, Problem{Path: field, Message: message})
	})
	if len(problems) == 0 {
		return nil
	}
	return &ValidationError{Problems: problems}
}

func validateDNSLabel(field, value string, add func(string, string)) {
	if !dnsLabelPattern.MatchString(value) {
		add(field, "must be a lowercase DNS label of at most 63 characters")
	}
}

func validateStringList(field string, values []string, add func(string, string)) {
	for index, value := range values {
		if strings.TrimSpace(value) == "" {
			add(fmt.Sprintf("%s[%d]", field, index), "must not be empty")
		}
	}
}

func validateEnvironment(field string, values map[string]string, secrets bool, add func(string, string)) {
	for key, value := range values {
		if !envNamePattern.MatchString(key) {
			add(field+"."+key, "has an invalid environment variable name")
		}
		if secrets {
			if !dnsLabelPattern.MatchString(value) {
				add(field+"."+key, "must reference a secret by lowercase DNS label")
			}
		}
	}
}

func validCPU(value string) bool {
	match := cpuPattern.FindStringSubmatch(value)
	if match == nil {
		return false
	}
	amount, err := strconv.ParseUint(match[1], 10, 32)
	return err == nil && amount > 0
}

func validMemory(value string) bool {
	if !memoryPattern.MatchString(value) {
		return false
	}
	units := map[string]uint64{"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40}
	for suffix, multiplier := range units {
		if strings.HasSuffix(value, suffix) {
			amount, err := strconv.ParseUint(strings.TrimSuffix(value, suffix), 10, 64)
			return err == nil && amount > 0 && amount <= uint64(1<<63-1)/multiplier
		}
	}
	return false
}

func normalizeCPU(value string) string {
	match := cpuPattern.FindStringSubmatch(value)
	if match == nil || match[2] == "m" {
		return value
	}
	amount, err := strconv.ParseUint(match[1], 10, 32)
	if err != nil {
		return value
	}
	return strconv.FormatUint(amount*1000, 10) + "m"
}

func validHostname(value string) bool {
	if value == "" || len(value) > 253 || strings.Contains(value, "*") || value != strings.ToLower(value) {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if !hostLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

func safeSecretTarget(target string) bool {
	clean := path.Clean(target)
	return target == clean && strings.HasPrefix(clean, "/run/secrets/") && clean != "/run/secrets"
}

func safeMountTarget(target string) bool {
	clean := path.Clean(target)
	if target != clean || !strings.HasPrefix(clean, "/") || clean == "/" {
		return false
	}
	for _, protected := range []string{"/proc", "/sys", "/dev", "/run/orch"} {
		if clean == protected || strings.HasPrefix(clean, protected+"/") {
			return false
		}
	}
	return true
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
