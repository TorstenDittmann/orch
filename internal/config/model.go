package config

import (
	"fmt"
	"sort"
)

const (
	SchemaVersion           = "v1alpha1"
	DefaultCPU              = "100m"
	DefaultMemory           = "128Mi"
	DefaultRestart          = "always"
	DefaultHealthDeadline   = "2m"
	DefaultJobStartDeadline = "1m"
	DefaultJobTimeout       = "15m"
	DefaultAutoscaleCPU     = 70
)

// Manifest is the resolved, validated application specification. All defaults
// are applied before a Manifest is returned to callers.
type Manifest struct {
	Schema   string             `json:"schema"`
	Name     string             `json:"name"`
	Services map[string]Service `json:"services"`
	CronJobs map[string]CronJob `json:"cron_jobs,omitempty"`
	Volumes  map[string]Volume  `json:"volumes,omitempty"`
	// Notifications is omitted when empty so existing manifests keep their
	// canonical digest.
	Notifications []Notification `json:"notifications,omitempty"`
	Deploy        Deployment     `json:"deploy"`
}

// PrivilegedServices returns the sorted names of services that set
// privileged.
func (m *Manifest) PrivilegedServices() []string {
	var names []string
	for name, service := range m.Services {
		if service.Privileged {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
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
	UDP         *UDP              `json:"udp,omitempty"`
	DependsOn   []string          `json:"depends_on,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	SecretEnv   map[string]string `json:"secret_env,omitempty"`
	SecretFiles []SecretFile      `json:"secret_files,omitempty"`
	Constraints map[string]string `json:"constraints,omitempty"`
	Mounts      []Mount           `json:"mounts,omitempty"`
	// StopGrace is empty when the service keeps the default grace period, so
	// manifests that never set it keep their canonical digest.
	StopGrace string `json:"stop_grace_period,omitempty"`
	// Privileged replicas run without the default confinement. It is omitted
	// when false so existing manifests keep their canonical digest.
	Privileged bool `json:"privileged,omitempty"`
	// Autoscale lets the controller choose the replica count between Min
	// and Max. Replicas then equals Min, the count a first deploy starts with.
	Autoscale *Autoscale `json:"autoscale,omitempty"`
	// Database is set on the service orch runs for a `databases` entry.
	Database *Database `json:"database,omitempty"`
}

// Autoscale keeps a service's average CPU use near CPUPercent of its
// requested CPU by adding and removing replicas.
type Autoscale struct {
	Min        int `json:"min"`
	Max        int `json:"max"`
	CPUPercent int `json:"cpu_percent"`
}

// MaxReplicas is the most replicas the service can run: Max when it
// autoscales, otherwise Replicas.
func (s Service) MaxReplicas() int {
	if s.Autoscale != nil {
		return s.Autoscale.Max
	}
	return s.Replicas
}

type Resources struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

type HTTP struct {
	Port       int      `json:"port"`
	Public     bool     `json:"public,omitempty"`
	Domains    []string `json:"domains,omitempty"`
	HealthPath string   `json:"health_path,omitempty"`
	HTTPS      bool     `json:"https"`
	NodeLocal  bool     `json:"node_local,omitempty"`
}

type TCP struct {
	Port       int  `json:"port"`
	PublicPort *int `json:"public_port,omitempty"`
	NodePort   *int `json:"node_port,omitempty"`
}

type UDP struct {
	Port     int `json:"port"`
	NodePort int `json:"node_port"`
}

// NodePorts returns the node-local ports the service publishes as
// "tcp/PORT" and "udp/PORT" keys.
func (s Service) NodePorts() []string {
	var ports []string
	if s.TCP != nil && s.TCP.NodePort != nil {
		ports = append(ports, fmt.Sprintf("tcp/%d", *s.TCP.NodePort))
	}
	if s.UDP != nil {
		ports = append(ports, fmt.Sprintf("udp/%d", s.UDP.NodePort))
	}
	return ports
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
	Constraints   map[string]string `json:"constraints,omitempty"`
	StartDeadline string            `json:"start_deadline"`
	Timeout       string            `json:"timeout"`
	Mounts        []Mount           `json:"mounts,omitempty"`
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
	Driver string        `json:"driver"`
	Backup *VolumeBackup `json:"backup,omitempty"`
}

// VolumeBackup archives a local volume on its node on a UTC cron schedule.
type VolumeBackup struct {
	Schedule string   `json:"schedule"`
	Keep     int      `json:"keep"`
	Before   []string `json:"before,omitempty"`
}

type PreDeployHook struct {
	Service string   `json:"service"`
	Command []string `json:"command"`
	Args    []string `json:"args,omitempty"`
	Timeout string   `json:"timeout"`
}

type Deployment struct {
	PreDeploy      *PreDeployHook `json:"pre_deploy,omitempty"`
	MaxSurge       int            `json:"max_surge"`
	MaxUnavailable int            `json:"max_unavailable"`
	HealthDeadline string         `json:"health_deadline"`
	AutoRollback   bool           `json:"auto_rollback"`
	// AutoUpdate lets the controller redeploy when an image tag moves.
	AutoUpdate bool `json:"auto_update,omitempty"`
}
