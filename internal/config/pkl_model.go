package config

import applepkl "github.com/apple/pkl-go/pkl"

type pklDocument struct {
	Schema        string                 `pkl:"schema"`
	Name          string                 `pkl:"name"`
	Services      map[string]pklService  `pkl:"services"`
	CronJobs      map[string]pklCronJob  `pkl:"cronJobs"`
	Volumes       map[string]pklVolume   `pkl:"volumes"`
	Databases     map[string]pklDatabase `pkl:"databases"`
	Notifications []pklNotification      `pkl:"notifications"`
	Deploy        pklDeployment          `pkl:"deploy"`
}

type pklNotification struct {
	Kind string   `pkl:"kind"`
	URL  string   `pkl:"url"`
	On   []string `pkl:"on"`
}

type pklDatabase struct {
	Engine      string             `pkl:"engine"`
	Version     string             `pkl:"version"`
	Image       *string            `pkl:"image"`
	Resources   pklResources       `pkl:"resources"`
	Constraints map[string]string  `pkl:"constraints"`
	Backup      *pklDatabaseBackup `pkl:"backup"`
}

type pklDatabaseBackup struct {
	Schedule string `pkl:"schedule"`
	Keep     int    `pkl:"keep"`
}

type pklService struct {
	Image       string            `pkl:"image"`
	Command     []string          `pkl:"command"`
	Args        []string          `pkl:"args"`
	Replicas    int               `pkl:"replicas"`
	Restart     string            `pkl:"restart"`
	Resources   pklResources      `pkl:"resources"`
	HTTP        any               `pkl:"http"`
	TCP         *pklTCP           `pkl:"tcp"`
	UDP         *pklUDP           `pkl:"udp"`
	DependsOn   []string          `pkl:"dependsOn"`
	Env         map[string]string `pkl:"env"`
	SecretEnv   map[string]string `pkl:"secretEnv"`
	Connect     map[string]string `pkl:"connect"`
	SecretFiles []pklSecretFile   `pkl:"secretFiles"`
	Constraints map[string]string `pkl:"constraints"`
	Mounts      []pklMount        `pkl:"mounts"`
	StopGrace   *string           `pkl:"stopGracePeriod"`
	Privileged  bool              `pkl:"privileged"`
	Autoscale   *pklAutoscale     `pkl:"autoscale"`
}

type pklAutoscale struct {
	Min        int `pkl:"min"`
	Max        int `pkl:"max"`
	CPUPercent int `pkl:"cpuPercent"`
}

type pklResources struct {
	CPU    string `pkl:"cpu"`
	Memory string `pkl:"memory"`
}

type pklHTTP struct {
	Port       int               `pkl:"port"`
	Public     bool              `pkl:"public"`
	Domains    []string          `pkl:"domains"`
	HealthPath string            `pkl:"healthPath"`
	HTTPS      bool              `pkl:"https"`
	NodeLocal  bool              `pkl:"nodeLocal"`
	BasicAuth  *pklBasicAuth     `pkl:"basicAuth"`
	Redirects  map[string]string `pkl:"redirects"`
}

// The schema types http as Http | Listing<Http>, so pklService.HTTP is an
// any and its entries decode through this mapping into *pklHTTP.
func init() { applepkl.RegisterMapping("orch.Application#Http", pklHTTP{}) }

// pklHTTPEntries returns the entries of a service's http value, first the one
// readiness checks.
func pklHTTPEntries(value any) []*pklHTTP {
	switch value := value.(type) {
	case *pklHTTP:
		return []*pklHTTP{value}
	case []any:
		entries := make([]*pklHTTP, 0, len(value))
		for _, entry := range value {
			if entry, ok := entry.(*pklHTTP); ok {
				entries = append(entries, entry)
			}
		}
		return entries
	}
	return nil
}

type pklBasicAuth struct {
	User           string `pkl:"user"`
	PasswordSecret string `pkl:"passwordSecret"`
}

type pklTCP struct {
	Port       int  `pkl:"port"`
	PublicPort *int `pkl:"publicPort"`
	NodePort   *int `pkl:"nodePort"`
}

type pklUDP struct {
	Port     int `pkl:"port"`
	NodePort int `pkl:"nodePort"`
}

type pklCronJob struct {
	Image         string            `pkl:"image"`
	Schedule      string            `pkl:"schedule"`
	Command       []string          `pkl:"command"`
	Args          []string          `pkl:"args"`
	Resources     pklResources      `pkl:"resources"`
	Env           map[string]string `pkl:"env"`
	SecretEnv     map[string]string `pkl:"secretEnv"`
	Connect       map[string]string `pkl:"connect"`
	SecretFiles   []pklSecretFile   `pkl:"secretFiles"`
	DependsOn     []string          `pkl:"dependsOn"`
	Constraints   map[string]string `pkl:"constraints"`
	StartDeadline string            `pkl:"startDeadline"`
	Timeout       string            `pkl:"timeout"`
	Mounts        []pklMount        `pkl:"mounts"`
}

type pklSecretFile struct {
	Source string `pkl:"source"`
	Target string `pkl:"target"`
}

type pklMount struct {
	Source   string `pkl:"source"`
	Target   string `pkl:"target"`
	ReadOnly bool   `pkl:"readOnly"`
}

type pklVolume struct {
	Driver string           `pkl:"driver"`
	Backup *pklVolumeBackup `pkl:"backup"`
}

type pklVolumeBackup struct {
	Schedule string   `pkl:"schedule"`
	Keep     int      `pkl:"keep"`
	Before   []string `pkl:"before"`
}

type pklPreDeployHook struct {
	Service string   `pkl:"service"`
	Command []string `pkl:"command"`
	Args    []string `pkl:"args"`
	Timeout string   `pkl:"timeout"`
}

type pklDeployment struct {
	PreDeploy      *pklPreDeployHook `pkl:"preDeploy"`
	MaxSurge       int               `pkl:"maxSurge"`
	MaxUnavailable int               `pkl:"maxUnavailable"`
	HealthDeadline string            `pkl:"healthDeadline"`
	AutoRollback   bool              `pkl:"autoRollback"`
	AutoUpdate     bool              `pkl:"autoUpdate"`
}

func resolvePkl(doc pklDocument) *Manifest {
	manifest := &Manifest{
		Schema:   doc.Schema,
		Name:     doc.Name,
		Services: make(map[string]Service, len(doc.Services)),
		CronJobs: make(map[string]CronJob, len(doc.CronJobs)),
		Volumes:  make(map[string]Volume, len(doc.Volumes)),
		Deploy: Deployment{
			MaxSurge:       doc.Deploy.MaxSurge,
			MaxUnavailable: doc.Deploy.MaxUnavailable,
			HealthDeadline: doc.Deploy.HealthDeadline,
			AutoRollback:   doc.Deploy.AutoRollback,
			AutoUpdate:     doc.Deploy.AutoUpdate,
		},
	}
	if hook := doc.Deploy.PreDeploy; hook != nil {
		manifest.Deploy.PreDeploy = &PreDeployHook{Service: hook.Service, Command: hook.Command, Args: hook.Args, Timeout: hook.Timeout}
	}
	for name, input := range doc.Services {
		var httpConfig *HTTP
		if entries := pklHTTPEntries(input.HTTP); len(entries) != 0 {
			first := entries[0]
			httpConfig = &HTTP{
				Port:       first.Port,
				Public:     first.Public,
				Domains:    append([]string(nil), first.Domains...),
				HealthPath: first.HealthPath,
				HTTPS:      first.HTTPS,
				NodeLocal:  first.NodeLocal,
				Redirects:  cloneMap(first.Redirects),
			}
			if auth := first.BasicAuth; auth != nil {
				httpConfig.BasicAuth = &BasicAuth{User: auth.User, PasswordSecret: auth.PasswordSecret}
			}
			// Later entries share the first one's policy, which the schema
			// checks, so they only add domains served on their own port.
			for _, entry := range entries[1:] {
				if httpConfig.DomainPorts == nil {
					httpConfig.DomainPorts = make(map[string]int)
				}
				for _, domain := range entry.Domains {
					httpConfig.Domains = append(httpConfig.Domains, domain)
					httpConfig.DomainPorts[domain] = entry.Port
				}
				for from, to := range entry.Redirects {
					if httpConfig.Redirects == nil {
						httpConfig.Redirects = make(map[string]string)
					}
					httpConfig.Redirects[from] = to
				}
			}
		}
		var tcpConfig *TCP
		if input.TCP != nil {
			tcpConfig = &TCP{Port: input.TCP.Port}
			if input.TCP.PublicPort != nil {
				publicPort := *input.TCP.PublicPort
				tcpConfig.PublicPort = &publicPort
			}
			if input.TCP.NodePort != nil {
				nodePort := *input.TCP.NodePort
				tcpConfig.NodePort = &nodePort
			}
		}
		var udpConfig *UDP
		if input.UDP != nil {
			udpConfig = &UDP{Port: input.UDP.Port, NodePort: input.UDP.NodePort}
		}
		secretFiles := make([]SecretFile, len(input.SecretFiles))
		for index, secret := range input.SecretFiles {
			secretFiles[index] = SecretFile{Source: secret.Source, Target: secret.Target}
		}
		mounts := make([]Mount, len(input.Mounts))
		for index, mount := range input.Mounts {
			mounts[index] = Mount{Source: mount.Source, Target: mount.Target, ReadOnly: mount.ReadOnly}
		}
		stopGrace := ""
		if input.StopGrace != nil {
			stopGrace = *input.StopGrace
		}
		replicas := input.Replicas
		var autoscale *Autoscale
		if input.Autoscale != nil {
			autoscale = &Autoscale{Min: input.Autoscale.Min, Max: input.Autoscale.Max, CPUPercent: input.Autoscale.CPUPercent}
			// The schema default of 1 means replicas was left unset; any other
			// value conflicts with autoscale and fails validation.
			if replicas == 1 {
				replicas = autoscale.Min
			}
		}
		manifest.Services[name] = Service{
			Image:       input.Image,
			Command:     append([]string(nil), input.Command...),
			Args:        append([]string(nil), input.Args...),
			Replicas:    replicas,
			Restart:     input.Restart,
			Resources:   Resources{CPU: input.Resources.CPU, Memory: input.Resources.Memory},
			HTTP:        httpConfig,
			TCP:         tcpConfig,
			UDP:         udpConfig,
			DependsOn:   append([]string(nil), input.DependsOn...),
			Env:         cloneMap(input.Env),
			SecretEnv:   cloneMap(input.SecretEnv),
			SecretFiles: secretFiles,
			Constraints: cloneMap(input.Constraints),
			Mounts:      mounts,
			StopGrace:   stopGrace,
			Privileged:  input.Privileged,
			Autoscale:   autoscale,
		}
	}
	for name, input := range doc.CronJobs {
		secretFiles := make([]SecretFile, len(input.SecretFiles))
		for index, secret := range input.SecretFiles {
			secretFiles[index] = SecretFile{Source: secret.Source, Target: secret.Target}
		}
		mounts := make([]Mount, len(input.Mounts))
		for index, mount := range input.Mounts {
			mounts[index] = Mount{Source: mount.Source, Target: mount.Target, ReadOnly: mount.ReadOnly}
		}
		manifest.CronJobs[name] = CronJob{
			Image: input.Image, Schedule: input.Schedule,
			Command: append([]string(nil), input.Command...), Args: append([]string(nil), input.Args...),
			Resources: Resources{CPU: input.Resources.CPU, Memory: input.Resources.Memory},
			Env:       cloneMap(input.Env), SecretEnv: cloneMap(input.SecretEnv), SecretFiles: secretFiles,
			DependsOn:     append([]string(nil), input.DependsOn...),
			Constraints:   cloneMap(input.Constraints),
			StartDeadline: input.StartDeadline, Timeout: input.Timeout,
			Mounts: mounts,
		}
	}
	for name, input := range doc.Volumes {
		volume := Volume{Driver: input.Driver}
		if backup := input.Backup; backup != nil {
			volume.Backup = &VolumeBackup{Schedule: backup.Schedule, Keep: backup.Keep, Before: append([]string(nil), backup.Before...)}
		}
		manifest.Volumes[name] = volume
	}
	for _, input := range doc.Notifications {
		on := append([]string(nil), input.On...)
		if len(on) == 0 {
			on = append(on, DefaultNotificationEvents...)
		}
		manifest.Notifications = append(manifest.Notifications, Notification{Kind: input.Kind, URLSecret: input.URL, On: on})
	}
	return manifest
}

func resolvePklDatabases(doc pklDocument) map[string]DatabaseSpec {
	databases := make(map[string]DatabaseSpec, len(doc.Databases))
	for name, input := range doc.Databases {
		spec := DatabaseSpec{
			Engine: input.Engine, Version: input.Version,
			Resources:   Resources{CPU: input.Resources.CPU, Memory: input.Resources.Memory},
			Constraints: cloneMap(input.Constraints),
		}
		if input.Image != nil {
			spec.Image = *input.Image
		}
		if backup := input.Backup; backup != nil {
			spec.Backup = &VolumeBackup{Schedule: backup.Schedule, Keep: backup.Keep}
		}
		databases[name] = spec
	}
	return databases
}

// resolvePklConnections returns each service's and cron job's `connect`,
// keyed by "services.NAME" or "cronJobs.NAME".
func resolvePklConnections(doc pklDocument) map[string]map[string]string {
	connections := map[string]map[string]string{}
	for name, input := range doc.Services {
		if len(input.Connect) != 0 {
			connections["services."+name] = cloneMap(input.Connect)
		}
	}
	for name, input := range doc.CronJobs {
		if len(input.Connect) != 0 {
			connections["cronJobs."+name] = cloneMap(input.Connect)
		}
	}
	return connections
}
