package config

const (
	SchemaVersion           = "v1alpha1"
	DefaultCPU              = "100m"
	DefaultMemory           = "128Mi"
	DefaultRestart          = "always"
	DefaultHealthDeadline   = "2m"
	DefaultJobStartDeadline = "1m"
	DefaultJobTimeout       = "15m"
)

// Manifest is the resolved, validated application specification. All defaults
// are applied before a Manifest is returned to callers.
type Manifest struct {
	Schema   string             `json:"schema"`
	Name     string             `json:"name"`
	Services map[string]Service `json:"services"`
	CronJobs map[string]CronJob `json:"cron_jobs,omitempty"`
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
	TCP         *TCP              `json:"tcp,omitempty"`
	DependsOn   []string          `json:"depends_on,omitempty"`
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

type TCP struct {
	Port       int  `json:"port"`
	PublicPort *int `json:"public_port,omitempty"`
}

type CronJob struct {
	Image         string            `json:"image"`
	Schedule      string            `json:"schedule"`
	Command       []string          `json:"command,omitempty"`
	Args          []string          `json:"args,omitempty"`
	Resources     Resources         `json:"resources"`
	Env           map[string]string `json:"env,omitempty"`
	SecretEnv     map[string]string `json:"secret_env,omitempty"`
	SecretFiles   []SecretFile      `json:"secret_files,omitempty"`
	DependsOn     []string          `json:"depends_on,omitempty"`
	StartDeadline string            `json:"start_deadline"`
	Timeout       string            `json:"timeout"`
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
