package config

type pklDocument struct {
	Schema   string                `pkl:"schema"`
	Name     string                `pkl:"name"`
	Services map[string]pklService `pkl:"services"`
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
			Env:         cloneMap(input.Env),
			SecretEnv:   cloneMap(input.SecretEnv),
			SecretFiles: secretFiles,
			Constraints: cloneMap(input.Constraints),
			Mounts:      mounts,
		}
	}
	for name, input := range doc.Volumes {
		manifest.Volumes[name] = Volume{Driver: input.Driver}
	}
	return manifest
}
