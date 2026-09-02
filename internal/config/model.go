package config

const (
	SchemaVersion         = "v1alpha1"
	DefaultCPU            = "100m"
	DefaultMemory         = "128Mi"
	DefaultRestart        = "always"
	DefaultHealthDeadline = "2m"
)

// Manifest is the resolved, validated application specification. All defaults
// are applied before a Manifest is returned to callers.
type Manifest struct {
	Schema   string             `json:"schema"`
	Name     string             `json:"name"`
	Services map[string]Service `json:"services"`
	Volumes  map[string]Volume  `json:"volumes,omitempty"`
	Deploy   Deployment         `json:"deploy"`
}

type Service struct {
	Image       string            `json:"image"`
	Command     []string          `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Replicas    int               `json:"replicas"`
	Restart     string            `json:"restart"`
	Resources   Resources         `json:"resources"`
	HTTP        *HTTP             `json:"http,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	SecretEnv   map[string]string `json:"secret_env,omitempty"`
	SecretFiles []SecretFile      `json:"secret_files,omitempty"`
	Constraints map[string]string `json:"constraints,omitempty"`
	Mounts      []Mount           `json:"mounts,omitempty"`
}

type Resources struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

type HTTP struct {
	Port       int      `json:"port"`
	Domains    []string `json:"domains,omitempty"`
	HealthPath string   `json:"health_path,omitempty"`
	HTTPS      bool     `json:"https"`
}

type SecretFile struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

type Mount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

type Volume struct {
	Driver string `json:"driver"`
}

type Deployment struct {
	MaxSurge       int    `json:"max_surge"`
	MaxUnavailable int    `json:"max_unavailable"`
	HealthDeadline string `json:"health_deadline"`
	AutoRollback   bool   `json:"auto_rollback"`
}

type document struct {
	Schema   string                     `toml:"schema"`
	Name     string                     `toml:"name"`
	Services map[string]serviceDocument `toml:"services"`
	Volumes  map[string]volumeDocument  `toml:"volumes"`
	Deploy   deploymentDocument         `toml:"deploy"`
}

type serviceDocument struct {
	Image       string            `toml:"image"`
	Command     []string          `toml:"command"`
	Args        []string          `toml:"args"`
	Replicas    *int              `toml:"replicas"`
	Restart     string            `toml:"restart"`
	Resources   resourcesDocument `toml:"resources"`
	HTTP        *httpDocument     `toml:"http"`
	Env         map[string]string `toml:"env"`
	SecretEnv   map[string]string `toml:"secret_env"`
	SecretFiles []SecretFile      `toml:"secret_files"`
	Constraints map[string]string `toml:"constraints"`
	Mounts      []Mount           `toml:"mounts"`
}

type resourcesDocument struct {
	CPU    string `toml:"cpu"`
	Memory string `toml:"memory"`
}

type httpDocument struct {
	Port       int      `toml:"port"`
	Domains    []string `toml:"domains"`
	HealthPath string   `toml:"health_path"`
	HTTPS      *bool    `toml:"https"`
}

type volumeDocument struct {
	Driver string `toml:"driver"`
}

type deploymentDocument struct {
	MaxSurge       *int   `toml:"max_surge"`
	MaxUnavailable *int   `toml:"max_unavailable"`
	HealthDeadline string `toml:"health_deadline"`
	AutoRollback   *bool  `toml:"auto_rollback"`
}
