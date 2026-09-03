package config

type pklDocument struct {
	Schema   string                `pkl:"schema"`
	Name     string                `pkl:"name"`
	Services map[string]pklService `pkl:"services"`
	CronJobs map[string]pklCronJob `pkl:"cronJobs"`
	Volumes  map[string]pklVolume  `pkl:"volumes"`
	Deploy   pklDeployment         `pkl:"deploy"`
}

type pklService struct {
	Image       string            `pkl:"image"`
	Command     []string          `pkl:"command"`
	Args        []string          `pkl:"args"`
	Replicas    int               `pkl:"replicas"`
	Restart     string            `pkl:"restart"`
	Resources   pklResources      `pkl:"resources"`
	HTTP        *pklHTTP          `pkl:"http"`
	TCP         *pklTCP           `pkl:"tcp"`
	DependsOn   []string          `pkl:"dependsOn"`
	Env         map[string]string `pkl:"env"`
	SecretEnv   map[string]string `pkl:"secretEnv"`
	SecretFiles []pklSecretFile   `pkl:"secretFiles"`
	Constraints map[string]string `pkl:"constraints"`
	Mounts      []pklMount        `pkl:"mounts"`
}

type pklResources struct {
	CPU    string `pkl:"cpu"`
	Memory string `pkl:"memory"`
}

type pklHTTP struct {
	Port       int      `pkl:"port"`
	Domains    []string `pkl:"domains"`
	HealthPath string   `pkl:"healthPath"`
	HTTPS      bool     `pkl:"https"`
}

type pklTCP struct {
	Port       int  `pkl:"port"`
	PublicPort *int `pkl:"publicPort"`
}

type pklCronJob struct {
	Image         string            `pkl:"image"`
	Schedule      string            `pkl:"schedule"`
	Command       []string          `pkl:"command"`
	Args          []string          `pkl:"args"`
	Resources     pklResources      `pkl:"resources"`
	Env           map[string]string `pkl:"env"`
	SecretEnv     map[string]string `pkl:"secretEnv"`
	SecretFiles   []pklSecretFile   `pkl:"secretFiles"`
	DependsOn     []string          `pkl:"dependsOn"`
	StartDeadline string            `pkl:"startDeadline"`
	Timeout       string            `pkl:"timeout"`
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
	Driver string `pkl:"driver"`
}

type pklDeployment struct {
	MaxSurge       int    `pkl:"maxSurge"`
	MaxUnavailable int    `pkl:"maxUnavailable"`
	HealthDeadline string `pkl:"healthDeadline"`
	AutoRollback   bool   `pkl:"autoRollback"`
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
		},
	}
	for name, input := range doc.Services {
		var httpConfig *HTTP
		if input.HTTP != nil {
			httpConfig = &HTTP{
				Port:       input.HTTP.Port,
				Domains:    append([]string(nil), input.HTTP.Domains...),
				HealthPath: input.HTTP.HealthPath,
				HTTPS:      input.HTTP.HTTPS,
			}
		}
		var tcpConfig *TCP
		if input.TCP != nil {
			tcpConfig = &TCP{Port: input.TCP.Port}
			if input.TCP.PublicPort != nil {
				publicPort := *input.TCP.PublicPort
				tcpConfig.PublicPort = &publicPort
			}
		}
		secretFiles := make([]SecretFile, len(input.SecretFiles))
		for index, secret := range input.SecretFiles {
			secretFiles[index] = SecretFile{Source: secret.Source, Target: secret.Target}
		}
		mounts := make([]Mount, len(input.Mounts))
		for index, mount := range input.Mounts {
			mounts[index] = Mount{Source: mount.Source, Target: mount.Target, ReadOnly: mount.ReadOnly}
		}
		manifest.Services[name] = Service{
			Image:       input.Image,
			Command:     append([]string(nil), input.Command...),
			Args:        append([]string(nil), input.Args...),
			Replicas:    input.Replicas,
			Restart:     input.Restart,
			Resources:   Resources{CPU: input.Resources.CPU, Memory: input.Resources.Memory},
			HTTP:        httpConfig,
			TCP:         tcpConfig,
			DependsOn:   append([]string(nil), input.DependsOn...),
			Env:         cloneMap(input.Env),
			SecretEnv:   cloneMap(input.SecretEnv),
			SecretFiles: secretFiles,
			Constraints: cloneMap(input.Constraints),
			Mounts:      mounts,
		}
	}
	for name, input := range doc.CronJobs {
		secretFiles := make([]SecretFile, len(input.SecretFiles))
		for index, secret := range input.SecretFiles {
			secretFiles[index] = SecretFile{Source: secret.Source, Target: secret.Target}
		}
		manifest.CronJobs[name] = CronJob{
			Image: input.Image, Schedule: input.Schedule,
			Command: append([]string(nil), input.Command...), Args: append([]string(nil), input.Args...),
			Resources: Resources{CPU: input.Resources.CPU, Memory: input.Resources.Memory},
			Env:       cloneMap(input.Env), SecretEnv: cloneMap(input.SecretEnv), SecretFiles: secretFiles,
			DependsOn:     append([]string(nil), input.DependsOn...),
			StartDeadline: input.StartDeadline, Timeout: input.Timeout,
		}
	}
	for name, input := range doc.Volumes {
		manifest.Volumes[name] = Volume{Driver: input.Driver}
	}
	return manifest
}
