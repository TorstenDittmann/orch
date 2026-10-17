package config

import (
	"fmt"
	"net"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	orchschedule "github.com/TorstenDittmann/orch/internal/schedule"
	"github.com/distribution/reference"
	"golang.org/x/net/idna"
)

const (
	maxServices              = 100
	maxCronJobs              = 100
	maxDependencies          = 64
	maxVolumes               = 100
	maxTotalReplicas         = 200
	maxDomainsPerService     = 100
	maxTotalDomains          = 1000
	maxEnvironmentEntries    = 256
	maxConstraintEntries     = 64
	maxMountsPerService      = 64
	maxSecretFilesPerService = 64
	maxCommandEntries        = 256
	maxManifestStringBytes   = 4 << 10
	// wireGuardPort is the default mesh port (model.DefaultWireGuardListenPort).
	wireGuardPort = 51820
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
	} else if len(manifest.Services) > maxServices {
		add("services", fmt.Sprintf("must not define more than %d services", maxServices))
	}
	if len(manifest.Volumes) > maxVolumes {
		add("volumes", fmt.Sprintf("must not define more than %d volumes", maxVolumes))
	}
	if len(manifest.CronJobs) > maxCronJobs {
		add("cron_jobs", fmt.Sprintf("must not define more than %d jobs", maxCronJobs))
	}
	if hook := manifest.Deploy.PreDeploy; hook != nil {
		if _, ok := manifest.Services[hook.Service]; !ok {
			add("deploy.pre_deploy.service", "must name an existing service")
		}
		if len(hook.Command) == 0 || len(hook.Command) > 256 || len(hook.Args) > 256 {
			add("deploy.pre_deploy.command", "must contain 1-256 command entries and at most 256 arguments")
		}
		for _, value := range append(append([]string(nil), hook.Command...), hook.Args...) {
			if value == "" || len(value) > 4096 || strings.ContainsRune(value, 0) {
				add("deploy.pre_deploy.command", "contains an invalid command or argument")
				break
			}
		}
		if d, err := time.ParseDuration(hook.Timeout); err != nil || d < 10*time.Second || d > 24*time.Hour {
			add("deploy.pre_deploy.timeout", "must be between 10s and 24h")
		}
	}
	if manifest.Deploy.MaxSurge < 0 || manifest.Deploy.MaxSurge > 100 {
		add("deploy.max_surge", "must be between 0 and 100")
	}
	if manifest.Deploy.MaxUnavailable < 0 || manifest.Deploy.MaxUnavailable > 100 {
		add("deploy.max_unavailable", "must be between 0 and 100")
	}
	if manifest.Deploy.MaxSurge == 0 && manifest.Deploy.MaxUnavailable == 0 {
		add("deploy", "max_surge and max_unavailable cannot both be zero")
	}
	if duration, err := time.ParseDuration(manifest.Deploy.HealthDeadline); err != nil || duration < 10*time.Second || duration > time.Hour {
		add("deploy.health_deadline", "must be a duration between 10s and 1h")
	}

	domainOwners := make(map[string]string)
	publicPortOwners := make(map[int]string)
	volumeOwners := make(map[string]string)
	totalReplicas := 0
	totalDomains := 0
	publicHTTPService := ""
	var nodePortServices []string
	serviceNames := sortedKeys(manifest.Services)
	for _, serviceName := range serviceNames {
		service := manifest.Services[serviceName]
		base := "services." + serviceName
		validateDNSLabel(base, serviceName, add)
		if strings.TrimSpace(service.Image) == "" {
			add(base+".image", "is required")
		} else if len(service.Image) > 1024 || strings.ContainsRune(service.Image, 0) {
			add(base+".image", "must not exceed 1024 bytes or contain NUL bytes")
		} else if _, err := reference.ParseNormalizedNamed(service.Image); err != nil {
			add(base+".image", "must be a valid OCI image reference")
		}
		if service.Replicas < 1 || service.Replicas > 100 {
			add(base+".replicas", "must be between 1 and 100")
		} else {
			totalReplicas += service.Replicas
		}
		switch service.Restart {
		case "always", "on-failure", "never":
		default:
			add(base+".restart", "must be one of always, on-failure, or never")
		}
		if service.StopGrace != "" {
			if duration, err := time.ParseDuration(service.StopGrace); err != nil || duration < time.Second || duration > 10*time.Minute || duration%time.Second != 0 {
				add(base+".stop_grace_period", "must be a whole-second duration between 1s and 10m")
			}
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
		validateConstraints(base+".constraints", service.Constraints, add)

		if service.HTTP != nil {
			if service.HTTP.Port < 1 || service.HTTP.Port > 65535 {
				add(base+".http.port", "must be between 1 and 65535")
			}
			seenDomains := make(map[string]struct{})
			if len(service.HTTP.Domains) > maxDomainsPerService {
				add(base+".http.domains", fmt.Sprintf("must not contain more than %d domains", maxDomainsPerService))
			}
			totalDomains += len(service.HTTP.Domains)
			for index, domain := range service.HTTP.Domains {
				normalized := strings.TrimSuffix(strings.ToLower(domain), ".")
				field := fmt.Sprintf("%s.http.domains[%d]", base, index)
				if domain != normalized || !validHostname(domain) {
					add(field, "must be a normalized lowercase DNS hostname without a wildcard or trailing dot")
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
			if service.HTTP.Public {
				if publicHTTPService != "" {
					add(base+".http.public", fmt.Sprintf("duplicates the public HTTP listener already declared by service %q", publicHTTPService))
				} else {
					publicHTTPService = serviceName
				}
			}
			if (service.HTTP.Public || len(service.HTTP.Domains) > 0) && service.HTTP.HealthPath == "" {
				add(base+".http.health_path", "is required when public HTTP ingress or domains are configured")
			}
			if service.HTTP.HealthPath != "" && (!strings.HasPrefix(service.HTTP.HealthPath, "/") ||
				len(service.HTTP.HealthPath) > 2048 || strings.ContainsAny(service.HTTP.HealthPath, "\x00\r\n")) {
				add(base+".http.health_path", "must begin with /, fit within 2048 bytes, and contain no NUL or newline bytes")
			}
		}
		if service.HTTP != nil && service.TCP != nil {
			add(base, "http and tcp endpoints cannot both be configured")
		}
		if service.TCP != nil {
			if service.TCP.Port < 1 || service.TCP.Port > 65535 {
				add(base+".tcp.port", "must be between 1 and 65535")
			}
			if service.TCP.PublicPort != nil {
				port := *service.TCP.PublicPort
				if port < 1 || port > 65535 {
					add(base+".tcp.public_port", "must be between 1 and 65535")
				} else if port == 53 || port == 80 || port == 443 {
					add(base+".tcp.public_port", "port is reserved by orch DNS or HTTP ingress")
				} else if owner, exists := publicPortOwners[port]; exists {
					add(base+".tcp.public_port", fmt.Sprintf("is already exposed by service %q", owner))
				} else {
					publicPortOwners[port] = serviceName
				}
			}
			if service.TCP.NodePort != nil {
				port := *service.TCP.NodePort
				if port < 1 || port > 65535 {
					add(base+".tcp.node_port", "must be between 1 and 65535")
				} else if port == 53 || port == 80 || port == 443 {
					add(base+".tcp.node_port", "port is reserved by orch DNS or HTTP ingress")
				}
			}
		}
		if service.UDP != nil {
			if service.UDP.Port < 1 || service.UDP.Port > 65535 {
				add(base+".udp.port", "must be between 1 and 65535")
			}
			if port := service.UDP.NodePort; port < 1 || port > 65535 {
				add(base+".udp.node_port", "must be between 1 and 65535")
			} else if port == 53 || port == wireGuardPort {
				add(base+".udp.node_port", "port is reserved by orch DNS or the WireGuard mesh")
			}
			if service.HTTP == nil && service.TCP == nil {
				add(base+".udp", "requires an http or tcp endpoint on the same service for readiness")
			}
		}
		if len(service.NodePorts()) > 0 {
			nodePortServices = append(nodePortServices, serviceName)
		}

		if len(service.SecretFiles) > maxSecretFilesPerService {
			add(base+".secret_files", fmt.Sprintf("must not contain more than %d entries", maxSecretFilesPerService))
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

		if len(service.Mounts) > maxMountsPerService {
			add(base+".mounts", fmt.Sprintf("must not contain more than %d entries", maxMountsPerService))
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
	// A node-local port is bound by the node itself, so an old and a new
	// replica cannot hold it at the same time on one node.
	if len(nodePortServices) > 0 && manifest.Deploy.MaxSurge > 0 {
		add("deploy.max_surge", fmt.Sprintf("must be 0 because service %q publishes node-local ports; set maxSurge = 0 and maxUnavailable = 1", nodePortServices[0]))
	}
	for _, serviceName := range nodePortServices {
		service := manifest.Services[serviceName]
		if service.TCP == nil || service.TCP.NodePort == nil {
			continue
		}
		if owner, exists := publicPortOwners[*service.TCP.NodePort]; exists {
			add("services."+serviceName+".tcp.node_port", fmt.Sprintf("is already exposed on ingress nodes by service %q", owner))
		}
	}
	validateDependencies(manifest, add)
	if totalReplicas > maxTotalReplicas {
		add("services", fmt.Sprintf("must not request more than %d total replicas", maxTotalReplicas))
	}

	for _, jobName := range sortedKeys(manifest.CronJobs) {
		job := manifest.CronJobs[jobName]
		base := "cron_jobs." + jobName
		validateDNSLabel(base, jobName, add)
		if strings.TrimSpace(job.Image) == "" {
			add(base+".image", "is required")
		} else if len(job.Image) > 1024 || strings.ContainsRune(job.Image, 0) {
			add(base+".image", "must not exceed 1024 bytes or contain NUL bytes")
		} else if _, err := reference.ParseNormalizedNamed(job.Image); err != nil {
			add(base+".image", "must be a valid OCI image reference")
		}
		if _, err := orchschedule.Next(job.Schedule, time.Now()); err != nil {
			add(base+".schedule", err.Error())
		}
		validateStringList(base+".command", job.Command, add)
		validateStringList(base+".args", job.Args, add)
		validateEnvironment(base+".env", job.Env, false, add)
		validateEnvironment(base+".secret_env", job.SecretEnv, true, add)
		for key := range job.Env {
			if _, exists := job.SecretEnv[key]; exists {
				add(base+".secret_env."+key, "duplicates a plain environment variable")
			}
		}
		validateConstraints(base+".constraints", job.Constraints, add)
		if len(job.SecretFiles) > maxSecretFilesPerService {
			add(base+".secret_files", fmt.Sprintf("must not contain more than %d entries", maxSecretFilesPerService))
		}
		secretTargets := make(map[string]struct{})
		for index, secret := range job.SecretFiles {
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
		if !validCPU(job.Resources.CPU) {
			add(base+".resources.cpu", "must be a positive whole CPU count or millicpu value such as 500m")
		}
		if !validMemory(job.Resources.Memory) {
			add(base+".resources.memory", "must be a positive binary unit such as 128Mi or 2Gi")
		}
		if duration, err := time.ParseDuration(job.StartDeadline); err != nil || duration < time.Second || duration > time.Hour {
			add(base+".start_deadline", "must be a duration between 1s and 1h")
		}
		if duration, err := time.ParseDuration(job.Timeout); err != nil || duration < 10*time.Second || duration > 24*time.Hour {
			add(base+".timeout", "must be a duration between 10s and 24h")
		}
		validateCronMounts(base, job, manifest.Volumes, volumeOwners, add)
	}
	if totalDomains > maxTotalDomains {
		add("services", fmt.Sprintf("must not configure more than %d total domains", maxTotalDomains))
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

func validateDependencies(manifest *Manifest, add func(string, string)) {
	for _, serviceName := range sortedKeys(manifest.Services) {
		validateDependencyList("services."+serviceName+".depends_on", serviceName,
			manifest.Services[serviceName].DependsOn, manifest.Services, add)
	}
	for _, jobName := range sortedKeys(manifest.CronJobs) {
		validateDependencyList("cron_jobs."+jobName+".depends_on", "",
			manifest.CronJobs[jobName].DependsOn, manifest.Services, add)
	}

	visiting := make(map[string]bool)
	visited := make(map[string]bool)
	var visit func(string)
	visit = func(serviceName string) {
		if visited[serviceName] {
			return
		}
		visiting[serviceName] = true
		for index, dependency := range manifest.Services[serviceName].DependsOn {
			if _, exists := manifest.Services[dependency]; !exists || dependency == serviceName {
				continue
			}
			if visiting[dependency] {
				add(fmt.Sprintf("services.%s.depends_on[%d]", serviceName, index), "creates a dependency cycle")
				continue
			}
			visit(dependency)
		}
		visiting[serviceName] = false
		visited[serviceName] = true
	}
	for _, serviceName := range sortedKeys(manifest.Services) {
		visit(serviceName)
	}
}

func validateDependencyList(field, owner string, dependencies []string, services map[string]Service, add func(string, string)) {
	if len(dependencies) > maxDependencies {
		add(field, fmt.Sprintf("must not contain more than %d services", maxDependencies))
	}
	seen := make(map[string]struct{}, len(dependencies))
	for index, dependency := range dependencies {
		item := fmt.Sprintf("%s[%d]", field, index)
		validateDNSLabel(item, dependency, add)
		if _, duplicate := seen[dependency]; duplicate {
			add(item, "duplicates another dependency")
		}
		seen[dependency] = struct{}{}
		if dependency == owner && owner != "" {
			add(item, "cannot depend on itself")
		} else if _, exists := services[dependency]; !exists {
			add(item, "does not name a service in this application")
		}
	}
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

// ValidateDomain applies the exact-host ingress rule after parser
// normalization. Wildcards, IP literals, private service names, and
// single-label hosts are intentionally not routable.
func ValidateDomain(domain string) error {
	if !validHostname(domain) {
		return fmt.Errorf("domain must be a normalized multi-label DNS hostname without a wildcard")
	}
	return nil
}

func validateDNSLabel(field, value string, add func(string, string)) {
	if !dnsLabelPattern.MatchString(value) {
		add(field, "must be a lowercase DNS label of at most 63 characters")
	}
}

func validateStringList(field string, values []string, add func(string, string)) {
	if len(values) > maxCommandEntries {
		add(field, fmt.Sprintf("must not contain more than %d entries", maxCommandEntries))
	}
	for index, value := range values {
		if strings.TrimSpace(value) == "" || len(value) > maxManifestStringBytes || strings.ContainsRune(value, 0) {
			add(fmt.Sprintf("%s[%d]", field, index), "must be nonempty, bounded, and contain no NUL bytes")
		}
	}
}

func validateEnvironment(field string, values map[string]string, secrets bool, add func(string, string)) {
	if len(values) > maxEnvironmentEntries {
		add(field, fmt.Sprintf("must not contain more than %d entries", maxEnvironmentEntries))
	}
	for key, value := range values {
		if !envNamePattern.MatchString(key) || len(key) > 256 {
			add(field+"."+key, "has an invalid environment variable name")
		}
		if len(value) > maxManifestStringBytes || strings.ContainsRune(value, 0) {
			add(field+"."+key, "must be bounded and contain no NUL bytes")
		}
		if secrets {
			if !dnsLabelPattern.MatchString(value) {
				add(field+"."+key, "must reference a secret by lowercase DNS label")
			}
		}
	}
}

func validateConstraints(field string, constraints map[string]string, add func(string, string)) {
	if len(constraints) > maxConstraintEntries {
		add(field, fmt.Sprintf("must not contain more than %d entries", maxConstraintEntries))
	}
	for _, key := range sortedKeys(constraints) {
		value := constraints[key]
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" || len(key) > 256 || len(value) > maxManifestStringBytes ||
			strings.ContainsRune(key, 0) || strings.ContainsRune(value, 0) {
			add(field, "keys and values must be nonempty, bounded, and contain no NUL bytes")
			return
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
	if !strings.Contains(value, ".") || net.ParseIP(value) != nil || value == "localhost" ||
		value == "svc.internal" || strings.HasSuffix(value, ".svc.internal") {
		return false
	}
	ascii, err := idna.Lookup.ToASCII(value)
	if err != nil || ascii != value {
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

// validateCronMounts allows a cron job to read local volumes that one service
// in the same application owns. The service's durable volume claim decides the
// node, so every mounted volume must share that owner to share a node.
func validateCronMounts(base string, job CronJob, volumes map[string]Volume, volumeOwners map[string]string, add func(string, string)) {
	if len(job.Mounts) > maxMountsPerService {
		add(base+".mounts", fmt.Sprintf("must not contain more than %d entries", maxMountsPerService))
	}
	targets := make(map[string]struct{})
	for _, secret := range job.SecretFiles {
		targets[secret.Target] = struct{}{}
	}
	owner := ""
	for index, mount := range job.Mounts {
		item := fmt.Sprintf("%s.mounts[%d]", base, index)
		validateDNSLabel(item+".source", mount.Source, add)
		if !mount.ReadOnly {
			add(item+".read_only", "must be true; cron jobs may only read local volumes")
		}
		if _, exists := volumes[mount.Source]; !exists {
			add(item+".source", "does not name a declared volume")
		} else if service, owned := volumeOwners[mount.Source]; !owned {
			add(item+".source", "must be a local volume mounted by a service in this application")
		} else if owner != "" && service != owner {
			add(item+".source", fmt.Sprintf("must belong to service %q like the job's other mounts", owner))
		} else {
			owner = service
		}
		if !safeMountTarget(mount.Target) {
			add(item+".target", "must be a clean absolute path outside protected system paths")
		}
		if _, exists := targets[mount.Target]; exists {
			add(item+".target", "duplicates another mount or secret file target")
		}
		targets[mount.Target] = struct{}{}
	}
}

func safeMountTarget(target string) bool {
	clean := path.Clean(target)
	if target != clean || !strings.HasPrefix(clean, "/") || clean == "/" {
		return false
	}
	for _, protected := range []string{"/proc", "/sys", "/dev", "/run/orch", "/etc/resolv.conf"} {
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
